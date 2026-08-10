package hyperv

import (
	"context"
	"fmt"
	"strings"

	"github.com/joshua-fourie/ballast/api/types"
)

// ISCSIAdoption asks for one array-owned LUN to be taken into the cluster.
//
// Name is the CSV's name (or the witness's label). AsWitness adopts the disk as
// a plain clustered disk instead of a CSV — the two are not interchangeable and
// the distinction is load-bearing; see WitnessSpec.Disk.
//
// Wipe permits adopting a LUN that already carries a partition or filesystem.
// Without it such a LUN is refused, because adoption formats it: an array
// presents LUNs to whoever it is told to, and a serial typed one character out,
// or a LUN re-presented from another cluster, looks exactly like a new one right
// up to the point its contents are gone.
type ISCSIAdoption struct {
	Name      string
	Source    types.CSVSourceSpec
	AsWitness bool
	Wipe      bool
	// FileSystem is NTFS or ReFS. ReFS is what Microsoft recommends under CSV for
	// Hyper-V workloads, and is the default when this is empty.
	FileSystem string
}

// adoptScript finds the declared LUN on this node, refuses it if adopting would
// destroy data, and otherwise brings it into the cluster.
//
// Identification is by SERIAL first. A disk number is per-node and changes across
// reboots, and a LUN number is per-target — neither names the same storage on
// every member, which is exactly what a clustered disk has to be. Target+LUN is
// accepted as a fallback for authoring a volume before the array has presented
// it, and the serial is reported back so the spec can be pinned to it.
//
// Every step is verified by re-reading rather than trusted, because a half-
// adopted LUN (partitioned but not clustered, clustered but not a CSV) is worse
// than an unadopted one: the console shows a volume, the cluster does not have it.
const adoptScript = `
$ErrorActionPreference = 'Stop'
$serial  = %[1]s
$target  = %[2]s
$name    = %[3]s
$witness = %[4]s
$wipe    = %[5]s
$fs      = %[6]s

Import-Module FailoverClusters -ErrorAction SilentlyContinue

function Fail($m) { throw $m }

# Ensure-MountPoint makes the volume's path under ClusterStorage match its
# declared name, and reports whether it had to change it.
#
# The mount point is named separately from the cluster resource:
# Add-ClusterSharedVolume always mounts at C:\ClusterStorage\VolumeN whatever the
# resource is called. So a resource named iSCSI_DS1 can sit at Volume1 while the
# cluster's default storage path names iSCSI_DS1 and points at nothing — the
# fault behind VMMS 18172, where every new VM lands somewhere unintended.
#
# Renaming the directory is how a mount point is renamed; there is no cmdlet.
# A failure here is REPORTED, not swallowed. The first version caught everything
# and returned false, so a rename that could not happen was indistinguishable from
# one that was not needed — the volumes stayed at VolumeN, the adoption reported
# "already matches desired state", and nothing anywhere said why.
$script:mountNote = ''
function Ensure-MountPoint($csvObj, $want) {
  if (-not $csvObj -or -not $want) { return $false }
  $cur = ''
  try { $cur = [string]$csvObj.SharedVolumeInfo.FriendlyVolumeName } catch {}
  if (-not $cur) { $script:mountNote = 'the volume did not report a mount path'; return $false }
  $leaf = Split-Path -Path $cur -Leaf
  if ($leaf -eq $want) { return $false }
  # Never while something is running from the old path: renaming a mount point
  # with VMs on it takes their storage out from under them.
  $inUse = $false
  try { $inUse = @(Get-VM -ErrorAction SilentlyContinue | Get-VMHardDiskDrive -ErrorAction SilentlyContinue | Where-Object { [string]$_.Path -like ($cur + '*') }).Count -gt 0 } catch {}
  if ($inUse) {
    $script:mountNote = 'the mount point is still ' + $leaf + ' because VMs are running from it; it is renamed once nothing is using it'
    return $false
  }
  # The directory can only be renamed by the node that OWNS the volume. On a
  # cluster the former runs the adoption, and it does not necessarily own every
  # CSV, so this legitimately has to wait for a pass on the owning node.
  $owner = ''
  try { $owner = [string]$csvObj.OwnerNode.Name } catch {}
  if ($owner -and -not ($owner -like ($env:COMPUTERNAME + '*'))) {
    $script:mountNote = 'the mount point is still ' + $leaf + ' and can only be renamed on its owner ' + $owner
    return $false
  }
  try {
    Rename-Item -LiteralPath $cur -NewName $want -ErrorAction Stop
    return $true
  } catch {
    $script:mountNote = 'could not rename the mount point ' + $leaf + ' to ' + $want + ': ' + ([string]$_.Exception.Message).Trim()
    return $false
  }
}

# ---- locate the LUN -------------------------------------------------------
$disk = $null
if ($serial) {
  $disk = Get-Disk -ErrorAction SilentlyContinue | Where-Object {
    $_.SerialNumber -and ([string]$_.SerialNumber).Trim() -eq $serial } | Select-Object -First 1
}
if (-not $disk -and $target) {
  # Fall back to the target, for a volume authored before the array presented the
  # LUN and its serial could be known. Get-Disk carries neither the target nor the
  # LUN number, so the association comes through the iSCSI session.
  $cands = @()
  foreach ($s in @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object { $_.TargetNodeAddress -eq $target })) {
    foreach ($dev in @($s | Get-Disk -ErrorAction SilentlyContinue)) { $cands += $dev }
  }
  $cands = @($cands | Sort-Object -Property Number -Unique)
  if ($cands.Count -eq 1) {
    $disk = $cands[0]
  } elseif ($cands.Count -gt 1) {
    # Refuse rather than guess. A LUN number would disambiguate, but Windows does
    # not report it against a disk, so picking one here would be picking by
    # enumeration order — which differs per node, and adopting a different LUN on
    # each member is precisely the corruption this whole path exists to avoid.
    $list = @($cands | ForEach-Object { ([string]$_.SerialNumber).Trim() + ' (' + [math]::Round($_.Size/1GB,1) + 'GB)' })
    Fail ('target ' + $target + ' presents ' + $cands.Count + ' disks to this node and the volume does not say which: ' + ($list -join '; ') + '. Set the volume''s serial number to the one you mean — a serial is the only identifier that names the same disk on every member.')
  }
}
if (-not $disk) {
  $seen = @(Get-Disk -ErrorAction SilentlyContinue | Where-Object { $_.BusType -eq 'iSCSI' } | ForEach-Object { ([string]$_.SerialNumber).Trim() + ' (' + [math]::Round($_.Size/1GB,1) + 'GB)' })
  $what = if ($serial) { 'serial ' + $serial } else { 'target ' + $target }
  if ($seen.Count -eq 0) {
    Fail ('no iSCSI disk with ' + $what + ' is presented to this node, and this node can see no iSCSI disks at all. Check the node is logged in to the array and that the array grants this LUN to this initiator.')
  }
  Fail ('no iSCSI disk with ' + $what + ' is presented to this node. It can see: ' + ($seen -join '; ') + '. Grant the LUN to this node''s initiator on the array, or correct the serial.')
}

# ---- refuse to destroy data ----------------------------------------------
# A LUN that already carries a partition or a filesystem is refused unless the
# operator has explicitly asked to wipe it. Adoption formats the disk, and an
# array will happily present a LUN that belongs to something else.
$hasData = $false
$ours = $false
$what = @()
if ($disk.PartitionStyle -ne 'RAW') {
  $parts = @(Get-Partition -DiskNumber $disk.Number -ErrorAction SilentlyContinue | Where-Object { $_.Type -ne 'Reserved' })
  foreach ($p in $parts) {
    $hasData = $true
    $v = Get-Volume -Partition $p -ErrorAction SilentlyContinue
    if ($v -and $v.FileSystem) {
      # A volume carrying THIS volume's name is one Ballast formatted for this
      # very adoption — it labels with the volume name — so a pass that formatted
      # the disk and then failed before the cluster took it leaves exactly this.
      # Refusing it makes the adoption unresumable: every retry finds the contents
      # its own previous attempt wrote and stops. Seen on the rig with iSCSI_DS1
      # and iSCSI_DS2, both refused for holding a volume of their own name.
      #
      # Nothing is destroyed by continuing: an existing filesystem is not
      # reformatted below, so this resumes the adoption rather than redoing it.
      if ([string]$v.FileSystemLabel -eq $name) { $ours = $true }
      $used = ''
      if ($v.Size -gt 0) { $used = ', ' + [math]::Round(($v.Size - $v.SizeRemaining)/1GB,1) + 'GB used of ' + [math]::Round($v.Size/1GB,1) + 'GB' }
      $what += ('a ' + [string]$v.FileSystem + ' volume' + $(if ($v.FileSystemLabel) { ' labelled "' + $v.FileSystemLabel + '"' } else { '' }) + $used)
    } else {
      $what += ('a ' + [string]$p.Type + ' partition')
    }
  }
  if (-not $hasData -and $parts.Count -eq 0) {
    # Initialised but empty. That is not data, and refusing it would strand a
    # LUN that a previous adoption initialised and did not finish.
    $hasData = $false
  }
}
if ($hasData -and -not $wipe -and -not $ours) {
  Fail ('the LUN (serial ' + ([string]$disk.SerialNumber).Trim() + ', ' + [math]::Round($disk.Size/1GB,1) + 'GB) already contains ' + ($what -join ' and ') + '. Adopting it formats it, so Ballast will not do that to a disk with contents. If this is the right LUN and its contents are finished with, use "Wipe and adopt"; otherwise correct the serial or present a different LUN.')
}

# ---- already adopted? ------------------------------------------------------
# Idempotency is judged on the cluster, not on this node's view: another member
# may have adopted it, in which case this node has nothing to do.
# Matched on the disk's own identity, NOT on a DiskNumber parameter.
#
# A Physical Disk resource does not carry the node's disk number, so the match
# never succeeded: the disk was added to the cluster on the first pass, and every
# pass after it failed to notice, tried to add it again, found nothing on offer
# because it was already clustered, and reported that the cluster would not take
# a disk the cluster already had. Available Storage came Online holding both LUNs
# while the console showed two failed adoptions.
#
# DiskIdGuid is the GPT disk GUID, which Get-Disk reports as Guid; DiskUniqueId
# is the page-83 identity. Both are read rather than assumed, because assuming a
# parameter exists is exactly what produced this.
$clusDisk = $null
$dguid = ''
try { $dguid = ([string]$disk.Guid).Trim('{}').ToLowerInvariant() } catch {}
$duid = ''
try { $duid = ([string]$disk.UniqueId).Trim() } catch {}
foreach ($res in @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'Physical Disk' })) {
  foreach ($pp in @($res | Get-ClusterParameter -ErrorAction SilentlyContinue)) {
    $pv = ([string]$pp.Value).Trim()
    if ($pp.Name -eq 'DiskIdGuid' -and $dguid -and $pv.Trim('{}').ToLowerInvariant() -eq $dguid) { $clusDisk = $res; break }
    if ($pp.Name -eq 'DiskUniqueId' -and $duid -and $pv -eq $duid) { $clusDisk = $res; break }
  }
  if ($clusDisk) { break }
}
$csv = Get-ClusterSharedVolume -ErrorAction SilentlyContinue | Where-Object { [string]$_.Name -eq $name } | Select-Object -First 1
if ($csv -and -not $witness) {
  # An adopted volume still has its mount point checked. Returning here without
  # doing so meant the rename only ever ran on the pass that created the CSV — so
  # a volume adopted before Ballast knew to name the mount point kept
  # C:\ClusterStorage\VolumeN for ever, and the declared name never became the
  # path the operator was told to expect.
  $renamed = Ensure-MountPoint $csv $name
  $n = 'already a CSV'
  if ($script:mountNote) { $n = $script:mountNote }
  [pscustomobject]@{ changed = $renamed; serial = ([string]$disk.SerialNumber).Trim(); note = $n } | ConvertTo-Json -Compress
  return
}
if ($clusDisk -and $witness) {
  [pscustomobject]@{ changed = $false; serial = ([string]$disk.SerialNumber).Trim(); note = 'already a clustered disk' } | ConvertTo-Json -Compress
  return
}

# ---- prepare the disk ------------------------------------------------------
# Skipped entirely once the cluster holds the disk: it is prepared already (that
# is why it could be added), and bringing a clustered disk online or repartitioning
# it from a node that may not own it is reaching past the cluster for something it
# is managing.
if (-not $clusDisk) {
if ($disk.IsOffline)  { Set-Disk -Number $disk.Number -IsOffline $false -ErrorAction Stop }
if ($disk.IsReadOnly) { Set-Disk -Number $disk.Number -IsReadOnly $false -ErrorAction Stop }

if ($wipe -and $hasData) { Clear-Disk -Number $disk.Number -RemoveData -RemoveOEM -Confirm:$false -ErrorAction Stop }
$disk = Get-Disk -Number $disk.Number -ErrorAction Stop
if ($disk.PartitionStyle -eq 'RAW') { Initialize-Disk -Number $disk.Number -PartitionStyle GPT -ErrorAction Stop | Out-Null }

$part = @(Get-Partition -DiskNumber $disk.Number -ErrorAction SilentlyContinue | Where-Object { $_.Type -ne 'Reserved' }) | Select-Object -First 1
if (-not $part) {
  $part = New-Partition -DiskNumber $disk.Number -UseMaximumSize -ErrorAction Stop
}
$vol = Get-Volume -Partition $part -ErrorAction SilentlyContinue
if (-not $vol -or -not $vol.FileSystem) {
  # A witness is always NTFS: it is small, owned by one node, and ReFS buys it
  # nothing. Data volumes default to ReFS, which is what Microsoft recommends
  # under CSV for Hyper-V.
  $useFs = $fs
  if ($witness) { $useFs = 'NTFS' }
  Format-Volume -Partition $part -FileSystem $useFs -NewFileSystemLabel $name -Confirm:$false -Force -ErrorAction Stop | Out-Null
}
}

# ---- hand it to the cluster ------------------------------------------------
if (-not $clusDisk) {
  # Add-ClusterDisk takes disks the cluster can see and are not already clustered.
  $before = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'Physical Disk' } | ForEach-Object { [string]$_.Name })
  $avail = @(Get-ClusterAvailableDisk -ErrorAction SilentlyContinue)
  # Matched on UniqueId first, because a ClusterAvailableDisk's Number is NOT this
  # node's disk number — the cluster numbers what it offers independently. Filtering
  # on it discarded the very disks the cluster was offering: two available, neither
  # of them "disk 5", while disk 5 sat there prepared and waiting.
  $mine = @($avail | Where-Object { $_.UniqueId -and [string]$_.UniqueId -eq [string]$disk.UniqueId })
  if ($mine.Count -eq 0) {
    $mine = @($avail | Where-Object { $null -ne $_.Number -and [int]$_.Number -eq [int]$disk.Number })
  }
  if ($mine.Count -eq 0) {
    # Size is a last resort and only when it is unambiguous: two LUNs of the same
    # size would make it a coin toss, and adopting the wrong one is not recoverable.
    $bySize = @($avail | Where-Object { $_.Size -and [uint64]$_.Size -eq [uint64]$disk.Size })
    if ($bySize.Count -eq 1) { $mine = $bySize }
  }
  if ($mine.Count -gt 0) {
    $mine | Add-ClusterDisk -ErrorAction SilentlyContinue | Out-Null
  }
  $after = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'Physical Disk' } | ForEach-Object { [string]$_.Name })
  $new = @($after | Where-Object { $before -notcontains $_ })
  if ($new.Count -eq 0) {
    # Report what is actually the case rather than asserting a cause.
    #
    # "It must be visible to every member" was a guess dressed as a diagnosis: the
    # disk WAS visible to every member, and saying otherwise sent the operator to
    # the array to check something that was already right. Get-ClusterAvailableDisk
    # excludes a disk for several reasons and does not say which, so state the
    # facts that distinguish them and let them be read.
    $why = @()
    if ($avail.Count -eq 0) {
      $why += 'the cluster currently offers no available disks at all'
    } else {
      # Say WHAT it offers, not just how many. "None of them matched" without the
      # candidates is unfalsifiable — it was a filtering bug, and the count alone
      # could not have shown that.
      $shown = @($avail | ForEach-Object { 'number ' + [string]$_.Number + ' / ' + [math]::Round($_.Size/1GB,1) + 'GB / ' + [string]$_.UniqueId })
      $why += ('the cluster offers ' + $avail.Count + ' available disk(s) and none matched this one: ' + ($shown -join ', '))
    }
    # The usual reason a presented LUN is not offered: it is mounted read/write on
    # more than one node at once, which is the state clustering exists to prevent.
    # Windows brings a newly arrived shared LUN online on every node that sees it
    # unless the node's new-disk policy says otherwise.
    $pol = ''
    try { $pol = [string](Get-StorageSetting -ErrorAction SilentlyContinue).NewDiskPolicy } catch {}
    if ($pol -and $pol -ne 'OfflineShared' -and $pol -ne 'OfflineAll') {
      $why += ('this node''s new-disk policy is ' + $pol + ', so a shared LUN is brought online here automatically; a LUN online on more than one node at once is not offered to the cluster')
    }
    $why += ('this node sees it as disk ' + [string]$disk.Number + ', online=' + (-not $disk.IsOffline) + ', partition style ' + [string]$disk.PartitionStyle)
    Fail ('the disk was prepared but the cluster did not take it: ' + ($why -join '; ') + '. Check the same LUN is offline on the other members, or that it is not already held by another cluster.')
  }
  $clusDisk = Get-ClusterResource -Name $new[0] -ErrorAction Stop
}

# Named outside the add, so a disk the cluster already holds is named too. Inside
# it, a resumed adoption kept whatever the cluster called the resource — "Cluster
# Disk 1" — and the operator's volume name appeared nowhere.
if ($name -and [string]$clusDisk.Name -ne $name) {
  try { $clusDisk.Name = $name; $clusDisk = Get-ClusterResource -Name $name -ErrorAction Stop } catch {}
}

if (-not $witness) {
  # Already a CSV under this resource's name is success, not a failure to re-add:
  # Add-ClusterSharedVolume throws for a disk that is already shared, and a
  # resumed adoption would otherwise fail on the step it had already completed.
  $existing = Get-ClusterSharedVolume -ErrorAction SilentlyContinue | Where-Object { [string]$_.Name -eq [string]$clusDisk.Name } | Select-Object -First 1
  if (-not $existing) {
    Add-ClusterSharedVolume -InputObject $clusDisk -ErrorAction Stop | Out-Null
    $existing = Get-ClusterSharedVolume -ErrorAction SilentlyContinue | Where-Object { [string]$_.Name -eq [string]$clusDisk.Name } | Select-Object -First 1
    if (-not $existing) { Fail ('the disk joined the cluster but did not become a Cluster Shared Volume.') }
  }

  Ensure-MountPoint $existing $name | Out-Null
}

[pscustomobject]@{ changed = $true; serial = ([string]$disk.SerialNumber).Trim(); note = $script:mountNote } | ConvertTo-Json -Compress
`

