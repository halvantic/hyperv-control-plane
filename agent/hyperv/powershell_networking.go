package hyperv

import (
	"context"
	"fmt"
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

// vnicObservation is the actual state of a management OS vNIC.
//
// IP configuration is intentionally not observed or reconciled in v1: rewriting
// the management NIC's address is how a host strands itself, and that path must
// be validated on a real host before the agent applies it autonomously. The
// adapter, its switch binding, and its VLAN are reconciled here.
type vnicObservation struct {
	Exists     bool   `json:"exists"`
	SwitchName string `json:"switchName"`
	VlanID     int    `json:"vlanID"`
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

// EnsureMgmtVNIC makes the management OS vNIC exist on its switch and carry its
// VLAN and bandwidth-weight intent, idempotently. See vnicObservation for why
// IP configuration is deferred.
func (p *PowerShell) EnsureMgmtVNIC(ctx context.Context, spec types.ManagementVNICSpec) (Outcome, error) {
	obs, err := p.queryVNIC(ctx, spec.Name)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("query vNIC %q: %w", spec.Name, err)
	}

	switch planVNIC(spec, obs) {
	case vnicNoop:
		return OutcomeUnchanged, nil
	case vnicCreate:
		if err := p.run2(ctx, createVNICScript(spec)); err != nil {
			return OutcomeUnchanged, fmt.Errorf("create vNIC %q: %w", spec.Name, err)
		}
		return OutcomeCreated, nil
	default: // vnicUpdate
		if err := p.run2(ctx, updateVNICScript(spec)); err != nil {
			return OutcomeUnchanged, fmt.Errorf("update vNIC %q: %w", spec.Name, err)
		}
		return OutcomeUpdated, nil
	}
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

func psBool(b bool) string {
	if b {
		return "$true"
	}
	return "$false"
}

// psAdapterList renders adapter names as a comma-separated list of quoted
// strings for -NetAdapterName.
func psAdapterList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = psQuote(n)
	}
	return strings.Join(quoted, ",")
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
