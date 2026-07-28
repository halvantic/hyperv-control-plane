package hyperv

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/joshua-fourie/ballast/api/types"
)

// ---------------------------------------------------------------------------
// Virtual switch
// ---------------------------------------------------------------------------

// switchObservation is the actual state of a SET switch, as read from the host.
type switchObservation struct {
	Exists            bool     `json:"exists"`
	TeamMembers       []string `json:"teamMembers"`
	LoadBalancing     string   `json:"loadBalancing"`
	AllowManagementOS bool     `json:"allowManagementOS"`
}

type switchPlan int

const (
	switchNoop switchPlan = iota
	switchCreate
	switchUpdate
)

// psLoadBalancing maps the schema's load-balancing constant to the PowerShell
// value used by Set-VMSwitchTeam.
func psLoadBalancing(l types.SETLoadBalancing) string {
	switch l {
	case types.SETHyperVPort:
		return "HyperVPort"
	case types.SETDynamic:
		return "Dynamic"
	default:
		return ""
	}
}

// planSwitch decides, purely, what an EnsureSwitch call must do to make obs
// match spec. Kept free of any host interaction so it is unit-tested directly.
func planSwitch(spec types.VirtualSwitchSpec, obs switchObservation) switchPlan {
	if !obs.Exists {
		return switchCreate
	}
	if !sameStringSet(spec.TeamMembers, obs.TeamMembers) ||
		psLoadBalancing(spec.LoadBalancing) != obs.LoadBalancing ||
		spec.AllowManagementOS != obs.AllowManagementOS {
		return switchUpdate
	}
	return switchNoop
}

