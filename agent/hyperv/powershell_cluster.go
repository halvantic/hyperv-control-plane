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
	SizeBytes      uint64 `json:"sizeBytes"`
	FreeBytes      uint64 `json:"freeBytes"`
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
	Name            string `json:"name"`
	RawBytes        uint64 `json:"rawBytes"`
	AllocatedBytes  uint64 `json:"allocatedBytes"`
	Health          string `json:"health"`
	Operational     string `json:"operational"`
	UnhealthyDisks  int    `json:"unhealthyDisks"`
	TotalDisks      int    `json:"totalDisks"`
	Resyncing       bool   `json:"resyncing"`
	ResyncPercent   int    `json:"resyncPercent"`
	ResyncJob       string `json:"resyncJob"`
	ResyncRemaining uint64 `json:"resyncRemaining"`
}

type clusterNetworkObs struct {
	Name   string `json:"name"`
	CIDR   string `json:"cidr"`
	Role   string `json:"role"`
	State  string `json:"state"`
	Metric int    `json:"metric"`
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
  # Size comes from the VOLUME, not the backing virtual disk, and the difference
  # is the whole point of reporting it. Growing a volume is two steps — grow the
  # virtual disk, then extend the partition into it — and a half-done resize
  # leaves the virtual disk larger while the usable space is unchanged. Reporting
  # the virtual disk's size would confirm a resize that never reached the
  # filesystem.
  $sz = [uint64]0; $free = [uint64]0
  $info = $_.SharedVolumeInfo
  if ($info -and $info.Partition) {
    $sz = [uint64]$info.Partition.Size
    $free = [uint64]$info.Partition.FreeSpace
  }
  [pscustomobject]@{ name = $csvName; owner = [string]$_.OwnerNode; state = [string]$_.State; health = $h; operational = $op; detachedReason = $dr; sizeBytes = $sz; freeBytes = $free } })
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
  # No "| Select-Object -First 1": the -First pipeline stop can abort the whole
  # script after a storage cmdlet, exiting 0 with no output marker.
  $rjobs = @(Get-StorageJob -ErrorAction SilentlyContinue | Where-Object {
    [string]$_.JobState -in @('Running','Starting','Suspended') -and
    ([string]$_.Name -match 'Repair|Regener|Resync|Rebalance|Optimi')
  })
  $resync = ($rjobs.Count -gt 0)
  $rpct = 0; $rname = ''; $rrem = [uint64]0
  if ($resync) {
    # Progress comes from the BYTE counters, not PercentComplete. Storage Spaces
    # leaves PercentComplete at 0 for the whole of a running repair on many
    # builds — it is populated for some job types and not others — so reading it
    # reported "rebuilding 0%" for the entire rebuild and made the figure
    # worthless. BytesProcessed/BytesTotal are populated; summing them across the
    # jobs gives the rebuild's real overall progress.
    $tot = 0.0; $don = 0.0
    foreach ($j in $rjobs) {
      if ($j.BytesTotal) { $tot += [double]$j.BytesTotal }
      if ($j.BytesProcessed) { $don += [double]$j.BytesProcessed }
    }
    # Outstanding work. The percentage cannot express overall progress: a
    # finished job disappears from Get-StorageJob, so the next one starts the
    # count again — seen live going 21% to 84% to 0. This figure IS comparable
    # across jobs. It trends down across a rebuild's phases, and stays put when a
    # repair keeps restarting and retaining nothing, which is the one thing that
    # tells those two apart.
    if ($tot -gt $don) { $rrem = [uint64]($tot - $don) }
    if ($tot -gt 0) {
      $rpct = [int][math]::Round(($don / $tot) * 100)
    } else {
      # No byte counters: fall back to the LEAST-progressed job, so several jobs
      # report the work still outstanding rather than one that has finished.
      $pcts = @($rjobs | ForEach-Object { [int]$_.PercentComplete })
      if ($pcts.Count -gt 0) { $rpct = [int](($pcts | Measure-Object -Minimum).Minimum) }
    }
    if ($rpct -lt 0) { $rpct = 0 }
    if ($rpct -gt 100) { $rpct = 100 }
    # Job names look like "<volume>-Repair"; report the kind, not the volume.
    $rname = [string]$rjobs[0].Name
    if ($rname -match '-([A-Za-z]+)$') { $rname = $Matches[1] }
  }
  [pscustomobject]@{ name = [string]$sp.FriendlyName; rawBytes = [uint64]$sp.Size; allocatedBytes = [uint64]$sp.AllocatedSize; health = [string]$sp.HealthStatus; operational = ([string]($sp.OperationalStatus -join ',')); unhealthyDisks = [int]$bad; totalDisks = [int]$pd.Count; resyncing = $resync; resyncPercent = $rpct; resyncJob = $rname; resyncRemaining = $rrem }
} else { $null }
$nets = @(Get-ClusterNetwork -ErrorAction SilentlyContinue | ForEach-Object {
  $bits = (($_.AddressMask -split '\.') | ForEach-Object { ([Convert]::ToString([int]$_,2)).ToCharArray() } | Where-Object { $_ -eq '1' }).Count
  $role = switch ([int]$_.Role) { 0 { 'None' } 1 { 'Cluster' } 3 { 'ClusterAndClient' } default { [string]$_.Role } }
  # Metric is what decides where cluster and CSV/SMB traffic actually goes:
  # lowest metric wins among the networks enabled for cluster use. Windows
  # assigns it automatically (preferring networks with no gateway), so the role
  # alone never answers "which network is storage on".
  [pscustomobject]@{ name = [string]$_.Name; cidr = ([string]$_.Address + '/' + $bits); role = $role; state = [string]$_.State; metric = [int]$_.Metric } })
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
			Health: v.Health, Operational: v.Operational, DetachedReason: v.DetachedReason,
			SizeBytes: v.SizeBytes, FreeBytes: v.FreeBytes})
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
			Resyncing: obs.Pool.Resyncing, ResyncPercent: obs.Pool.ResyncPercent, ResyncJob: obs.Pool.ResyncJob,
			ResyncRemainingBytes: obs.Pool.ResyncRemaining}
	}
	netw := make([]ClusterNetworkInfo, 0, len(obs.Networks))
	for _, n := range obs.Networks {
		netw = append(netw, ClusterNetworkInfo{Name: n.Name, CIDR: n.CIDR, Role: n.Role, State: n.State, Metric: n.Metric})
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

// A clustered VM's registration with Hyper-V IS a cluster resource: the "Virtual
// Machine Configuration" resource in the VM's group. When the role is taken
// Offline that resource goes offline too, and the VM is DEREGISTERED from Hyper-V
// on every node — Get-VM cannot see it anywhere, and says so with "Hyper-V was
// unable to find a virtual machine with name X".
//
// That reads exactly like a deleted VM, and it is not: the group, the resources,
// the configuration and the disks are all intact and the VM comes straight back
// when the role is started. It was diagnosed on the live rig after a stop, a
// failed clone and a failed capture all pointed at a VM nobody had touched.
//
// It also creates a catch-22 for any operation that copies a VM's disk. Those
// need the VM Off, because a running VM holds its VHDX open — but for a clustered
// VM, Off means the role is Offline, which means there is no VM to find.
//
// clusteredVMRegisterPrelude resolves it: when Get-VM comes up empty and the VM
// is an offline cluster role, it brings ONLY the configuration resource online.
// That registers the VM with Hyper-V and leaves it Off — the same state a
// clustered VM is in between being created and first started — without starting
// it. clusteredVMRestoreSuffix puts it back afterwards, so the operator's role
// state is unchanged either way.
//
// The scripts using this must place the body between the two, and must define
// nothing named $__vm, $__broughtOnline or $__cfg of their own.
func clusteredVMRegisterPrelude(vmVar string) string {
	return `
$global:__broughtOnline = $null
function Ensure-BallastVMRegistered {
  param([string]$n)
  $v = Get-VM -Name $n -ErrorAction SilentlyContinue
  if ($v) { return $v }
  # Get-ClusterResource is absent on a standalone host, which is a legitimate
  # "no, it is not a cluster role" rather than an error.
  $cfg = @(Get-ClusterResource -ErrorAction SilentlyContinue |
    Where-Object { [string]$_.OwnerGroup -eq $n -and [string]$_.ResourceType -eq 'Virtual Machine Configuration' })
  if ($cfg.Count -eq 0) { return $null }
  Start-ClusterResource -InputObject $cfg[0] -ErrorAction Stop | Out-Null
  if (-not $global:__broughtOnline) { $global:__broughtOnline = $cfg[0] }
  $deadline = (Get-Date).AddSeconds(60)
  while ($true) {
    $v = Get-VM -Name $n -ErrorAction SilentlyContinue
    if ($v) { return $v }
    if ((Get-Date) -gt $deadline) {
      throw ('the cluster role for ' + $n + ' came online but Hyper-V still does not see the VM after 60 seconds')
    }
    Start-Sleep -Seconds 2
  }
}
$__vm = Ensure-BallastVMRegistered ` + vmVar + `
if (-not $__vm) {
  throw ('no virtual machine named ' + ` + vmVar + ` + ' exists on this host, and it is not an offline cluster role here either. If it is clustered it may have moved to another node.')
}
try {
`
}

const clusteredVMRestoreSuffix = `
} finally {
  if ($global:__broughtOnline) {
    try { Stop-ClusterResource -InputObject $global:__broughtOnline -ErrorAction Stop | Out-Null } catch {}
  }
}
`

// NodeMaintenanceState is what the cluster says about this node's availability.
type NodeMaintenanceState struct {
	// IsMember is false on a host that is not in a cluster at all, where
	// maintenance is purely a centre-side fact and there is nothing to pause.
	IsMember bool
	// Paused means the node accepts no roles.
	Paused bool
	// Draining means roles are still moving off. A node is paused the instant the
	// drain is asked for, but it is not OUT OF SERVICE until its VMs have
	// actually left — reporting maintenance before then would tell an operator it
	// is safe to reboot a node still running their workloads.
	Draining bool
}

// MaintenanceIntent says what a pass should do about a node's availability.
//
// Observe exists because a node can be paused by someone who never went through
// Ballast — Failover Cluster Manager, a script, a half-finished job. Reading the
// state on every pass and only ACTING when maintenance is declared keeps that
// visible instead of the console quietly disagreeing with the cluster, and stops
// the reconciler resuming a node an operator paused by hand for a reason.
type MaintenanceIntent int

const (
	MaintenanceObserve MaintenanceIntent = iota
	MaintenanceEnter
	MaintenanceExit
)

func intentWord(i MaintenanceIntent) string {
	switch i {
	case MaintenanceEnter:
		return "enter"
	case MaintenanceExit:
		return "exit"
	default:
		return "observe"
	}
}

// maintenanceScript is built by a pure function so its content is pinned by
// tests without a host, like the template and clone scripts.
//
// The drain deliberately does NOT pass -Wait. On Suspend-ClusterNode that is a
// SWITCH, not a timeout: without it the drain is initiated and the cmdlet
// returns, which is what a reconciler wants — waiting would hold the whole cycle
// for minutes of live migration and look like a hang. "-Wait 0" would be worse
// than wrong, because 0 then binds positionally to -Name, a StringCollection —
// the same trap as the cluster cmdlets fixed in d113ecc. Checked against the
// cmdlet's real syntax on a live cluster before it ever ran.
func maintenanceScript(node string, intent MaintenanceIntent) string {
	return fmt.Sprintf(`$ErrorActionPreference='Stop'
$intent = %[2]s
# Get-ClusterNode is absent on a host with no FailoverClusters module, and
# returns nothing for a host that is not a member. Both mean "not a cluster
# node", which is not an error — maintenance on a standalone host is a
# centre-side fact with nothing to enforce here.
$n = $null
try { $n = @(Get-ClusterNode -Name %[1]s -ErrorAction SilentlyContinue)[0] } catch {}
if (-not $n) { [pscustomobject]@{ member=$false; paused=$false; draining=$false; changed=$false } | ConvertTo-Json -Compress; return }
$state = [string]$n.State
$drain = [string]$n.DrainStatus
$paused = ($state -eq 'Paused')
$changed = $false
if ($intent -eq 'enter' -and -not $paused) {
  Suspend-ClusterNode -Name %[1]s -Drain -ErrorAction Stop | Out-Null
  $changed = $true
} elseif ($intent -eq 'exit' -and $paused) {
  Resume-ClusterNode -Name %[1]s -ErrorAction Stop | Out-Null
  $changed = $true
}
if ($changed) {
  $n = @(Get-ClusterNode -Name %[1]s -ErrorAction SilentlyContinue)[0]
  if ($n) { $state = [string]$n.State; $drain = [string]$n.DrainStatus; $paused = ($state -eq 'Paused') }
}
[pscustomobject]@{ member=$true; paused=$paused; draining=($drain -eq 'InProgress'); changed=$changed } | ConvertTo-Json -Compress`,
		psQuote(node), psQuote(intentWord(intent)))
}

// EnsureNodeMaintenance reads a cluster node's availability and, when the intent
// says so, drives it. Idempotent: a paused node asked to pause is unchanged.
func (p *PowerShell) EnsureNodeMaintenance(ctx context.Context, node string, intent MaintenanceIntent) (Outcome, NodeMaintenanceState, error) {
	out, err := p.run(ctx, maintenanceScript(node, intent))
	if err != nil {
		return OutcomeUnchanged, NodeMaintenanceState{}, fmt.Errorf("ensure node maintenance on %q: %w", node, err)
	}
	var obs struct {
		Member   bool `json:"member"`
		Paused   bool `json:"paused"`
		Draining bool `json:"draining"`
		Changed  bool `json:"changed"`
	}
	if derr := decodeJSON(out, &obs); derr != nil {
		return OutcomeUnchanged, NodeMaintenanceState{}, fmt.Errorf("ensure node maintenance on %q: %w", node, derr)
	}
	st := NodeMaintenanceState{IsMember: obs.Member, Paused: obs.Paused, Draining: obs.Draining}
	if obs.Changed {
		return OutcomeUpdated, st, nil
	}
	return OutcomeUnchanged, st, nil
}
