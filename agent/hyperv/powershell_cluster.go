package hyperv

import (
	"context"
	"fmt"
	"strings"
)

type clusterOwnedObs struct {
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	State     string `json:"state"`
	GroupType string `json:"groupType,omitempty"`
}

type clusterObservation struct {
	Exists     bool              `json:"exists"`
	Name       string            `json:"name"`
	Members    []string          `json:"members"`
	Nodes      []clusterOwnedObs `json:"nodes"`
	Groups     []clusterOwnedObs `json:"groups"`
	CSVs       []clusterOwnedObs `json:"csvs"`
	ClusterVMs []clusterOwnedObs `json:"clustervms"`
}

// clusterStateScript observes membership plus clustered groups/roles and CSV
// ownership. @(...) guards a single element collapsing to an object, and -Depth
// keeps the nested arrays in the JSON.
const clusterStateScript = `
$ErrorActionPreference = 'Stop'
$c = Get-Cluster -ErrorAction SilentlyContinue
if (-not $c) { [pscustomobject]@{ exists = $false } | ConvertTo-Json -Compress; return }
$nodeObjs = @(Get-ClusterNode -ErrorAction SilentlyContinue | ForEach-Object {
  [pscustomobject]@{ name = [string]$_.Name; state = [string]$_.State } })
$nodes = @($nodeObjs | ForEach-Object { $_.name })
$groups = @(Get-ClusterGroup -ErrorAction SilentlyContinue | ForEach-Object {
  [pscustomobject]@{ name = [string]$_.Name; owner = [string]$_.OwnerNode; state = [string]$_.State; groupType = [string]$_.GroupType } })
$csvs = @(Get-ClusterSharedVolume -ErrorAction SilentlyContinue | ForEach-Object {
  [pscustomobject]@{ name = [string]$_.Name; owner = [string]$_.OwnerNode; state = [string]$_.State } })
$cvms = @(Get-ClusterGroup -ErrorAction SilentlyContinue | Where-Object { $_.GroupType -eq 'VirtualMachine' } | ForEach-Object {
  [pscustomobject]@{ name = [string]$_.Name; owner = [string]$_.OwnerNode; state = [string]$_.State } })
[pscustomobject]@{ exists = $true; name = [string]$c.Name; members = @($nodes); nodes = @($nodeObjs); groups = @($groups); csvs = @($csvs); clustervms = @($cvms) } | ConvertTo-Json -Compress -Depth 4
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
	groups := make([]ClusterGroup, 0, len(obs.Groups))
	for _, g := range obs.Groups {
		groups = append(groups, ClusterGroup{Name: g.Name, OwnerNode: g.Owner, State: g.State, GroupType: g.GroupType})
	}
	csvs := make([]ClusterCSV, 0, len(obs.CSVs))
	for _, v := range obs.CSVs {
		csvs = append(csvs, ClusterCSV{Name: v.Name, OwnerNode: v.Owner, State: v.State})
	}
	cvms := make([]ClusterVM, 0, len(obs.ClusterVMs))
	for _, v := range obs.ClusterVMs {
		cvms = append(cvms, ClusterVM{Name: v.Name, OwnerNode: v.Owner, State: v.State})
	}
	nodes := make([]ClusterNodeState, 0, len(obs.Nodes))
	for _, n := range obs.Nodes {
		nodes = append(nodes, ClusterNodeState{Name: n.Name, State: n.State})
	}
	return ClusterState{Exists: obs.Exists, Name: obs.Name, Members: obs.Members, Nodes: nodes, Groups: groups, CSVs: csvs, VMs: cvms}, nil
}

const installClusteringScript = `
$ErrorActionPreference = 'Stop'
$r = Install-WindowsFeature -Name Failover-Clustering -IncludeManagementTools
$changed = (@($r.FeatureResult) | Measure-Object).Count -gt 0
[pscustomobject]@{ changed = $changed } | ConvertTo-Json -Compress
`

// clusterFirewallScript enables the inbound firewall rule groups a cluster node
// needs to coordinate with peers. WMI carries the RPC calls cross-node cluster
// operations make (e.g. Add-ClusterVirtualMachineRole); without it they fail
// "RPC server unavailable". Enabling an already-enabled rule is a no-op.
const clusterFirewallScript = `
$ErrorActionPreference = 'Stop'
$groups = @('Failover Clusters','Windows Management Instrumentation (WMI)')
$changed = 0
foreach ($g in $groups) {
  # Enable the rule group AND make it apply on every profile: a cluster node's
  # management NIC sometimes sits on the Public profile (e.g. when the DC isn't
  # detected on it), and Domain/Private-scoped rules then don't apply, so cross-
  # node RPC/WMI (Add-ClusterVirtualMachineRole etc.) fails 'RPC server unavailable'.
  $rules = Get-NetFirewallRule -DisplayGroup $g -ErrorAction SilentlyContinue
  foreach ($r in $rules) {
    if ($r.Enabled -ne 'True' -or $r.Profile -ne 'Any') {
      Set-NetFirewallRule -Name $r.Name -Enabled True -Profile Any -ErrorAction SilentlyContinue
      $changed++
    }
  }
}
[pscustomobject]@{ changed = ($changed -gt 0) } | ConvertTo-Json -Compress
`

func (p *PowerShell) EnsureClusterFirewall(ctx context.Context) (Outcome, error) {
	out, err := p.run(ctx, clusterFirewallScript)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure cluster firewall: %w", err)
	}
	var res struct {
		Changed bool `json:"changed"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure cluster firewall: %w", err)
	}
	if res.Changed {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
}

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
// DestroyCluster tears the cluster down from this node (the former): it removes
// every clustered VM role, disables Storage Spaces Direct (destroying the pool
// and CSVs), and removes the failover cluster along with its AD computer object.
// Destructive and idempotent — a no-op when no cluster exists. Runs agent-local
// (cluster cmdlets cannot run over a remote WinRM double-hop).
func (p *PowerShell) DestroyCluster(ctx context.Context) error {
	script := "$ErrorActionPreference='Stop'; Import-Module FailoverClusters; " +
		"if (-not (Get-Cluster -ErrorAction SilentlyContinue)) { 'no cluster'; return }; " +
		"Get-ClusterGroup -ErrorAction SilentlyContinue | Where-Object { $_.GroupType -eq 'VirtualMachine' } | ForEach-Object { " +
		"Stop-ClusterGroup -Name $_.Name -ErrorAction SilentlyContinue | Out-Null; " +
		"Remove-ClusterGroup -Name $_.Name -RemoveResources -Force -ErrorAction SilentlyContinue }; " +
		"Disable-ClusterStorageSpacesDirect -Confirm:$false -ErrorAction SilentlyContinue; " +
		"Remove-Cluster -Force -CleanupAD; 'destroyed'"
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("destroy cluster: %w", err)
	}
	return nil
}

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
