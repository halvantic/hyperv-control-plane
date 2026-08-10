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
  if (-not $want.ContainsKey($name)) {
    $seen += ($name + ': not a declared volume')
    continue
  }
  $target = [string]$want[$name]
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
    $seen += ($name + ': reported no mount path')
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
			if strings.HasPrefix(s, name+":") {
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