func (p *PowerShell) querySwitch(ctx context.Context, name string) (switchObservation, error) {
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$sw = Get-VMSwitch -Name %[1]s -ErrorAction SilentlyContinue
if (-not $sw) { [pscustomobject]@{ exists = $false } | ConvertTo-Json -Compress; return }
$members = @()
$team = Get-VMSwitchTeam -Name %[1]s -ErrorAction SilentlyContinue
if ($team) {
  foreach ($desc in @($team.NetAdapterInterfaceDescription)) {
    $na = Get-NetAdapter -InterfaceDescription $desc -ErrorAction SilentlyContinue
    if ($na) { $members += $na.Name }
  }
}
$lb = if ($team) { [string]$team.LoadBalancingAlgorithm } else { '' }
$mgmt = @(Get-VMNetworkAdapter -ManagementOS -SwitchName %[1]s -ErrorAction SilentlyContinue)
[pscustomobject]@{
  exists            = $true
  teamMembers       = @($members)
  loadBalancing     = $lb
  allowManagementOS = ($mgmt.Count -gt 0)
} | ConvertTo-Json -Compress
`, psQuote(name))

	out, err := p.run(ctx, script)
	if err != nil {
		return switchObservation{}, err
	}
	var obs switchObservation
	if err := decodeJSON(out, &obs); err != nil {
		return switchObservation{}, err
	}
	return obs, nil
}

// EnsureSwitch makes the SET switch match spec, idempotently.
func (p *PowerShell) EnsureSwitch(ctx context.Context, spec types.VirtualSwitchSpec) (Outcome, error) {
	obs, err := p.querySwitch(ctx, spec.Name)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("query switch %q: %w", spec.Name, err)
	}

	switch planSwitch(spec, obs) {
	case switchNoop:
		return OutcomeUnchanged, nil
	case switchCreate:
		if err := p.run2(ctx, createSwitchScript(spec)); err != nil {
			return OutcomeUnchanged, fmt.Errorf("create switch %q: %w", spec.Name, err)
		}
		return OutcomeCreated, nil
	default: // switchUpdate
		if err := p.run2(ctx, updateSwitchScript(spec)); err != nil {
			return OutcomeUnchanged, fmt.Errorf("update switch %q: %w", spec.Name, err)
		}
		return OutcomeUpdated, nil
	}
}

func createSwitchScript(spec types.VirtualSwitchSpec) string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	// Weight bandwidth mode is set at creation (it cannot be changed afterwards)
	// so per-vNIC MinimumBandwidthWeight can be honoured — the converged-switch
	// QoS model this product targets. MinimumBandwidthMode is not read back in
	// planSwitch, so it never triggers a spurious update.
	fmt.Fprintf(&b, "New-VMSwitch -Name %s -NetAdapterName %s -EnableEmbeddedTeaming $true -MinimumBandwidthMode Weight -AllowManagementOS %s | Out-Null\n",
		psQuote(spec.Name), psAdapterList(spec.TeamMembers), psBool(spec.AllowManagementOS))
	if lb := psLoadBalancing(spec.LoadBalancing); lb != "" {
		fmt.Fprintf(&b, "Set-VMSwitchTeam -Name %s -LoadBalancingAlgorithm %s | Out-Null\n",
			psQuote(spec.Name), psQuote(lb))
	}
	return b.String()
}

func updateSwitchScript(spec types.VirtualSwitchSpec) string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	fmt.Fprintf(&b, "Set-VMSwitchTeam -Name %s -NetAdapterName %s | Out-Null\n",
		psQuote(spec.Name), psAdapterList(spec.TeamMembers))
	if lb := psLoadBalancing(spec.LoadBalancing); lb != "" {
		fmt.Fprintf(&b, "Set-VMSwitchTeam -Name %s -LoadBalancingAlgorithm %s | Out-Null\n",
			psQuote(spec.Name), psQuote(lb))
	}
	fmt.Fprintf(&b, "Set-VMSwitch -Name %s -AllowManagementOS %s | Out-Null\n",
		psQuote(spec.Name), psBool(spec.AllowManagementOS))
	return b.String()
}

// ---------------------------------------------------------------------------
// Management OS vNIC
// ---------------------------------------------------------------------------

// vnicObservation is the actual state of a management OS vNIC: its adapter,
// switch binding and VLAN. IP configuration is observed and reconciled
// separately (see ipObservation) because it lives on the NetAdapter the vNIC
// projects, not on the VM adapter object.
type vnicObservation struct {
	Exists     bool   `json:"exists"`
	SwitchName string `json:"switchName"`
	VlanID     int    `json:"vlanID"`
}

// ipObservation is the actual IPv4 configuration on a management vNIC's
// projected interface ("vEthernet (<name>)").
type ipObservation struct {
	Address    string   `json:"address"` // CIDR, e.g. 10.0.0.21/24; empty if none/DHCP-less
	Gateway    string   `json:"gateway"`
	DNSServers []string `json:"dnsServers"`
	// Registers is the interface's "register this connection's addresses in
	// DNS" flag. Reconciled because it is part of what makes Failover
	// Clustering classify the interface's network as client-facing.
	Registers bool `json:"registers"`
}

type vnicPlan int

const (
	vnicNoop vnicPlan = iota
	vnicCreate
	vnicUpdate
)

// planVNIC decides what an EnsureMgmtVNIC call must do. Pure; unit-tested.
func planVNIC(spec types.ManagementVNICSpec, obs vnicObservation) vnicPlan {
	if !obs.Exists {
		return vnicCreate
	}
	if obs.SwitchName != spec.SwitchName || obs.VlanID != spec.VLANID {
		return vnicUpdate
	}
	return vnicNoop
}

func (p *PowerShell) queryVNIC(ctx context.Context, name string) (vnicObservation, error) {
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$a = Get-VMNetworkAdapter -ManagementOS -Name %[1]s -ErrorAction SilentlyContinue
if (-not $a) { [pscustomobject]@{ exists = $false } | ConvertTo-Json -Compress; return }
$vid = 0
$v = Get-VMNetworkAdapterVlan -ManagementOS -VMNetworkAdapterName %[1]s -ErrorAction SilentlyContinue
if ($v -and $v.OperationMode -eq 'Access') { $vid = [int]$v.AccessVlanId }
[pscustomobject]@{ exists = $true; switchName = [string]$a.SwitchName; vlanID = $vid } | ConvertTo-Json -Compress
`, psQuote(name))

	out, err := p.run(ctx, script)
	if err != nil {
		return vnicObservation{}, err
	}
	var obs vnicObservation
	if err := decodeJSON(out, &obs); err != nil {
		return vnicObservation{}, err
	}
	return obs, nil
}

