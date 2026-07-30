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

// clusterCSVObs is a CSV plus the health of the virtual disk behind it, which is
// observed separately from the pool's — the two fail independently.
type clusterCSVObs struct {
	Name           string `json:"name"`
	Owner          string `json:"owner"`
	State          string `json:"state"`
	Health         string `json:"health"`
	Operational    string `json:"operational"`
	DetachedReason string `json:"detachedReason"`
}

type clusterObservation struct {
	Exists     bool                `json:"exists"`
	Known      bool                `json:"known"`
	Name       string              `json:"name"`
	Members    []string            `json:"members"`
	Nodes      []clusterOwnedObs   `json:"nodes"`
	Groups     []clusterOwnedObs   `json:"groups"`
	CSVs       []clusterCSVObs     `json:"csvs"`
	ClusterVMs []clusterOwnedObs   `json:"clustervms"`
	Pool       *clusterPoolObs     `json:"pool"`
	Networks   []clusterNetworkObs `json:"networks"`
}

type clusterPoolObs struct {
	Name           string `json:"name"`
	RawBytes       uint64 `json:"rawBytes"`
	AllocatedBytes uint64 `json:"allocatedBytes"`
	Health         string `json:"health"`
	Operational    string `json:"operational"`
	UnhealthyDisks int    `json:"unhealthyDisks"`
	TotalDisks     int    `json:"totalDisks"`
	Resyncing      bool   `json:"resyncing"`
	ResyncPercent  int    `json:"resyncPercent"`
	ResyncJob      string `json:"resyncJob"`
}

type clusterNetworkObs struct {
	Name  string `json:"name"`
	CIDR  string `json:"cidr"`
	Role  string `json:"role"`
	State string `json:"state"`
}

