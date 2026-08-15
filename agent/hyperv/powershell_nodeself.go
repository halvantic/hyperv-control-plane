package hyperv

import (
	"context"
	"fmt"
	"strings"
)

// nodeSelfScript reads what this host can say about its OWN cluster membership.
//
// Both facts, because either alone is ambiguous. The node state is what the
// cluster thinks of this machine — Up, Paused, Quarantined — and is the useful
// answer when it can be had. But quarantine works by STOPPING the cluster
// service, so the node that most needs reporting is precisely the one that cannot
// answer, and its silence would read identically to "not clustered".
//
// The service state survives that. A stopped cluster service on a machine that
// has cluster membership on disk is the signal: this node is out, and must not be
// asked to speak for its cluster.
//
// Never throws. This runs on every host on every pass, including hosts in no
// cluster at all, where every one of these reads failing is the correct outcome.
const nodeSelfScript = `
$ErrorActionPreference = 'SilentlyContinue'
$out = [ordered]@{ node=''; service=''; joined=$false; cluster='' }

try { $out.service = [string](Get-Service ClusSvc -ErrorAction Stop).Status } catch {}

# Membership evidence that survives the service being down — the same evidence the
# cluster-state read uses, and for the same reason.
if (Test-Path 'HKLM:\Cluster') { $out.joined = $true }
if ((-not $out.joined) -and (Test-Path (Join-Path $env:SystemRoot 'Cluster\CLUSDB'))) { $out.joined = $true }
if (-not $out.joined) {
  $pn = (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Services\ClusSvc\Parameters' -ErrorAction SilentlyContinue).ClusterName
  if ($pn) { $out.joined = $true }
}

if ($out.joined) {
  try {
    Import-Module FailoverClusters -ErrorAction Stop
    $n = Get-ClusterNode -Name $env:COMPUTERNAME -ErrorAction Stop
    if ($n) { $out.node = [string]$n.State }
    # WHICH cluster. Read from the node itself rather than Get-Cluster so it is
    # the cluster this node belongs to, not whichever one the session happens to
    # be pointed at.
    try { $out.cluster = [string]$n.Cluster } catch {}
    if (-not $out.cluster) { try { $out.cluster = [string](Get-Cluster -ErrorAction Stop).Name } catch {} }
  } catch {}
}
$out | ConvertTo-Json -Compress
`

// NodeSelf is a host's own view of its cluster membership.
type NodeSelf struct {
	// State is what the cluster says of this node: Up, Paused, Quarantined,
	// Isolated, Down. Empty when it could not be read — which is itself
	// meaningful when Service is Stopped.
	State string
	// Service is the Failover Clustering service state on this host.
	Service string
	// Joined is true when this machine has cluster membership on disk, whatever
	// the service is doing. It distinguishes "not in a cluster" from "in a cluster
	// and cut off", which every other field here fails to.
	Joined bool
	// ClusterName is WHICH cluster this node is in, which every field above
	// leaves unsaid. State and Service answer "how is its membership", never "of
	// what" — so a node still joined to a cluster somebody thought they had
	// removed reports Up, Running and healthy, and Ballast agrees with it.
	//
	// Found 2026-08-16: three hosts wiped and re-authored into NewCluster
	// reported clusterNode=Up with no failing conditions, while the cluster the
	// centre had authored never formed. Every reading was true and about a
	// different cluster. Empty when the node is in none, or the name could not
	// be read.
	ClusterName string
}

// GetNodeSelf reads this host's own cluster membership state.
//
// A failure is REPORTED, not swallowed. The first version returned an empty
// NodeSelf and a nil error on any failure, which made a script that could not run
// indistinguishable from a host with nothing to say — three hosts reported no
// membership at all and there was no way to find out why. A host in no cluster
// genuinely has nothing to report; a host whose read failed has something to
// report and it is the failure.
func (p *PowerShell) GetNodeSelf(ctx context.Context) (NodeSelf, error) {
	raw, err := p.run(ctx, nodeSelfScript)
	if err != nil {
		return NodeSelf{}, fmt.Errorf("read own cluster membership: %w", err)
	}
	var res struct {
		Node    string `json:"node"`
		Service string `json:"service"`
		Joined  bool   `json:"joined"`
		Cluster string `json:"cluster"`
	}
	if derr := decodeJSON(raw, &res); derr != nil {
		return NodeSelf{}, fmt.Errorf("read own cluster membership: %w", derr)
	}
	return NodeSelf{
		State:       strings.TrimSpace(res.Node),
		Service:     strings.TrimSpace(res.Service),
		Joined:      res.Joined,
		ClusterName: strings.TrimSpace(res.Cluster),
	}, nil
}