// EnsureMgmtVNIC makes the management OS vNIC exist on its switch with its VLAN
// and bandwidth weight, then (when the spec declares one) reconciles its static
// IPv4 configuration. Idempotent throughout: a converged vNIC returns
// OutcomeUnchanged and the IP step is skipped when actual already matches.
//
// IP reconciliation is observe-then-apply: the disruptive remove/replace only
// runs when the address, gateway or DNS actually drift, never on a steady pass.
func (p *PowerShell) EnsureMgmtVNIC(ctx context.Context, spec types.ManagementVNICSpec) (Outcome, error) {
	obs, err := p.queryVNIC(ctx, spec.Name)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("query vNIC %q: %w", spec.Name, err)
	}

	adapter := OutcomeUnchanged
	switch planVNIC(spec, obs) {
	case vnicCreate:
		if err := p.run2(ctx, createVNICScript(spec)); err != nil {
			return OutcomeUnchanged, fmt.Errorf("create vNIC %q: %w", spec.Name, err)
		}
		adapter = OutcomeCreated
	case vnicUpdate:
		if err := p.run2(ctx, updateVNICScript(spec)); err != nil {
			return OutcomeUnchanged, fmt.Errorf("update vNIC %q: %w", spec.Name, err)
		}
		adapter = OutcomeUpdated
	}

	ipChanged := false
	if spec.IPConfig != nil {
		changed, err := p.reconcileVNICIP(ctx, spec.Name, spec.IPConfig)
		if err != nil {
			return adapter, fmt.Errorf("ensure vNIC %q IP: %w", spec.Name, err)
		}
		ipChanged = changed
	}

	switch {
	case adapter == OutcomeCreated:
		return OutcomeCreated, nil
	case adapter == OutcomeUpdated || ipChanged:
		return OutcomeUpdated, nil
	default:
		return OutcomeUnchanged, nil
	}
}

// reconcileVNICIP observes the vNIC's current IPv4 config and applies the
// desired one only if it differs. Returns whether it changed anything.
func (p *PowerShell) reconcileVNICIP(ctx context.Context, name string, desired *types.IPConfig) (bool, error) {
	// Validate the desired address up front so we never run a destructive apply
	// with a malformed spec.
	ip, prefix, err := parseCIDR(desired.Address)
	if err != nil {
		return false, err
	}

	obs, err := p.queryVNICIP(ctx, name)
	if err != nil {
		return false, fmt.Errorf("query IP: %w", err)
	}
	if !ipDiffers(desired, obs) {
		return false, nil
	}
	if err := p.run2(ctx, applyIPScript(name, ip, prefix, desired)); err != nil {
		return false, err
	}
	return true, nil
}

