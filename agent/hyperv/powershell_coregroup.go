package hyperv

import (
	"context"
	"fmt"
	"strings"
)

// coreGroupScript brings a cluster's own resources back online.
//
// The core group first, always. "Cluster Group" holds the cluster name and its IP
// addresses, and while it is down the cluster has no identity on the network:
// storage will not come online and roles fail, each reporting a separate problem.
// Starting anything else first is starting at a symptom — on the rig that meant
// two CSVs, two failed VM configuration resources and three rounds of diagnosis,
// all downstream of a core group nobody had looked at.
//
// Then the STORAGE resources, because they are what the cluster exists to serve
// and they cannot come online while the core group is down, so they are exactly
// what is left stranded behind it.
//
// VMs are deliberately not started. Power is imperative in Ballast and only ever
// reported, never assumed: a VM that was off before the cluster broke should be
// off after it is fixed, and a recovery action that silently powers on workloads
// is not a recovery an operator can trust.
//
// This is coordination through the cluster's own API, not a quorum decision. The
// cluster still owns whether a resource may come online and says so when it may
// not.
const coreGroupScript = `
$ErrorActionPreference = 'Stop'
Import-Module FailoverClusters -ErrorAction SilentlyContinue

$started = @()
$stuck = @()

function Start-BallastResource($res, $label) {
  $state = ''
  try { $state = [string]$res.State } catch {}
  if ($state -eq 'Online') { return }
  try {
    # Through the object, never by re-looking-up the name: a CSV's backing
    # resource is not always found by Get-ClusterResource -Name on this build,
    # and a lookup that quietly finds nothing reports success having done nothing.
    Start-ClusterResource -InputObject $res -ErrorAction Stop | Out-Null
  } catch {
    $script:stuck += ($label + ' (' + $state + '): ' + ([string]$_.Exception.Message).Trim())
    return
  }
  $after = ''
  try { $after = [string](Get-ClusterResource -InputObject $res -ErrorAction SilentlyContinue).State } catch {}
  if ($after -eq 'Online') { $script:started += $label } else { $script:stuck += ($label + ' is still ' + $after) }
}

# ---- the core group --------------------------------------------------------
$core = @(Get-ClusterGroup -ErrorAction SilentlyContinue | Where-Object { [string]$_.Name -eq 'Cluster Group' })[0]
if (-not $core) {
  throw 'this cluster reports no core group ("Cluster Group"), so its name and IP addresses cannot be brought online from here. The cluster service may not be running on this node.'
}
$coreState = [string]$core.State
if ($coreState -ne 'Online') {
  try {
    Start-ClusterGroup -InputObject $core -ErrorAction Stop | Out-Null
    $started += 'the cluster core group'
  } catch {
    throw ('the cluster core group would not come online: ' + ([string]$_.Exception.Message).Trim() + '. Its resources are the cluster name and its IP addresses; an address already in use on the network, or one on a subnet no member holds any more, is the usual reason.')
  }
  $core = @(Get-ClusterGroup -ErrorAction SilentlyContinue | Where-Object { [string]$_.Name -eq 'Cluster Group' })[0]
  $after = [string]$core.State
  if ($after -ne 'Online') {
    # PartialOnline here is worth naming precisely: some of the group came up and
    # some did not, and which is the whole question.
    $down = @()
    foreach ($r in @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { [string]$_.OwnerGroup -eq 'Cluster Group' })) {
      if ([string]$r.State -ne 'Online') { $down += ('"' + [string]$r.Name + '" [' + [string]$r.ResourceType + '] ' + [string]$r.State) }
    }
    $detail = ''
    if ($down.Count -gt 0) { $detail = ' Still down: ' + ($down -join ', ') + '.' }
    throw ('the cluster core group is ' + $after + ' after being started.' + $detail + ' An IP address resource usually refuses because its address is in use, or because no member still has an adapter on that subnet.')
  }
}

# ---- the storage it was holding down ---------------------------------------
foreach ($csv in @(Get-ClusterSharedVolume -ErrorAction SilentlyContinue)) {
  Start-BallastResource $csv ('volume ' + [string]$csv.Name)
}
foreach ($g in @(Get-ClusterGroup -ErrorAction SilentlyContinue | Where-Object { [string]$_.GroupType -eq 'AvailableStorage' })) {
  if ([string]$g.State -ne 'Online') {
    try { Start-ClusterGroup -InputObject $g -ErrorAction Stop | Out-Null; $started += 'available storage' } catch {}
  }
}

if ($stuck.Count -gt 0) {
  # Reported as a failure even though the core group came up. Half a recovery
  # presented as a whole one is how an operator walks away from a cluster that
  # still cannot run anything.
  $head = 'the cluster core group is online'
  if ($started.Count -gt 0) { $head = 'brought online: ' + ($started -join ', ') }
  throw ($head + '; but ' + ($stuck -join '; ') + '.')
}
if ($started.Count -eq 0) { 'RESULT=NOOP'; return }
'RESULT=UPDATED ' + ($started -join ', ')
`

// StartClusterCoreGroup brings the cluster's core group online, then the storage
// that could not come online behind it. Idempotent: a cluster already fully
// online is a no-op.
//
// VMs are left alone — power is imperative, and a recovery that starts workloads
// nobody asked for is not one an operator can run without checking first.
func (p *PowerShell) StartClusterCoreGroup(ctx context.Context) (Outcome, string, error) {
	out, err := p.run(ctx, coreGroupScript)
	if err != nil {
		return OutcomeUnchanged, "", fmt.Errorf("bring cluster resources online: %w", err)
	}
	s := strings.TrimSpace(string(out))
	if strings.Contains(s, "RESULT=NOOP") {
		return OutcomeUnchanged, "the cluster's resources were already online", nil
	}
	if i := strings.Index(s, "RESULT=UPDATED"); i >= 0 {
		return OutcomeUpdated, strings.TrimSpace(strings.TrimPrefix(s[i:], "RESULT=UPDATED")), nil
	}
	// No marker means the script stopped part-way without raising. Reporting
	// success would claim a recovery that may not have happened.
	return OutcomeUnchanged, "", fmt.Errorf("bring cluster resources online: ended without a result, so whether anything came online is unknown")
}
