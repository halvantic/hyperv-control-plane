package hyperv

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ResetISCSIInitiator returns this host's iSCSI initiator to a clean slate:
// every session disconnected, every persistent login forgotten, every discovery
// portal removed.
//
// It exists because iSCSI state accumulates and nothing prunes it. The reconcile
// is strictly additive by design — it never disconnects a session or removes a
// portal, because it cannot tell a target the operator retired from one
// something else still depends on. Over a few reconfigurations a host collects
// dead favourite targets it retries once a minute for ever, portals pinned to
// source addresses it no longer has, and disk objects for LUNs it cannot reach.
// All three were observed on one fleet in a single day.
//
// So this is the deliberate counterpart: not a repair of one thing, but a
// decision to start again. The reconcile rebuilds from the declared spec on its
// next pass, and that is what makes "from scratch" safe to offer — everything
// removed here that the spec still asks for comes straight back, by the same
// code path that would have created it in the first place.
//
// It REFUSES while any iSCSI disk is CARRYING something — clustered, or holding
// a partition — and checks that BEFORE changing anything, so a refusal leaves the
// host exactly as it was. The refusal names the disks and where they are mounted:
// "no" without a reason is not an answer an operator can act on.
//
// Online is deliberately NOT the test. It was, and it refused the exact cleanup
// this exists for: on the rig 2026-08-24 all three S2D members refused with "disk
// 1 is online" over a RAW disk left by a torn-down pool — no partitions, no
// filesystem, nothing mounted, nothing to take away from anybody.
func (p *PowerShell) ResetISCSIInitiator(ctx context.Context) (string, error) {
	script := `$ErrorActionPreference = 'Stop'

# 1. REFUSE while anything is CARRYING something. Before a single change is made.
#
# "Online" is not "in use", and treating it as such made this refuse the exact
# cleanup it exists for. On the rig 2026-08-24 all three S2D members refused with
# "disk 1 is online" — a RAW disk, no partitions, no filesystem, nothing mounted,
# left over from a torn-down pool. Disconnecting it takes nothing away from
# anything.
#
# What actually matters is whether something could have the disk OPEN:
#   - clustered: the cluster owns it, full stop
#   - a partition carrying a volume: a filesystem, possibly mounted, possibly
#     with a drive letter or an access path
# A RAW or partitionless disk is neither, however online it is.
$inUse = @()
foreach ($d in @(Get-Disk -ErrorAction SilentlyContinue | Where-Object { $_.BusType -eq 'iSCSI' })) {
  if ([bool]$d.IsClustered) { $inUse += ('disk ' + [int]$d.Number + ' is a clustered disk'); continue }
  if ([bool]$d.IsOffline) { continue }
  # Online. Does it carry anything?
  $parts = @()
  try { $parts = @(Get-Partition -DiskNumber ([int]$d.Number) -ErrorAction SilentlyContinue | Where-Object { $_.Type -ne 'Reserved' }) } catch {}
  if ($parts.Count -eq 0) { continue }
  # Built from char codes rather than written out: this script lives in a Go raw
  # string, where a backtick ends the literal and backslashes invite miscounting.
  # An unassigned DriveLetter comes back as NUL, not as empty.
  $volGuidPrefix = [string]([char]92 + [char]92 + '?' + [char]92)
  $where = @()
  foreach ($pt in $parts) {
    if ([string]$pt.DriveLetter -match '^[A-Za-z]$') { $where += ([string]$pt.DriveLetter + ':') }
    foreach ($ap in @($pt.AccessPaths)) {
      # The \\?\Volume{...} form is how EVERY partition names itself; it is not a
      # mount and listing it would make every disk look occupied.
      if ($ap -and -not ([string]$ap).StartsWith($volGuidPrefix)) { $where += [string]$ap }
    }
  }
  $detail = 'disk ' + [int]$d.Number + ' is online and carries ' + $parts.Count + ' partition(s)'
  if ($where.Count -gt 0) { $detail += ' mounted at ' + (($where | Sort-Object -Unique) -join ', ') }
  $inUse += $detail
}
if ($inUse.Count -gt 0) {
  throw ('refusing to reset the iSCSI initiator: ' + (($inUse | Sort-Object -Unique) -join '; ') +
    '. Those disks are in use, and a reset takes them away from whatever has them open. ' +
    'Take the volumes offline, or remove them from the cluster, first.')
}

# 2. Sessions. Unregister BEFORE disconnecting: a persistent session that is
#    merely disconnected comes back at the next boot.
#
#    The REASONS are kept. Both calls used to be SilentlyContinue inside a bare
#    catch, so a session that refused to go left the job reporting "2 sessions
#    are still connected" and nothing whatsoever about why — HVNEW03,
#    2026-08-24. A teardown that cannot say what stopped it sends an operator to
#    the one place this exists to keep them out of: a PowerShell session on the
#    host.
#
#    Unregister failing is tolerated and recorded rather than fatal: a session
#    that was never persistent has nothing to unregister, and that is not an
#    error worth stopping for. A DISCONNECT that fails is what actually leaves
#    the session behind.
$sessions = @(Get-IscsiSession -ErrorAction SilentlyContinue)
$sessionErr = @()
foreach ($s in $sessions) {
  $t = [string]$s.TargetNodeAddress
  try { Unregister-IscsiSession -SessionIdentifier $s.SessionIdentifier -ErrorAction Stop }
  catch {
    $m = [string]$_.Exception.Message
    if ($m -notmatch 'not persistent|does not exist|not found') { $sessionErr += ($t + ' (unregister): ' + $m.Trim()) }
  }
  try { Disconnect-IscsiTarget -NodeAddress $t -SessionIdentifier $s.SessionIdentifier -Confirm:$false -ErrorAction Stop }
  catch { $sessionErr += ($t + ': ' + ([string]$_.Exception.Message).Trim()) }
}

# 3. Persistent logins — the ones that outlive everything else and make a host
#    retry a deleted target for ever, with the array logging a warning a minute
#    and nothing on the host explaining it. No cmdlet exists, so iscsicli is
#    parsed.
$persist = 0
$persistErr = @()
$listing = @()
try { $listing = @(& iscsicli ListPersistentTargets 2>&1 | ForEach-Object { [string]$_ }) }
catch { $persistErr += ('could not list persistent targets: ' + $_.Exception.Message) }
$cur = @{}
$records = @()
foreach ($line in $listing) {
  $kv = $line -split '\s*:\s*', 2
  if ($kv.Count -ne 2) { continue }
  $k = $kv[0].Trim(); $v = $kv[1].Trim()
  if ($k -eq 'Target Name') {
    if ($cur.ContainsKey('target')) { $records += ,$cur }
    $cur = @{ target = $v }
  }
  elseif ($k -eq 'Initiator Name') { $cur['initiator'] = $v }
  elseif ($k -eq 'Port Number') { $cur['port'] = $v }
  elseif ($k -like 'Address and Socket*') { $cur['addr'] = $v }
}
if ($cur.ContainsKey('target')) { $records += ,$cur }
foreach ($rec in $records) {
  $init = [string]$rec['initiator']
  if (-not $init) { $init = 'ROOT' + [char]92 + 'ISCSIPRT' + [char]92 + '0000_0' }
  $port = [string]$rec['port']
  if (-not $port -or $port -like '*Any*') { $port = '*' }
  $addr = ''
  $sock = '3260'
  $parts = @(([string]$rec['addr']) -split '\s+' | Where-Object { $_ })
  if ($parts.Count -ge 1) { $addr = $parts[0] }
  if ($parts.Count -ge 2) { $sock = $parts[1] }
  if (-not $addr) { continue }
  try {
    & iscsicli RemovePersistentTarget $init ([string]$rec['target']) $port $addr $sock | Out-Null
    if ($LASTEXITCODE -eq 0) { $persist++ } else { $persistErr += ([string]$rec['target'] + ' via ' + $addr) }
  } catch { $persistErr += ([string]$rec['target'] + ' via ' + $addr + ' - ' + $_.Exception.Message) }
}

# 4. Discovery portals LAST: removing them first would leave the sessions above
#    with nothing naming them.
$portals = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue)
$portalErr = @()
foreach ($pt in $portals) {
  $pa = [string]$pt.TargetPortalAddress
  # THE OBJECT IS PIPED. Do not name the port.
  #
  # Remove-IscsiTargetPortal has an InputObject parameter set that takes the
  # portal straight from Get-IscsiTargetPortal, and Microsoft's own example
  # removes a portal without mentioning a port at all. Passing
  # -TargetPortalPortNumber failed with "Type mismatch for parameter" whatever
  # was put in it: [int] first, then [uint16] — which was a guess, and the wrong
  # way round, since Remove declares Int32 while New declares UInt16. The port
  # was never the fixable part.
  #
  # Same lesson as Remove-ClusterGroup -Name in RemoveReplicaBroker: with these
  # CDXML cmdlets, pipe the object rather than reconstructing its key. Observed
  # on all three members of Primary1, 2026-08-25 — every session went and every
  # portal survived, twice.
  # THE SOURCE BINDING IS PART OF WHAT IDENTIFIES A PORTAL.
  #
  # Since discovery portals started being registered with
  # -InitiatorPortalAddress, removing one by target address alone looks for a
  # portal with NO binding, does not find it, and reports "The specified portal
  # was not found" — while the bound portal sits there untouched. All three
  # members of Primary1 reported exactly that on 2026-08-25, having previously
  # reported "Type mismatch" for the same removal.
  $rm = @{ TargetPortalAddress = $pa }
  $ipa = [string]$pt.InitiatorPortalAddress
  if ($ipa -and $ipa -ne '0.0.0.0') { $rm['InitiatorPortalAddress'] = $ipa }
  try { Remove-IscsiTargetPortal @rm -Confirm:$false -ErrorAction Stop }
  catch {
    $first = ([string]$_.Exception.Message).Trim()
    # Piping the object is the fallback: it carries the CIM key itself, so it
    # covers a portal identified by something this does not reconstruct.
    try { $pt | Remove-IscsiTargetPortal -Confirm:$false -ErrorAction Stop }
    catch {
      # What was actually tried, or the next person debugs it blind. A portal
      # bound to a source address and one that is not fail identically
      # otherwise.
      $portalErr += ($pa + $(if ($ipa) { ' (via ' + $ipa + ')' } else { ' (unbound)' }) + ' - ' + $first)
    }
  }
}

# What is ACTUALLY left, read back rather than assumed. A teardown that reports
# what it intended instead of what it achieved is the shape of every bad one.
$leftSessions = @(Get-IscsiSession -ErrorAction SilentlyContinue).Count
$leftPortals = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue).Count
'RESULT=' + (@{
  sessions = $sessions.Count
  persistent = $persist
  portals = $portals.Count
  leftSessions = $leftSessions
  leftPortals = $leftPortals
  persistErrors = $persistErr
  portalErrors = $portalErr
  sessionErrors = $sessionErr
} | ConvertTo-Json -Compress -Depth 3)`

	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("reset iscsi initiator: %w", err)
	}
	var res struct {
		Sessions      int      `json:"sessions"`
		Persistent    int      `json:"persistent"`
		Portals       int      `json:"portals"`
		LeftSessions  int      `json:"leftSessions"`
		LeftPortals   int      `json:"leftPortals"`
		PersistErrors []string `json:"persistErrors"`
		PortalErrors  []string `json:"portalErrors"`
		SessionErrors []string `json:"sessionErrors"`
	}
	if err := json.Unmarshal([]byte(resultJSON(string(out))), &res); err != nil {
		return "", fmt.Errorf("reset iscsi initiator: could not read the result: %w", err)
	}

	// Judged on what is LEFT, not on what was attempted. A reset reporting that it
	// cleared everything while a session or a portal survived is the same defect
	// as a teardown reporting a pool it never removed.
	var stuck []string
	if res.LeftSessions > 0 {
		// With its reason. A count alone tells an operator that something is wrong
		// and nothing about what, which is the state HVNEW03 was reported in.
		why := strconv.Itoa(res.LeftSessions) + " sessions are still connected"
		if len(res.SessionErrors) > 0 {
			why += " (" + strings.Join(res.SessionErrors, "; ") + ")"
		}
		stuck = append(stuck, why)
	}
	if res.LeftPortals > 0 {
		stuck = append(stuck, strconv.Itoa(res.LeftPortals)+" discovery portals could not be removed")
	}
	if len(res.PortalErrors) > 0 {
		stuck = append(stuck, strings.Join(res.PortalErrors, "; "))
	}
	if len(stuck) > 0 {
		return "", fmt.Errorf("the initiator was only PARTLY reset — %s. The reconcile will still rebuild whatever the spec declares, but the leftovers named here will remain",
			strings.Join(stuck, "; "))
	}

	note := fmt.Sprintf("reset the iSCSI initiator: removed %d sessions, %d persistent logins and %d discovery portals. "+
		"The next reconcile re-registers whatever the storage spec still declares",
		res.Sessions, res.Persistent, res.Portals)
	// A persistent login that survived is the leftover that goes on costing
	// something afterwards, so it is named even though the reset otherwise worked.
	if len(res.PersistErrors) > 0 {
		note += "; NOTE: " + strconv.Itoa(len(res.PersistErrors)) +
			" persistent logins could not be removed (" + strings.Join(res.PersistErrors, "; ") +
			"), so this host may keep retrying them"
	}
	return note, nil
}
