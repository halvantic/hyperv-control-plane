package hyperv

import (
	"context"
	"fmt"
	"strings"
)

// csvMountScript renames the mount points of the CSVs this node owns so each
// matches its declared volume name.
//
// Add-ClusterSharedVolume always mounts at C:\ClusterStorage\VolumeN whatever the
// cluster resource is called, so a volume named iSCSI_DS1 lives at Volume1 and
// every path built from the declared name — the cluster's default storage path,
// replica storage, anything an operator types — points at a directory that does
// not exist. There is no cmdlet for it: renaming the directory IS renaming the
// mount point.
//
// Runs on EVERY member, and acts only on the CSVs this node owns. Renaming lives
// with ownership, and the volumes of a cluster are not all owned by one node — on
// the rig HVNEW04 owned one and HVNEW05 the other, so a step that ran only on the
// cluster's former could never have fixed both. Each node minding its own volumes
// also means no two nodes can race for the same directory.
const csvMountScript = `
$ErrorActionPreference = 'Stop'
Import-Module FailoverClusters -ErrorAction SilentlyContinue
$want = %[1]s
$renamed = @()
$notes = @()
# What this node actually saw, reported whether or not anything was done.
#
# Three attempts at this rename produced no output and no error, which is the one
# outcome that cannot be reasoned about: every explanation fitted equally. A step
# that silently skips is a step that cannot be debugged, so each volume records
# why it was passed over.
$seen = @()
foreach ($csv in @(Get-ClusterSharedVolume -ErrorAction SilentlyContinue)) {
  $name = [string]$csv.Name
  # A volume provisioned FROM THE POOL is named "Cluster Virtual Disk (DS1)" —
  # Add-ClusterSharedVolume wraps the virtual disk's name — while the operator
  # declared it as DS1. An adopted LUN carries the declared name directly, because
  # adoption renames the resource. Matching only the raw name therefore worked on
  # array-backed volumes and never on S2D ones, which reported every volume as
  # "not a declared volume" on a cluster whose mounts were already correct.
  $key = $name
  if (-not $want.ContainsKey($key) -and $name -match '\(([^)]+)\)\s*$') { $key = $Matches[1] }
  if (-not $want.ContainsKey($key)) {
    $seen += ($name + ': not a declared volume')
    continue
  }
  $target = [string]$want[$key]
  if (-not $target) { $seen += ($name + ': no name wanted'); continue }

  # SharedVolumeInfo is a COLLECTION, one entry per volume on the disk. Reading a
  # property straight off it relies on member enumeration and yields nothing when
  # the collection is empty — a silent skip that looks identical to "already
  # correct". Take the first entry explicitly.
  $cur = ''
  try {
    $info = @($csv.SharedVolumeInfo)[0]
    if ($info) { $cur = [string]$info.FriendlyVolumeName }
  } catch {}
  if (-not $cur) {
    # Say WHAT was seen instead of only that nothing was. A CSV has no mount path
    # for one ordinary reason -- the resource is not Online, so C:\ClusterStorage
    # holds nothing for it -- and "reported no mount path" is equally true of a
    # volume that is fine and a volume that is offline. Only one of them is a
    # problem, and the difference cost two rounds of guessing.
    #
    # ORDERED BY WHAT ANSWERS THE QUESTION, which it was not. Thirteen cluster
    # resources were listed in full and "The CSV object reports state Offline"
    # arrived as the last clause -- the whole answer, behind everything that was
    # not. The volume's own state leads now and the inventory is summarised.
    $csvState = ''
    $entries = 0
    try { $csvState = [string]$csv.State } catch {}
    try { $entries = @($csv.SharedVolumeInfo).Count } catch {}
    $ent = [string]$entries + ' volume entr' + $(if ($entries -eq 1) { 'y' } else { 'ies' })
    $why = ''
    $saidOffline = $false
    if ($csvState -and $csvState -ne 'Online') {
      $why = ': the volume is ' + $csvState + ' (' + $ent + '), and an offline volume has no mount path - so this is the state to fix rather than the name'
      $saidOffline = $true
    } elseif ($csvState) {
      $why = ': the volume reports ' + $csvState + ' (' + $ent + ')'
    }
    try {
      # Filtered rather than -Name: that parameter binds to a StringCollection
      # and throws on a PSObject. Here it is inside a diagnosis, so the throw
      # would replace the explanation with a cast error — the worst place for it.
      $r = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { [string]$_.Name -eq $name })[0]
      if ($r) {
        $why += '. Its resource is ' + [string]$r.State + ' on ' + [string]$r.OwnerNode
        if (-not $saidOffline -and [string]$r.State -ne 'Online') {
          $why += ' - an offline volume has no mount path, so this is the state to fix rather than the name'
        }
      } else {
        # Say what DOES exist. "No resource of that name" is unfalsifiable on its
        # own -- it cannot distinguish a missing resource from a lookup that does
        # not match the way the resource is actually named, and the CSV
        # enumeration two lines up clearly found something.
        #
        # EXCLUDING what cannot be a volume, never INCLUDING only what should be.
        # Filtering on 'Physical Disk' is what once reported "this cluster has no
        # Physical Disk resources at all" while two CSVs were plainly present --
        # the filter was the thing that was wrong. So anything NOT on this list is
        # named in full whatever its type turns out to be, and an oddly-typed
        # storage resource still shows; the rest is counted rather than listed.
        $notVolume = @('Network Name','Distributed Network Name','IP Address','IPv6 Address',
          'IPv6 Tunnel Address','Virtual Machine','Virtual Machine Configuration',
          'Virtual Machine Cluster WMI','Virtual Machine Replication Broker','User Manager',
          'Storage QoS Policy Manager','Health Service','File Share Witness','Cloud Witness',
          'Task Scheduler','Cluster Pool')
        $cand = @(); $others = 0; $notOnline = @()
        foreach ($rr in @(Get-ClusterResource -ErrorAction SilentlyContinue)) {
          $rn = [string]$rr.Name; $rt = [string]$rr.ResourceType; $rs = [string]$rr.State
          if ($notVolume -contains $rt) {
            $others += 1
            # Named whatever its type: a resource that is not online may be the
            # reason this volume cannot come up, and counting it hides that.
            #
            # Except a VM role, which is Offline whenever the VM is simply turned
            # off. That is an ordinary state and not a cluster fault, so listing
            # it here puts a normal thing in a line that reads as a fault list --
            # the replay of the rig's own data named a stopped LinuxVM alongside a
            # genuinely failed witness, as though the two were alike.
            if ($rs -ne 'Online' -and $rt -notlike 'Virtual Machine*') {
              $notOnline += ('"' + $rn + '" [' + $rt + '] ' + $rs)
            }
            continue
          }
          $cand += ('"' + $rn + '" [' + $rt + '] ' + $rs)
        }
        if ($cand.Count -eq 0 -and $others -eq 0) {
          $why += '. No cluster resource is named ' + $name + ', and this cluster reports no resources at all'
        } elseif ($cand.Count -eq 0) {
          $why += '. No cluster resource is named ' + $name + ', and none of this cluster''s ' + [string]$others + ' resources is a storage one'
        } else {
          $why += '. No cluster resource is named ' + $name + '; the storage resources it does have are ' + ($cand -join ', ')
        }
        if ($notOnline.Count -gt 0) { $why += '. Also not online: ' + ($notOnline -join ', ') }
      }
    } catch {}
    $seen += ($name + ': reported no mount path' + $why + '.')
    continue
  }
  $leaf = Split-Path -Path $cur -Leaf
  if ($leaf -eq $target) { $seen += ($name + ': already at ' + $target); continue }

  # Only the owner renames. The directory is a cluster object and the owning node
  # is the one coordinating it; a non-owner attempting it is asking a node to
  # rename something it does not control.
  $owner = ''
  try { $owner = [string]$csv.OwnerNode.Name } catch {}
  if ($owner -and $owner -notlike ($env:COMPUTERNAME + '*')) {
    $seen += ($name + ': at ' + $leaf + ', owned by ' + $owner + ' which renames it')
    continue
  }

  # Never while something is running from the old path: renaming a mount point
  # with VMs on it takes their storage out from under them.
  $inUse = $false
  try { $inUse = @(Get-VM -ErrorAction SilentlyContinue | Get-VMHardDiskDrive -ErrorAction SilentlyContinue | Where-Object { [string]$_.Path -like ($cur + '*') }).Count -gt 0 } catch {}
  if ($inUse) {
    $notes += ($name + ' is mounted at ' + $leaf + ' and VMs are running from it; it is renamed to ' + $target + ' once nothing is using it')
    continue
  }

  $dest = Join-Path (Split-Path -Path $cur -Parent) $target
  if (Test-Path -LiteralPath $dest) {
    $notes += ($name + ' cannot take the mount point ' + $target + ' because something already exists at ' + $dest)
    continue
  }
  try {
    Rename-Item -LiteralPath $cur -NewName $target -ErrorAction Stop
    $renamed += ($name + ': ' + $leaf + ' -> ' + $target)
  } catch {
    $notes += ($name + ': could not rename ' + $leaf + ' to ' + $target + ' on ' + $env:COMPUTERNAME + ': ' + ([string]$_.Exception.Message).Trim())
  }
}
[pscustomobject]@{ renamed = @($renamed); notes = @($notes); seen = @($seen) } | ConvertTo-Json -Compress -Depth 3
`