func (p *PowerShell) queryVNICIP(ctx context.Context, name string) (ipObservation, error) {
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$alias = 'vEthernet (%[1]s)'
$clusterIps = @(); try { $clusterIps = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -like 'IP Address*' } | ForEach-Object { [string]($_ | Get-ClusterParameter -Name Address -ErrorAction SilentlyContinue).Value }) } catch {}
$ip = Get-NetIPAddress -InterfaceAlias $alias -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.PrefixOrigin -eq 'Manual' -and $_.IPAddress -notlike '169.254.*' -and ($clusterIps -notcontains $_.IPAddress) } | Select-Object -First 1
$gw = (Get-NetRoute -InterfaceAlias $alias -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue | Select-Object -First 1).NextHop
$dns = @((Get-DnsClientServerAddress -InterfaceAlias $alias -AddressFamily IPv4 -ErrorAction SilentlyContinue).ServerAddresses)
$reg = [bool](Get-DnsClient -InterfaceAlias $alias -ErrorAction SilentlyContinue).RegisterThisConnectionsAddress
[pscustomobject]@{
  address    = if ($ip) { "$($ip.IPAddress)/$($ip.PrefixLength)" } else { '' }
  gateway    = if ($gw) { [string]$gw } else { '' }
  dnsServers = @($dns)
  registers  = $reg
} | ConvertTo-Json -Compress
`, name) // name is a host-controlled adapter name; not attacker-supplied.

	out, err := p.run(ctx, script)
	if err != nil {
		return ipObservation{}, err
	}
	var obs ipObservation
	if err := decodeJSON(out, &obs); err != nil {
		return ipObservation{}, err
	}
	return obs, nil
}

// ipDiffers reports whether the desired IPv4 config differs from observed.
// DNS order is significant (it is resolver priority), so it is compared exactly.
func ipDiffers(desired *types.IPConfig, obs ipObservation) bool {
	if desired.Address != obs.Address {
		return true
	}
	if desired.Gateway != obs.Gateway {
		return true
	}
	// DNS registration follows the DNS declaration: a management vNIC (has DNS)
	// registers its address; an isolated cluster-network vNIC (no DNS) must not,
	// or clustering classifies its network as client-facing and New-Cluster
	// demands a cluster IP for it.
	if (len(desired.DNSServers) > 0) != obs.Registers {
		return true
	}
	return !sameOrderedStrings(desired.DNSServers, obs.DNSServers)
}

func applyIPScript(name, ip string, prefix int, cfg *types.IPConfig) string {
	alias := fmt.Sprintf("vEthernet (%s)", name)
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	fmt.Fprintf(&b, "$alias = %s\n", psQuote(alias))
	// Switch off DHCP, then clear any existing address and default route so the
	// new address applies cleanly and idempotently.
	b.WriteString("Set-NetIPInterface -InterfaceAlias $alias -Dhcp Disabled -ErrorAction SilentlyContinue\n")
	// The desired address may already exist on a *different* interface (e.g. an
	// AllowManagementOS auto-vNIC picked it up via DHCP). Free it there first, or
	// New-NetIPAddress below fails "object already exists" (Windows error 5010).
	fmt.Fprintf(&b, "Get-NetIPAddress -IPAddress %s -AddressFamily IPv4 -ErrorAction SilentlyContinue | Remove-NetIPAddress -Confirm:$false -ErrorAction SilentlyContinue\n", psQuote(ip))
	// Clear only addresses we own: Manual statics and DHCP leases — and never
	// an address belonging to a cluster IP Address resource, whatever its
	// PrefixOrigin. The cluster IP appears on the owning node's vNIC as 'Other'
	// on some builds but Manual on others, so origin alone is not a safe guard:
	// deleting it under a live resource fails its health check with 1168 and
	// parks the cluster IP Failed once the restart budget runs out.
	b.WriteString("$clusterIps = @(); try { $clusterIps = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -like 'IP Address*' } | ForEach-Object { [string]($_ | Get-ClusterParameter -Name Address -ErrorAction SilentlyContinue).Value }) } catch {}\n")
	b.WriteString("Get-NetIPAddress -InterfaceAlias $alias -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { ($_.PrefixOrigin -eq 'Manual' -or $_.PrefixOrigin -eq 'Dhcp') -and ($clusterIps -notcontains $_.IPAddress) } | Remove-NetIPAddress -Confirm:$false -ErrorAction SilentlyContinue\n")
	b.WriteString("Remove-NetRoute -InterfaceAlias $alias -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -Confirm:$false -ErrorAction SilentlyContinue\n")
	if cfg.Gateway != "" {
		fmt.Fprintf(&b, "New-NetIPAddress -InterfaceAlias $alias -IPAddress %s -PrefixLength %d -DefaultGateway %s | Out-Null\n",
			psQuote(ip), prefix, psQuote(cfg.Gateway))
	} else {
		fmt.Fprintf(&b, "New-NetIPAddress -InterfaceAlias $alias -IPAddress %s -PrefixLength %d | Out-Null\n",
			psQuote(ip), prefix)
	}
	if len(cfg.DNSServers) > 0 {
		fmt.Fprintf(&b, "Set-DnsClientServerAddress -InterfaceAlias $alias -ServerAddresses %s | Out-Null\n",
			psStringList(cfg.DNSServers))
		b.WriteString("Set-DnsClient -InterfaceAlias $alias -RegisterThisConnectionsAddress $true -ErrorAction SilentlyContinue\n")
	} else {
		// No DNS declared: an isolated cluster network (storage, live migration).
		// Also stop it registering in DNS — a published A record for a non-routed
		// address breaks name resolution to the host, and DNS on the interface
		// makes clustering treat the network as client-facing.
		b.WriteString("Set-DnsClientServerAddress -InterfaceAlias $alias -ResetServerAddresses | Out-Null\n")
		b.WriteString("Set-DnsClient -InterfaceAlias $alias -RegisterThisConnectionsAddress $false -ErrorAction SilentlyContinue\n")
	}
	if cfg.Gateway == "" {
		// Isolated networks must not pick up an IPv6 default route from router
		// advertisements either — on an untagged/flat layer-2 every vNIC hears
		// the RA, and any default route makes clustering classify the network
		// ClusterAndClient (New-Cluster then demands a cluster IP for it).
		b.WriteString("Set-NetIPInterface -InterfaceAlias $alias -AddressFamily IPv6 -RouterDiscovery Disabled -ErrorAction SilentlyContinue\n")
		b.WriteString("Get-NetRoute -InterfaceAlias $alias -AddressFamily IPv6 -DestinationPrefix '::/0' -ErrorAction SilentlyContinue | Remove-NetRoute -Confirm:$false -ErrorAction SilentlyContinue\n")
	}
	// Creating/rebinding a management vNIC makes NLA reclassify it, and at that
	// instant the domain controller is not yet reachable on it, so Windows lands
	// the adapter on Public/Private and never re-checks — breaking domain auth and
	// cross-node WMI/RPC (Add-ClusterVirtualMachineRole etc.). Now that the vNIC
	// has its address + DNS, restart NLA so it re-detects the DC and promotes the
	// adapter to DomainAuthenticated. Best-effort; re-classification only, no link
	// drop. Runs only when the IP/DNS is (re)applied, not every reconcile pass.
	b.WriteString("try { Restart-Service NlaSvc -Force -ErrorAction Stop; Start-Sleep -Seconds 3 } catch {}\n")
	return b.String()
}

// parseCIDR splits an address like "10.0.0.21/24" into its IP and prefix length.
func parseCIDR(cidr string) (ip string, prefix int, err error) {
	host, ipnet, perr := net.ParseCIDR(cidr)
	if perr != nil {
		return "", 0, fmt.Errorf("invalid CIDR address %q: %w", cidr, perr)
	}
	ones, _ := ipnet.Mask.Size()
	return host.String(), ones, nil
}

func createVNICScript(spec types.ManagementVNICSpec) string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	fmt.Fprintf(&b, "Add-VMNetworkAdapter -ManagementOS -Name %s -SwitchName %s | Out-Null\n",
		psQuote(spec.Name), psQuote(spec.SwitchName))
	b.WriteString(vlanCommand(spec))
	b.WriteString(weightCommand(spec))
	return b.String()
}

func updateVNICScript(spec types.ManagementVNICSpec) string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	// A management vNIC cannot be moved between switches in place; reconnect it.
	fmt.Fprintf(&b, "Connect-VMNetworkAdapter -VMNetworkAdapter (Get-VMNetworkAdapter -ManagementOS -Name %s) -SwitchName %s | Out-Null\n",
		psQuote(spec.Name), psQuote(spec.SwitchName))
	b.WriteString(vlanCommand(spec))
	b.WriteString(weightCommand(spec))
	return b.String()
}

func vlanCommand(spec types.ManagementVNICSpec) string {
	if spec.VLANID == 0 {
		return fmt.Sprintf("Set-VMNetworkAdapterVlan -ManagementOS -VMNetworkAdapterName %s -Untagged | Out-Null\n",
			psQuote(spec.Name))
	}
	return fmt.Sprintf("Set-VMNetworkAdapterVlan -ManagementOS -VMNetworkAdapterName %s -Access -VlanId %d | Out-Null\n",
		psQuote(spec.Name), spec.VLANID)
}

func weightCommand(spec types.ManagementVNICSpec) string {
	if spec.MinBandwidthWeight <= 0 {
		return ""
	}
	return fmt.Sprintf("Set-VMNetworkAdapter -ManagementOS -Name %s -MinimumBandwidthWeight %d | Out-Null\n",
		psQuote(spec.Name), spec.MinBandwidthWeight)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// run2 runs an act script and discards stdout; act scripts pipe to Out-Null.
func (p *PowerShell) run2(ctx context.Context, script string) error {
	_, err := p.run(ctx, script)
	return err
}

// ConvergedNetworkReady reports whether this node's converged networking is in
// place: every named SET switch exists AND a management vNIC carries a routable
// manual IPv4 (the host's management IP re-homed onto the switch's vNIC, not still
// on a physical NIC or on APIPA). The cluster former uses this to defer formation
// until networking is stable — forming first and re-homing the IP afterwards is
// what churns heartbeats/DNS on a live cluster.
func (p *PowerShell) ConvergedNetworkReady(ctx context.Context, switchNames []string) (bool, error) {
	if len(switchNames) == 0 {
		return true, nil
	}
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
$switches = %[1]s
$ready = $true
foreach ($sw in $switches) { if (-not (Get-VMSwitch -Name $sw -ErrorAction SilentlyContinue)) { $ready = $false } }
$hasMgmtIP = @(Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.PrefixOrigin -eq 'Manual' -and $_.IPAddress -notlike '169.254.*' -and $_.InterfaceAlias -like 'vEthernet*' }).Count -gt 0
if (-not $hasMgmtIP) { $ready = $false }
[pscustomobject]@{ ready = [bool]$ready } | ConvertTo-Json -Compress`, psStringList(switchNames))
	out, err := p.run(ctx, script)
	if err != nil {
		return false, fmt.Errorf("check converged network ready: %w", err)
	}
	var res struct {
		Ready bool `json:"ready"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return false, fmt.Errorf("check converged network ready: %w", err)
	}
	return res.Ready, nil
}

// PruneManagementVNICs removes stray management-OS vNICs on the given managed
// switches: any vNIC not in keep that carries no real IPv4 at all (an auto or
// leftover vNIC on APIPA/link-local only). It never removes a declared (kept)
// vNIC, never removes the last management vNIC on a switch (it only prunes where
// a kept vNIC remains, so AllowManagementOS stays true and the switch keeps its
// management connection), and leaves a vNIC holding ANY non-APIPA IPv4 —
// whatever its origin. That last rule is load-bearing: the failover cluster's
// IP address resource is added to the owning node's vNIC with PrefixOrigin
// 'Other' (not 'Manual'), and DHCP leases are 'Dhcp' — a Manual-only test once
// deleted a vNIC carrying the live cluster IP, killing the cluster IP resource
// and dropping the node out of the cluster network.
func (p *PowerShell) PruneManagementVNICs(ctx context.Context, switches, keep []string) (Outcome, error) {
	if len(switches) == 0 {
		return OutcomeUnchanged, nil
	}
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
$switches = %[1]s
$keep = %[2]s
$all = @(Get-VMNetworkAdapter -ManagementOS -ErrorAction SilentlyContinue)
$removed = @()
foreach ($a in $all) {
  $sw = [string]$a.SwitchName; $nm = [string]$a.Name
  if ($switches -notcontains $sw) { continue }
  if ($keep -contains $nm) { continue }
  $hasKept = @($all | Where-Object { [string]$_.SwitchName -eq $sw -and $keep -contains [string]$_.Name }).Count -gt 0
  if (-not $hasKept) { continue }
  $alias = 'vEthernet (' + $nm + ')'
  $hasIP = @(Get-NetIPAddress -InterfaceAlias $alias -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -notlike '169.254.*' }).Count -gt 0
  if ($hasIP) { continue }
  try { Remove-VMNetworkAdapter -ManagementOS -Name $nm -ErrorAction Stop; $removed += ($nm + '@' + $sw) } catch {}
}
if ($removed.Count -gt 0) { 'RESULT=REMOVED ' + ($removed -join ',') } else { 'RESULT=NOOP' }`,
		psStringList(switches), psStringList(keep))
	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("prune management vNICs: %w", err)
	}
	if strings.Contains(string(out), "RESULT=REMOVED") {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
}

