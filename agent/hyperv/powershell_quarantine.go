package hyperv

import (
	"context"
	"fmt"
	"strings"
)

// quarantineScript clears a cluster node's quarantine and brings it back into
// membership.
//
// Run from ANOTHER node, never the quarantined one. Quarantine works by stopping
// the cluster service on the offending node, so that node cannot talk to the
// cluster at all — asking it to readmit itself is asking the one machine that has
// been cut off. This is why bcluster2 went dark to Ballast: its designated former
// was the quarantined node, and every pass asked a stopped service what the
// cluster looked like.
//
// Clearing it is coordination through the cluster's own API, not a quorum
// decision. The cluster decides whether the node may rejoin, and quarantines it
// again if it keeps leaving — which is the point of the mechanism and not
// something to defeat by clearing it in a loop.
const quarantineScript = `
$ErrorActionPreference = 'Stop'
Import-Module FailoverClusters -ErrorAction SilentlyContinue
$node = %[1]s

$n = @(Get-ClusterNode -Name $node -ErrorAction SilentlyContinue)[0]
if (-not $n) {
  throw ('no cluster node named ' + $node + ' is known to this cluster. Run this from a node that is still a member — a quarantined node cannot answer for the cluster, because quarantine works by stopping its cluster service.')
}
$state = [string]$n.State
if ($state -eq 'Up') { 'RESULT=NOOP ' + $node + ' is already Up'; return }

# Anything other than Quarantined is refused rather than "fixed". Start-ClusterNode
# on a node that is Down for a real reason papers over the reason, and the states
# mean different things: quarantined is the cluster refusing a node it does not
# trust, down is a node that is not there.
if ($state -ne 'Quarantined' -and $state -ne 'Isolated') {
  throw ($node + ' is ' + $state + ', not quarantined. Clearing a quarantine is only meaningful for a node the cluster has ejected; a node that is ' + $state + ' needs whatever put it there looked at instead.')
}

try {
  Start-ClusterNode -Name $node -ClearQuarantine -ErrorAction Stop | Out-Null
} catch {
  throw ('the cluster would not readmit ' + $node + ': ' + ([string]$_.Exception.Message).Trim() + '. A node is quarantined after leaving the cluster three times within an hour, so the cluster may still consider it unstable; whatever made it leave has to be fixed or it will be quarantined again.')
}

$n = @(Get-ClusterNode -Name $node -ErrorAction SilentlyContinue)[0]
$after = ''
if ($n) { $after = [string]$n.State }
if ($after -ne 'Up') {
  # Judged on the state afterwards, not on the call returning. Reporting success
  # for a node still outside the cluster is how an operator walks away from a
  # cluster that is still a node short.
  throw ('the quarantine on ' + $node + ' was cleared but it is ' + $after + ', not Up. It may still be rejoining; if it stays there, the reason it kept leaving the cluster has not gone away.')
}
'RESULT=UPDATED ' + $node + ' rejoined the cluster'
`

// ClearNodeQuarantine readmits a quarantined cluster node.
//
// Must be run from a DIFFERENT member: quarantine stops the cluster service on
// the node it applies to, so that node cannot act for the cluster. Idempotent —
// a node already Up is a no-op.
func (p *PowerShell) ClearNodeQuarantine(ctx context.Context, node string) (Outcome, string, error) {
	n := strings.TrimSpace(node)
	if n == "" {
		return OutcomeUnchanged, "", fmt.Errorf("clear quarantine: no node named")
	}
	out, err := p.run(ctx, fmt.Sprintf(quarantineScript, psQuote(n)))
	if err != nil {
		return OutcomeUnchanged, "", fmt.Errorf("clear quarantine on %q: %w", n, err)
	}
	s := strings.TrimSpace(string(out))
	if i := strings.Index(s, "RESULT=NOOP"); i >= 0 {
		return OutcomeUnchanged, strings.TrimSpace(strings.TrimPrefix(s[i:], "RESULT=NOOP")), nil
	}
	if i := strings.Index(s, "RESULT=UPDATED"); i >= 0 {
		return OutcomeUpdated, strings.TrimSpace(strings.TrimPrefix(s[i:], "RESULT=UPDATED")), nil
	}
	return OutcomeUnchanged, "", fmt.Errorf("clear quarantine on %q: ended without a result, so whether the node rejoined is unknown", n)
}

// QuarantineScriptForTest exposes the script so a test can assert its refusals
// without a cluster. The behaviour it guards — refusing a node that is merely
// Down, and explaining why the quarantined node cannot readmit itself — is not
// reachable any other way off a real cluster.
func QuarantineScriptForTest() string { return quarantineScript }
