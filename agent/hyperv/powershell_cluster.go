package hyperv

import (
	"context"
	"fmt"
	"strings"
)

type clusterObservation struct {
	Exists  bool     `json:"exists"`
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

const clusterStateScript = `
$ErrorActionPreference = 'Stop'
$c = Get-Cluster -ErrorAction SilentlyContinue
if (-not $c) { [pscustomobject]@{ exists = $false } | ConvertTo-Json -Compress; return }
$nodes = @((Get-ClusterNode -ErrorAction SilentlyContinue).Name)
[pscustomobject]@{ exists = $true; name = [string]$c.Name; members = @($nodes) } | ConvertTo-Json -Compress
`

func (p *PowerShell) GetClusterState(ctx context.Context) (ClusterState, error) {
	out, err := p.run(ctx, clusterStateScript)
	if err != nil {
		return ClusterState{}, fmt.Errorf("get cluster state: %w", err)
	}
	var obs clusterObservation
	if err := decodeJSON(out, &obs); err != nil {
		return ClusterState{}, fmt.Errorf("get cluster state: %w", err)
	}
	return ClusterState{Exists: obs.Exists, Name: obs.Name, Members: obs.Members}, nil
}

const installClusteringScript = `
$ErrorActionPreference = 'Stop'
$r = Install-WindowsFeature -Name Failover-Clustering -IncludeManagementTools
$changed = (@($r.FeatureResult) | Measure-Object).Count -gt 0
[pscustomobject]@{ changed = $changed } | ConvertTo-Json -Compress
`

func (p *PowerShell) EnsureFailoverClusteringFeature(ctx context.Context) (Outcome, error) {
	out, err := p.run(ctx, installClusteringScript)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("install failover clustering: %w", err)
	}
	var res struct {
		Changed bool `json:"changed"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return OutcomeUnchanged, fmt.Errorf("install failover clustering: %w", err)
	}
	if res.Changed {
		return OutcomeCreated, nil
	}
	return OutcomeUnchanged, nil
}

// FormCluster runs New-Cluster on this node, the designated former. -NoStorage
// keeps formation independent of S2D (a later increment). The members are added
// in the same call, so a single former brings up the whole cluster.
func (p *PowerShell) FormCluster(ctx context.Context, f ClusterFormation) error {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	// -AdministrativeAccessPoint Dns avoids requiring an Active Directory
	// computer object, so this works for workgroup as well as domain hosts.
	fmt.Fprintf(&b, "New-Cluster -Name %s -Node %s -NoStorage -AdministrativeAccessPoint Dns -Force",
		psQuote(f.Name), psStringList(f.Members))
	if f.ManagementIP != "" {
		fmt.Fprintf(&b, " -StaticAddress %s", psQuote(f.ManagementIP))
	}
	b.WriteString(" | Out-Null\n")
	if err := p.run2(ctx, b.String()); err != nil {
		return fmt.Errorf("form cluster %q: %w", f.Name, err)
	}
	return nil
}
