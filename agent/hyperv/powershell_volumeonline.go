package hyperv

import (
	"context"
	"fmt"
	"strings"
)

/*
Bringing one cluster volume back online.

	An offline CSV is not a Ballast object gone wrong — the cluster owns whether a
	disk may come online, and it has usually taken it offline for a reason it will
	state. But an operator whose storage is down has, until now, had to open
	Failover Cluster Manager and right-click, which CLAUDE.md counts as a defect
	in Ballast rather than a step in a runbook. It bites hardest exactly here:
	after something has already gone wrong, when the operator is under most
	pressure and least able to improvise.

	Narrower than the core-group repair beside it, deliberately. That one exists
	because a whole cluster is down and everything is a symptom of one cause; this
	is an operator pointing at one volume and saying "try that again now".

	The cluster still decides. This asks through its own API and reports what it
	says back, including a refusal — it never forces anything, and it does not
	touch quorum, which is the cluster's alone.
*/
// volumeOnlineScript takes the volume name as a quoted literal. It is a name,
// not a secret — the environment is for credentials, and psQuote is how every
// other name reaches a script here.
func volumeOnlineScript(volume string) string {
	return fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
Import-Module FailoverClusters -ErrorAction SilentlyContinue

$want = %[1]s

# Find the volume's BACKING RESOURCE, which is what has a state and can be
# started. The CSV object itself is a presentation of it.
#
# Matched on the CSV's own name and on the resource name, because the two differ:
# a volume shown as "DS1" is commonly backed by "Cluster Virtual Disk (DS1)", and
# an operator naming what they see in the console must reach the right resource.
$res = $null
$csv = @(Get-ClusterSharedVolume -ErrorAction SilentlyContinue | Where-Object {
  ([string]$_.Name -eq $want) -or ([string]$_.Name -like ('*(' + $want + ')'))
})[0]
if ($csv) {
  $res = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { [string]$_.Name -eq [string]$csv.Name })[0]
}
if (-not $res) {
  $res = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object {
    ([string]$_.Name -eq $want) -or ([string]$_.Name -like ('*(' + $want + ')'))
  })[0]
}
if (-not $res) {
  throw ('this cluster has no volume or disk resource named ' + $want + '. It may have been removed, or renamed since the console last read it.')
}

$state = [string]$res.State
if ($state -eq 'Online') {
  'RESULT=NOOP ' + [string]$res.Name + ' is already online'
  return
}

try {
  # Through the object, never by re-looking-up the name: a CSV's backing resource
  # is not always found by Get-ClusterResource -Name on this build, and a lookup
  # that quietly finds nothing reports success having done nothing.
  Start-ClusterResource -InputObject $res -ErrorAction Stop | Out-Null
} catch {
  # The cluster's own words. It refuses for reasons an operator can act on —
  # a disk it cannot see, a reservation another node holds — and paraphrasing
  # them into "could not bring online" throws away the part that helps.
  throw ([string]$res.Name + ' would not come online (' + $state + '): ' + ([string]$_.Exception.Message).Trim())
}

# Read the state back rather than trusting the call. Start-ClusterResource
# returning is not the resource being online, and reporting a recovery that did
# not happen is worse than reporting the failure.
$after = ''
try { $after = [string](Get-ClusterResource -InputObject $res -ErrorAction SilentlyContinue).State } catch {}
if ($after -eq 'Online') {
  'RESULT=UPDATED ' + [string]$res.Name + ' is online'
} else {
  throw ([string]$res.Name + ' was asked to come online and is ' + $(if ($after) { $after } else { 'in an unknown state' }) +
    '. The cluster owns this decision; check the disk is visible to this node and that its storage is connected.')
}
`, psQuote(volume))
}

// StartClusterVolume brings one CSV or clustered disk online by name.
func (p *PowerShell) StartClusterVolume(ctx context.Context, volume string) (Outcome, string, error) {
	if strings.TrimSpace(volume) == "" {
		return OutcomeUnchanged, "", fmt.Errorf("bring volume online: no volume was named")
	}
	out, err := p.run(ctx, volumeOnlineScript(volume))
	if err != nil {
		return OutcomeUnchanged, "", fmt.Errorf("bring %s online: %w", volume, err)
	}
	s := strings.TrimSpace(string(out))
	if i := strings.Index(s, "RESULT=NOOP"); i >= 0 {
		return OutcomeUnchanged, strings.TrimSpace(strings.TrimPrefix(s[i:], "RESULT=NOOP")), nil
	}
	if i := strings.Index(s, "RESULT=UPDATED"); i >= 0 {
		return OutcomeUpdated, strings.TrimSpace(strings.TrimPrefix(s[i:], "RESULT=UPDATED")), nil
	}
	// No marker means the script stopped part-way without raising. Reporting
	// success would claim a recovery that may not have happened.
	return OutcomeUnchanged, "", fmt.Errorf("bring %s online: ended without a result, so whether it came online is unknown", volume)
}