// AdoptISCSIDisk takes an array-presented LUN into the cluster, as a CSV or as
// the witness disk. It reports the disk's serial so a volume authored by
// target/LUN can be pinned to the identifier that works cluster-wide.
//
// Idempotent: a LUN already adopted is a no-op, judged on the cluster's view
// rather than this node's, since another member may have done it.
func (p *PowerShell) AdoptISCSIDisk(ctx context.Context, a ISCSIAdoption) (serial, note string, out Outcome, err error) {
	if strings.TrimSpace(a.Source.SerialNumber) == "" && strings.TrimSpace(a.Source.TargetIQN) == "" {
		return "", "", OutcomeUnchanged, fmt.Errorf("volume %q does not say which LUN it is: give the disk's serial number, or the target IQN it is presented on", a.Name)
	}
	fs := strings.TrimSpace(a.FileSystem)
	if fs == "" {
		fs = "ReFS"
	}
	// Source.LUN is deliberately not used for matching. Windows does not report a
	// LUN number against a disk, so honouring it would mean picking by enumeration
	// order — which differs per node, and adopting a different LUN on each member
	// is the corruption this path exists to prevent. It stays in the schema as
	// operator-facing provenance; the serial is what identifies the disk.
	script := fmt.Sprintf(adoptScript,
		psQuote(strings.TrimSpace(a.Source.SerialNumber)),
		psQuote(strings.TrimSpace(a.Source.TargetIQN)),
		psQuote(a.Name),
		psBool(a.AsWitness),
		psBool(a.Wipe),
		psQuote(fs),
	)
	raw, rerr := p.run(ctx, script)
	if rerr != nil {
		return "", "", OutcomeUnchanged, fmt.Errorf("adopt iSCSI disk %q: %w", a.Name, rerr)
	}
	var res struct {
		Changed bool   `json:"changed"`
		Serial  string `json:"serial"`
		Note    string `json:"note"`
	}
	if derr := decodeJSON(raw, &res); derr != nil {
		return "", "", OutcomeUnchanged, fmt.Errorf("adopt iSCSI disk %q: %w", a.Name, derr)
	}
	if res.Changed {
		return res.Serial, res.Note, OutcomeUpdated, nil
	}
	return res.Serial, res.Note, OutcomeUnchanged, nil
}

