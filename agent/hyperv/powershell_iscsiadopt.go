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
$what = @()
if ($disk.PartitionStyle -ne 'RAW') {
  $parts = @(Get-Partition -DiskNumber $disk.Number -ErrorAction SilentlyContinue | Where-Object { $_.Type -ne 'Reserved' })
  foreach ($p in $parts) {
    $hasData = $true
    $v = Get-Volume -Partition $p -ErrorAction SilentlyContinue
    if ($v -and $v.FileSystem) {
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
if ($hasData -and -not $wipe) {
  Fail ('the LUN (serial ' + ([string]$disk.SerialNumber).Trim() + ', ' + [math]::Round($disk.Size/1GB,1) + 'GB) already contains ' + ($what -join ' and ') + '. Adopting it formats it, so Ballast will not do that to a disk with contents. If this is the right LUN and its contents are finished with, use "Wipe and adopt"; otherwise correct the serial or present a different LUN.')
}

# ---- already adopted? ------------------------------------------------------
# Idempotency is judged on the cluster, not on this node's view: another member
# may have adopted it, in which case this node has nothing to do.
$clusDisk = $null
foreach ($res in @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'Physical Disk' })) {
  $sig = ($res | Get-ClusterParameter -Name DiskIdGuid -ErrorAction SilentlyContinue).Value
  $num = ($res | Get-ClusterParameter -Name DiskNumber -ErrorAction SilentlyContinue).Value
  if ($num -ne $null -and [int]$num -eq [int]$disk.Number) { $clusDisk = $res; break }
}
$csv = Get-ClusterSharedVolume -ErrorAction SilentlyContinue | Where-Object { [string]$_.Name -eq $name } | Select-Object -First 1
if ($csv -and -not $witness) {
  [pscustomobject]@{ changed = $false; serial = ([string]$disk.SerialNumber).Trim(); note = 'already a CSV' } | ConvertTo-Json -Compress
  return
}
if ($clusDisk -and $witness) {
  [pscustomobject]@{ changed = $false; serial = ([string]$disk.SerialNumber).Trim(); note = 'already a clustered disk' } | ConvertTo-Json -Compress
  return
}

# ---- prepare the disk ------------------------------------------------------
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

# ---- hand it to the cluster ------------------------------------------------
if (-not $clusDisk) {
  # Add-ClusterDisk takes disks the cluster can see and are not already clustered.
  $before = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'Physical Disk' } | ForEach-Object { [string]$_.Name })
  Get-ClusterAvailableDisk -ErrorAction SilentlyContinue | Where-Object { [int]$_.Number -eq [int]$disk.Number } | Add-ClusterDisk -ErrorAction Stop | Out-Null
  $after = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'Physical Disk' } | ForEach-Object { [string]$_.Name })
  $new = @($after | Where-Object { $before -notcontains $_ })
  if ($new.Count -eq 0) {
    Fail ('the disk was prepared but the cluster did not take it. It must be visible to every member — check the array grants this LUN to all of their initiators and that each is logged in.')
  }
  $clusDisk = Get-ClusterResource -Name $new[0] -ErrorAction Stop
  if ($name -and [string]$clusDisk.Name -ne $name) {
    try { $clusDisk.Name = $name } catch {}
  }
}

if (-not $witness) {
  Add-ClusterSharedVolume -InputObject $clusDisk -ErrorAction Stop | Out-Null
  $check = Get-ClusterSharedVolume -ErrorAction SilentlyContinue | Where-Object { [string]$_.Name -eq [string]$clusDisk.Name } | Select-Object -First 1
  if (-not $check) { Fail ('the disk joined the cluster but did not become a Cluster Shared Volume.') }
}

[pscustomobject]@{ changed = $true; serial = ([string]$disk.SerialNumber).Trim(); note = '' } | ConvertTo-Json -Compress
`

// AdoptISCSIDisk takes an array-presented LUN into the cluster, as a CSV or as
// the witness disk. It reports the disk's serial so a volume authored by
// target/LUN can be pinned to the identifier that works cluster-wide.
//
// Idempotent: a LUN already adopted is a no-op, judged on the cluster's view
// rather than this node's, since another member may have done it.
func (p *PowerShell) AdoptISCSIDisk(ctx context.Context, a ISCSIAdoption) (serial string, out Outcome, err error) {
	if strings.TrimSpace(a.Source.SerialNumber) == "" && strings.TrimSpace(a.Source.TargetIQN) == "" {
		return "", OutcomeUnchanged, fmt.Errorf("volume %q does not say which LUN it is: give the disk's serial number, or the target IQN it is presented on", a.Name)
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
		return "", OutcomeUnchanged, fmt.Errorf("adopt iSCSI disk %q: %w", a.Name, rerr)
	}
	var res struct {
		Changed bool   `json:"changed"`
		Serial  string `json:"serial"`
		Note    string `json:"note"`
	}
	if derr := decodeJSON(raw, &res); derr != nil {
		return "", OutcomeUnchanged, fmt.Errorf("adopt iSCSI disk %q: %w", a.Name, derr)
	}
	if res.Changed {
		return res.Serial, OutcomeUpdated, nil
	}
	return res.Serial, OutcomeUnchanged, nil
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