// GetNetworkProfile returns the host's Windows network-location category: the
// weakest ("Public" beats "Private" beats "DomainAuthenticated") across the
// connection profiles that carry a default gateway. Empty when none are
// reported. The UI shows it as a coloured pill; Public/Private link to the
// repair action.
//
// Only gateway-bearing NICs count. A converged host's storage and live-migration
// vNICs sit on isolated subnets with no domain controller to authenticate
// against, so Windows correctly categorises them Private — and weakest-wins
// across every profile therefore reported Private on every properly built host,
// drowning out the management NIC and offering a repair that cannot help. What
// the pill is for is the domain-facing NIC's category: a management NIC stuck on
// Public or Private is what breaks cross-node WMI/RPC. Fall back to all profiles
// when none has a gateway, so a host mid-build still reports something.
func (p *PowerShell) GetNetworkProfile(ctx context.Context) (string, error) {
	const script = `$gwIdx = @(Get-NetRoute -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue | ForEach-Object { [int]$_.InterfaceIndex })
$profs = @(Get-NetConnectionProfile -ErrorAction SilentlyContinue)
$sel = @($profs | Where-Object { $gwIdx -contains [int]$_.InterfaceIndex })
if ($sel.Count -eq 0) { $sel = $profs }
$cats = @($sel | ForEach-Object { [string]$_.NetworkCategory })
if ($cats -contains 'Public') { 'Public' }
elseif ($cats -contains 'Private') { 'Private' }
elseif ($cats -contains 'DomainAuthenticated') { 'DomainAuthenticated' }
else { '' }`
	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("get network profile: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func psBool(b bool) string {
	if b {
		return "$true"
	}
	return "$false"
}

// psStringList renders items as a comma-separated list of quoted PowerShell
// strings, for parameters that take a string array (-NetAdapterName,
// -ServerAddresses).
func psStringList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = psQuote(s)
	}
	return strings.Join(quoted, ",")
}

// psAdapterList renders adapter names for -NetAdapterName.
func psAdapterList(names []string) string { return psStringList(names) }

// sameOrderedStrings reports whether a and b are equal element-for-element,
// order included. Used where sequence is significant (DNS resolver priority).
func sameOrderedStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameStringSet reports whether a and b contain the same elements, ignoring
// order and duplicates. Team membership is a set, not a sequence.
func sameStringSet(a, b []string) bool {
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	as = dedup(as)
	bs = dedup(bs)
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func dedup(sorted []string) []string {
	out := sorted[:0]
	var last string
	for i, s := range sorted {
		if i == 0 || s != last {
			out = append(out, s)
		}
		last = s
	}
	return out
}