// diskWitnessScript points quorum at an already-clustered disk.
//
// It runs AFTER adoption, and only names a disk the cluster already holds.
// Setting quorum to a disk that is not there takes the cluster's quorum with it,
// which is the one failure in this area that cannot be undone from the console —
// so the disk is verified present, owned and online first, and the whole thing is
// refused rather than half-applied.
//
// Set-ClusterQuorum is not idempotent (it tears the resource down and recreates
// it, dropping a vote for a moment), so it runs only when the configured witness
// actually differs.
const diskWitnessScript = `
$ErrorActionPreference = 'Stop'
$serial = %[1]s
Import-Module FailoverClusters -ErrorAction SilentlyContinue

$disk = $null
if ($serial) {
  $disk = Get-Disk -ErrorAction SilentlyContinue | Where-Object {
    $_.SerialNumber -and ([string]$_.SerialNumber).Trim() -eq $serial } | Select-Object -First 1
}
if (-not $disk) { throw ('the witness disk (serial ' + $serial + ') is not visible on this node, so quorum cannot be pointed at it. Adopt the witness LUN first.') }

$res = $null
foreach ($r in @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'Physical Disk' })) {
  $num = ($r | Get-ClusterParameter -Name DiskNumber -ErrorAction SilentlyContinue).Value
  if ($num -ne $null -and [int]$num -eq [int]$disk.Number) { $res = $r; break }
}
if (-not $res) { throw ('the witness disk (serial ' + $serial + ') is visible but is not a clustered disk, so it cannot hold a quorum vote. Adopt it as the witness first.') }

$q = Get-ClusterQuorum -ErrorAction Stop
$curName = ''
if ($q.QuorumResource) { $curName = [string]$q.QuorumResource.Name }
if ($curName -eq [string]$res.Name) {
  [pscustomobject]@{ changed = $false } | ConvertTo-Json -Compress
  return
}
Set-ClusterQuorum -DiskWitness $res.Name -ErrorAction Stop | Out-Null
[pscustomobject]@{ changed = $true } | ConvertTo-Json -Compress
`

func (p *PowerShell) ensureDiskWitness(ctx context.Context, src types.CSVSourceSpec) (Outcome, error) {
	serial := strings.TrimSpace(src.SerialNumber)
	if serial == "" {
		return OutcomeUnchanged, fmt.Errorf("a disk witness must be identified by serial number: a target IQN alone can name several disks, and quorum pointed at the wrong one is not something the console can undo")
	}
	raw, err := p.run(ctx, fmt.Sprintf(diskWitnessScript, psQuote(serial)))
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("set disk witness: %w", err)
	}
	var res struct {
		Changed bool `json:"changed"`
	}
	if derr := decodeJSON(raw, &res); derr != nil {
		return OutcomeUnchanged, fmt.Errorf("set disk witness: %w", derr)
	}
	if res.Changed {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
}