// EnsureCSVMountPoints makes each named CSV's mount point match its declared
// volume name, for the CSVs this node owns. want maps CSV name to the directory
// leaf it should have.
//
// Idempotent: a volume already mounted under its own name is skipped, as is one
// owned by another member — that node does its own on its own pass.
func (p *PowerShell) EnsureCSVMountPoints(ctx context.Context, want map[string]string) (Outcome, string, error) {
	if len(want) == 0 {
		return OutcomeUnchanged, "", nil
	}
	raw, err := p.run(ctx, fmt.Sprintf(csvMountScript, psStringMap(want)))
	if err != nil {
		return OutcomeUnchanged, "", fmt.Errorf("ensure CSV mount points: %w", err)
	}
	var res struct {
		Renamed []string `json:"renamed"`
		Notes   []string `json:"notes"`
		Seen    []string `json:"seen"`
	}
	if derr := decodeJSON(raw, &res); derr != nil {
		return OutcomeUnchanged, "", fmt.Errorf("ensure CSV mount points: %w", derr)
	}
	if len(res.Renamed) > 0 {
		return OutcomeUpdated, strings.Join(res.Renamed, "; "), nil
	}
	if len(res.Notes) > 0 {
		return OutcomeUnchanged, strings.Join(res.Notes, "; "), nil
	}
	// Nothing renamed and nothing to report is only trustworthy if every declared
	// volume was actually accounted for. A volume that was wanted and never seen
	// means this node did not find it at all, which is worth saying — silence there
	// is what made three attempts at this indistinguishable from success.
	for name := range want {
		var found bool
		for _, s := range res.Seen {
			// The line is keyed by the CSV's own name, which for a pool-provisioned
			// volume wraps the declared one — "Cluster Virtual Disk (DS1): already at
			// DS1". Match either form, or an S2D cluster reports every volume missing.
			if strings.HasPrefix(s, name+":") || strings.Contains(s, "("+name+"):") {
				found = true
				break
			}
		}
		if !found {
			return OutcomeUnchanged, "this node did not see a Cluster Shared Volume named " + name +
				"; it has: " + strings.Join(res.Seen, "; "), nil
		}
	}
	// Everything wanted was seen and needed nothing. Report the observation anyway
	// when any volume is not yet at its declared name, so "nothing to do" can never
	// again mean "silently skipped".
	for _, s := range res.Seen {
		if strings.Contains(s, ": at ") || strings.Contains(s, "reported no mount path") {
			return OutcomeUnchanged, strings.Join(res.Seen, "; "), nil
		}
	}
	return OutcomeUnchanged, "", nil
}

// psStringMap renders a Go map as a PowerShell hashtable literal. Keys are
// compared with ContainsKey, which is case-insensitive for a hashtable — right
// here, since Failover Clustering does not guarantee the case it reports a
// resource name in.
func psStringMap(m map[string]string) string {
	var b strings.Builder
	b.WriteString("@{")
	first := true
	for k, v := range m {
		if !first {
			b.WriteString("; ")
		}
		first = false
		b.WriteString(psQuote(k))
		b.WriteString(" = ")
		b.WriteString(psQuote(v))
	}
	b.WriteString("}")
	return b.String()
}