// clusterStateScript observes membership plus clustered groups/roles and CSV
// ownership. @(...) guards a single element collapsing to an object, and -Depth
// keeps the nested arrays in the JSON.
const clusterStateScript = `
$ErrorActionPreference = 'Stop'
$c = Get-Cluster -ErrorAction SilentlyContinue
if (-not $c) {
  # Get-Cluster returned nothing — but is the node truly un-clustered, or was the
  # cluster service just momentarily unavailable (starting, mid-operation)? A joined
  # node has the cluster database hive at HKLM:\Cluster. If it exists we ARE
  # clustered and must NOT report "no cluster", which would trigger a spurious
  # New-Cluster that fails ("already joined to a cluster"). Report exists+unknown so
  # the reconciler neither forms nor clobbers status this pass.
  if (Test-Path 'HKLM:\Cluster') { [pscustomobject]@{ exists = $true; known = $false } | ConvertTo-Json -Compress; return }
  [pscustomobject]@{ exists = $false; known = $true } | ConvertTo-Json -Compress; return
}
$nodeObjs = @(Get-ClusterNode -ErrorAction SilentlyContinue | ForEach-Object {
  [pscustomobject]@{ name = [string]$_.Name; state = [string]$_.State } })
$nodes = @($nodeObjs | ForEach-Object { $_.name })
$groups = @(Get-ClusterGroup -ErrorAction SilentlyContinue | ForEach-Object {
  [pscustomobject]@{ name = [string]$_.Name; owner = [string]$_.OwnerNode; state = [string]$_.State; groupType = [string]$_.GroupType } })
# Volume health is observed separately from pool health: the two fail
# independently, and attributing a volume's problem to the pool sends the
# operator to repair storage that is fine. A CSV is named "Cluster Virtual Disk
# (<volume>)", so match it back to its virtual disk by that inner name.
$vds = @{}
foreach ($vd in @(Get-VirtualDisk -ErrorAction SilentlyContinue)) {
  $vds[[string]$vd.FriendlyName] = $vd
}
$csvs = @(Get-ClusterSharedVolume -ErrorAction SilentlyContinue | ForEach-Object {
  $csvName = [string]$_.Name
  $vdName = $csvName
  if ($csvName -match '\(([^)]+)\)\s*$') { $vdName = $Matches[1] }
  $vd = $vds[$vdName]
  $h = ''; $op = ''; $dr = ''
  if ($vd) {
    $h = [string]$vd.HealthStatus
    $op = [string]($vd.OperationalStatus -join ',')
    $dr = [string]$vd.DetachedReason
  }
  [pscustomobject]@{ name = $csvName; owner = [string]$_.OwnerNode; state = [string]$_.State; health = $h; operational = $op; detachedReason = $dr } })
$cvms = @(Get-ClusterGroup -ErrorAction SilentlyContinue | Where-Object { $_.GroupType -eq 'VirtualMachine' } | ForEach-Object {
  [pscustomobject]@{ name = [string]$_.Name; owner = [string]$_.OwnerNode; state = [string]$_.State } })
$sp = Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial } | Select-Object -First 1
$pool = if ($sp) {
  $pd = @(Get-PhysicalDisk -StoragePool $sp -ErrorAction SilentlyContinue)
  $bad = @($pd | Where-Object { $_.HealthStatus -ne 'Healthy' }).Count
  # Observe whether S2D is actively rebuilding rather than inferring "broken"
  # from health alone. A repair/regeneration job makes the pool, its virtual
  # disks and some physical disks report non-Healthy while it runs — that is
  # normal, self-resolving work, not a fault needing operator action. Reporting
  # the job is what lets the console say "rebuilding, 12%" instead of telling
  # someone to repair a pool that is already repairing itself.
  $rjob = @(Get-StorageJob -ErrorAction SilentlyContinue | Where-Object {
    [string]$_.JobState -in @('Running','Starting','Suspended') -and
    ([string]$_.Name -match 'Repair|Regener|Resync|Rebalance|Optimi')
  }) | Sort-Object -Property @{ Expression = { [int]$_.PercentComplete } } | Select-Object -First 1
  $resync = [bool]$rjob
  $rpct = 0; $rname = ''
  if ($rjob) {
    $rpct = [int]$rjob.PercentComplete
    # Job names look like "<volume>-Repair"; report the kind, not the volume.
    $rname = [string]$rjob.Name
    if ($rname -match '-([A-Za-z]+)$') { $rname = $Matches[1] }
  }
  [pscustomobject]@{ name = [string]$sp.FriendlyName; rawBytes = [uint64]$sp.Size; allocatedBytes = [uint64]$sp.AllocatedSize; health = [string]$sp.HealthStatus; operational = ([string]($sp.OperationalStatus -join ',')); unhealthyDisks = [int]$bad; totalDisks = [int]$pd.Count; resyncing = $resync; resyncPercent = $rpct; resyncJob = $rname }
} else { $null }
$nets = @(Get-ClusterNetwork -ErrorAction SilentlyContinue | ForEach-Object {
  $bits = (($_.AddressMask -split '\.') | ForEach-Object { ([Convert]::ToString([int]$_,2)).ToCharArray() } | Where-Object { $_ -eq '1' }).Count
  $role = switch ([int]$_.Role) { 0 { 'None' } 1 { 'Cluster' } 3 { 'ClusterAndClient' } default { [string]$_.Role } }
  [pscustomobject]@{ name = [string]$_.Name; cidr = ([string]$_.Address + '/' + $bits); role = $role; state = [string]$_.State } })
[pscustomobject]@{ exists = $true; known = $true; name = [string]$c.Name; members = @($nodes); nodes = @($nodeObjs); groups = @($groups); csvs = @($csvs); clustervms = @($cvms); pool = $pool; networks = @($nets) } | ConvertTo-Json -Compress -Depth 4
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
		csvs = append(csvs, ClusterCSV{Name: v.Name, OwnerNode: v.Owner, State: v.State,
			Health: v.Health, Operational: v.Operational, DetachedReason: v.DetachedReason})
	}
	cvms := make([]ClusterVM, 0, len(obs.ClusterVMs))
	for _, v := range obs.ClusterVMs {
		cvms = append(cvms, ClusterVM{Name: v.Name, OwnerNode: v.Owner, State: v.State})
	}
	nodes := make([]ClusterNodeState, 0, len(obs.Nodes))
	for _, n := range obs.Nodes {
		nodes = append(nodes, ClusterNodeState{Name: n.Name, State: n.State})
	}
	var pool *ClusterPool
	if obs.Pool != nil {
		pool = &ClusterPool{Name: obs.Pool.Name, RawBytes: obs.Pool.RawBytes, AllocatedBytes: obs.Pool.AllocatedBytes,
			Health: obs.Pool.Health, Operational: obs.Pool.Operational, UnhealthyDisks: obs.Pool.UnhealthyDisks, TotalDisks: obs.Pool.TotalDisks,
			Resyncing: obs.Pool.Resyncing, ResyncPercent: obs.Pool.ResyncPercent, ResyncJob: obs.Pool.ResyncJob}
	}
	netw := make([]ClusterNetworkInfo, 0, len(obs.Networks))
	for _, n := range obs.Networks {
		netw = append(netw, ClusterNetworkInfo{Name: n.Name, CIDR: n.CIDR, Role: n.Role, State: n.State})
	}
	return ClusterState{Exists: obs.Exists, Known: obs.Known, Name: obs.Name, Members: obs.Members, Nodes: nodes, Groups: groups, CSVs: csvs, VMs: cvms, Pool: pool, Networks: netw}, nil
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
$groups = @('Failover Clusters','Windows Management Instrumentation (WMI)','Remote Event Log Management')
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

// EnsureMigrationDelegation sets Kerberos constrained delegation (the migration
// + cifs SPNs) between cluster nodes' computer accounts, which cluster-initiated
// live migration needs when the host auth type is Kerberos. Idempotent: it reads
// each computer's existing msDS-AllowedToDelegateTo and only adds what is
// missing. Run on the former as a domain admin.
//
// Uses System.DirectoryServices (LDAP port 389) rather than the ActiveDirectory
// PowerShell module so it does not depend on AD Web Services (port 9389) being
// reachable. ADWS is often absent or blocked on small lab DCs.
func (p *PowerShell) EnsureMigrationDelegation(ctx context.Context, nodes []string) (Outcome, error) {
	nodeExpr := "@(Get-ClusterNode -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.Name })"
	if len(nodes) > 0 {
		nodeExpr = "@(" + psStringList(nodes) + ")"
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$dom = (Get-CimInstance Win32_ComputerSystem).Domain
$domDn = 'DC=' + (($dom -split '\.') -join ',DC=')
# Locate a DC to connect to directly via LDAP/389.
# Strategy: try Netlogon DC locator first; if that fails (SRV records not
# resolvable on a freshly configured converged host) probe each DNS server
# configured on this host — in a domain environment the DNS servers are
# typically the DCs themselves, so attempting LDAP against each one works.
$dcAddr = $null
try {
  $ctx = [System.DirectoryServices.ActiveDirectory.DirectoryContext]::new(
    [System.DirectoryServices.ActiveDirectory.DirectoryContextType]::Domain, $dom)
  $dcAddr = ([System.DirectoryServices.ActiveDirectory.DomainController]::FindOne($ctx)).IPAddress
} catch {}
if (-not $dcAddr) {
  $candidates = @(Get-DnsClientServerAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
    Where-Object { $_.ServerAddresses } |
    ForEach-Object { $_.ServerAddresses } |
    Select-Object -Unique)
  foreach ($ip in $candidates) {
    try {
      $t = [System.DirectoryServices.DirectorySearcher]::new([ADSI]"LDAP://$ip/$domDn")
      $t.Filter = "(objectClass=domain)"; $t.SizeLimit = 1
      $null = $t.FindOne()
      $dcAddr = $ip; break
    } catch {}
  }
}
if (-not $dcAddr) { throw "cannot locate a domain controller for $dom — check DNS on this host points at a DC" }
# Always include the local host so delegation is configured both ways.
$nodes = (@($env:COMPUTERNAME) + (%[1]s)) | Where-Object { $_ } | Select-Object -Unique
$changed = @()
foreach ($src in $nodes) {
  $want = @()
  foreach ($dst in ($nodes | Where-Object { $_ -ne $src })) {
    $want += 'Microsoft Virtual System Migration Service/' + $dst + '.' + $dom
    $want += 'Microsoft Virtual System Migration Service/' + $dst
    $want += 'cifs/' + $dst + '.' + $dom
    $want += 'cifs/' + $dst
  }
  $srch = [System.DirectoryServices.DirectorySearcher]::new([ADSI]"LDAP://$dcAddr/$domDn")
  $srch.Filter = "(&(objectClass=computer)(sAMAccountName=${src}$))"
  $srch.PropertiesToLoad.Add('msDS-AllowedToDelegateTo') | Out-Null
  $result = $srch.FindOne()
  if (-not $result) { Write-Warning "AD: computer $src not found; skipping"; continue }
  $cur = @($result.Properties['msDS-AllowedToDelegateTo'] | ForEach-Object { [string]$_ })
  [string[]]$missing = @($want | Where-Object { $cur -notcontains $_ } | ForEach-Object { [string]$_ })
  if ($missing.Count -gt 0) {
    $de = $result.GetDirectoryEntry()
    foreach ($spn in $missing) { $de.Properties['msDS-AllowedToDelegateTo'].Add($spn) | Out-Null }
    $de.CommitChanges()
    $de.Dispose()
    $changed += $src
  }
}
[pscustomobject]@{ changed = ($changed.Count -gt 0) } | ConvertTo-Json -Compress`, nodeExpr)
	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure migration delegation: %w", err)
	}
	var res struct {
		Changed bool `json:"changed"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure migration delegation: %w", err)
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
	// Domain-joined hosts form the cluster with an Active Directory access point
	// (a real cluster name object, the CNO). Clustered Hyper-V live migration is
	// cluster-initiated and so must authenticate with Kerberos against the cluster
	// name; an AD-detached (DNS-only) cluster has no CNO and falls back to NTLM for
	// the cluster name, which makes Kerberos live migration unsupported (the
	// migration fails at "register cluster name in the local user groups", event
	// 20501). The access point is immutable after creation, so it must be chosen
	// correctly here. Workgroup hosts have no AD and fall back to DNS-only.
	b.WriteString("$aap = if ((Get-CimInstance Win32_ComputerSystem).PartOfDomain) { 'ActiveDirectoryAndDns' } else { 'Dns' }\n")
	fmt.Fprintf(&b, "New-Cluster -Name %s -Node %s -NoStorage -AdministrativeAccessPoint $aap -Force",
		psQuote(f.Name), psStringList(f.Members))
	if f.ManagementIP != "" {
		fmt.Fprintf(&b, " -StaticAddress %s", psQuote(f.ManagementIP))
	}
	b.WriteString(" | Out-Null\n")
	// Make the cluster tolerant of transient heartbeat loss under heavy I/O. On a
	// converged/nested cluster an S2D repair can saturate the single network, drop
	// heartbeats, partition nodes, and spiral into a repair storm (repair restarts
	// each time a node flaps). Raising the thresholds lets one repair pass finish
	// instead of flapping. Best-effort so it never fails formation.
	b.WriteString("try { $cl = Get-Cluster; $cl.SameSubnetThreshold = 20; $cl.SameSubnetDelay = 2000; $cl.CrossSubnetThreshold = 20; $cl.CrossSubnetDelay = 4000 } catch {}\n")
	if err := p.run2(ctx, b.String()); err != nil {
		return fmt.Errorf("form cluster %q: %w", f.Name, err)
	}
	return nil
}
