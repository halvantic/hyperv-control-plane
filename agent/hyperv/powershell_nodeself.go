package hyperv

import (
	"context"
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
$out = [ordered]@{ node=''; service=''; joined=$false }

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
}

// GetNodeSelf reads this host's own cluster membership state. A pure read, and it
// never fails: a host in no cluster answers with an empty state, which is correct
// rather than an error.
func (p *PowerShell) GetNodeSelf(ctx context.Context) (NodeSelf, error) {
	raw, err := p.run(ctx, nodeSelfScript)
	if err != nil {
		// Not an error worth propagating: this is an observation, and a host that
		// cannot answer is reported as not having answered.
		return NodeSelf{}, nil
	}
	var res struct {
		Node    string `json:"node"`
		Service string `json:"service"`
		Joined  bool   `json:"joined"`
	}
	if derr := decodeJSON(raw, &res); derr != nil {
		return NodeSelf{}, nil
	}
	return NodeSelf{
		State:   strings.TrimSpace(res.Node),
		Service: strings.TrimSpace(res.Service),
		Joined:  res.Joined,
	}, nil
}
