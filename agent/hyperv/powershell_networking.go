package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/joshua-fourie/ballast/api/types"
)

// ---------------------------------------------------------------------------
// Virtual switch
// ---------------------------------------------------------------------------

// switchObservation is the actual state of a SET switch, as read from the host.
// ErrHyperVUnavailable reports that Hyper-V itself did not answer — the Virtual
// Machine Management service is stopped, crashed or restarting — so nothing
// could be observed about switches or vNICs this pass.
//
// It exists because "Hyper-V did not answer" and "the object is not there" are
// the same empty result to a caller, and they have opposite remedies: defer, or
// create. Reading the first as the second is how a reconciler comes to run
// New-VMSwitch against a live SET team that is carrying the host's management
// IP. Observed on the rig 2026-08-06, when VMMS terminated unexpectedly on two
// nodes and the agent tried to create the converged switch and all three
// management vNICs on both. The creates failed only because VMMS was fully
// down; a partial recovery would have let them through.
//
// The asymmetry decides it, exactly as with the cluster-membership guard: being
// wrong about "present" costs one deferred pass, being wrong about "absent"
// rebuilds the host's networking underneath a running cluster.
var ErrHyperVUnavailable = errors.New("Hyper-V is not answering (its Virtual Machine Management service is stopped or restarting), so nothing was observed or changed this pass")

type switchObservation struct {
	Exists            bool     `json:"exists"`
	TeamMembers       []string `json:"teamMembers"`
	LoadBalancing     string   `json:"loadBalancing"`
	AllowManagementOS bool     `json:"allowManagementOS"`
	// Known is false when Hyper-V could not be read at all. Exists is then
	// meaningless and must never be acted on. Absent from the JSON reads as
	// false, so a script that forgets to set it fails safe (defer, not create).
	Known bool `json:"known"`
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
# Enumerate WITHOUT -Name so the two failures separate. A switch that genuinely
# does not exist comes back as an empty list; the only thing left that can throw
# here is Hyper-V itself being unavailable. Asking for a name would conflate
# them, because Get-VMSwitch -Name errors for a missing switch too.
try { $all = @(Get-VMSwitch -ErrorAction Stop) }
catch { [pscustomobject]@{ exists = $false; known = $false } | ConvertTo-Json -Compress; return }
$sw = @($all | Where-Object { $_.Name -eq %[1]s })[0]
if (-not $sw) { [pscustomobject]@{ exists = $false; known = $true } | ConvertTo-Json -Compress; return }
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
  known             = $true
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
	// Nothing was observed, so nothing may be concluded — least of all that the
	// switch is missing and should be built.
	if !obs.Known {
		return OutcomeUnchanged, fmt.Errorf("switch %q: %w", spec.Name, ErrHyperVUnavailable)
	}

	switch planSwitch(spec, obs) {
	case switchNoop:
		return OutcomeUnchanged, nil
	case switchCreate:
		if err := p.run2(ctx, createSwitchScript(spec)); err != nil {
			return OutcomeUnchanged, fmt.Errorf("create switch %q: %w", spec.Name, p.explainTeamMembers(ctx, spec, err))
		}
		return OutcomeCreated, nil
	default: // switchUpdate
		if err := p.run2(ctx, updateSwitchScript(spec)); err != nil {
			return OutcomeUnchanged, fmt.Errorf("update switch %q: %w", spec.Name, p.explainTeamMembers(ctx, spec, err))
		}
		return OutcomeUpdated, nil
	}
}

// adapterPresence is one physical adapter as the host sees it now, and what has
// claimed it. Enough to decide which NIC a declared team member should become.
type adapterPresence struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
	Up   bool   `json:"up"`
	// InUseBy names the SET switch already teaming this adapter, empty when it is
	// free. A free NIC is the one an operator can remap onto.
	InUseBy string `json:"inUseBy"`
	// IPv4 is a host address on the adapter. A NIC carrying the host's own
	// address must not be swallowed into a team without care, so it is shown.
	IPv4 string `json:"ipv4"`
}

// adaptersPresentScript lists the physical adapters and what owns each one.
func adaptersPresentScript() string {
	return `
$ErrorActionPreference = 'Stop'
$owned = @{}
try {
  foreach ($t in @(Get-VMSwitchTeam -ErrorAction SilentlyContinue)) {
    foreach ($desc in @($t.NetAdapterInterfaceDescription)) {
      $na = Get-NetAdapter -InterfaceDescription $desc -ErrorAction SilentlyContinue
      if ($na) { $owned[[string]$na.Name] = [string]$t.Name }
    }
  }
} catch {}
$list = @(Get-NetAdapter -Physical -ErrorAction SilentlyContinue | ForEach-Object {
  $a = @(Get-NetIPAddress -InterfaceIndex $_.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -notlike '169.254.*' })[0]
  [pscustomobject]@{
    name    = [string]$_.Name
    mac     = [string]$_.MacAddress
    up      = ($_.Status -eq 'Up')
    inUseBy = [string]$owned[[string]$_.Name]
    ipv4    = [string]$a.IPAddress
  }
})
ConvertTo-Json -InputObject $list -Depth 4 -Compress
`
}

func (p *PowerShell) adaptersPresent(ctx context.Context) ([]adapterPresence, error) {
	out, err := p.run(ctx, adaptersPresentScript())
	if err != nil {
		return nil, err
	}
	var list []adapterPresence
	if err := decodeJSON(out, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// explainTeamMembers turns a failed switch operation into something an operator
// can act on when the cause is a declared team member the host does not have.
//
// Swapping a NIC is ordinary maintenance, and Windows names the replacement
// something new — 'Ethernet0 2' where the spec says 'Ethernet0'. What Ballast
// said about it was:
//
//	update switch "ConvergedSwitch": powershell: exit status 1:
//	Set-VMSwitchTeam : Physical network adapter 'Ethernet0 2' not found.
//
// which names the cmdlet that failed and nothing an operator can do. The agent
// is ON the host and can enumerate its adapters, so which NICs exist and which
// are free is a fact it can establish, not research to hand out as homework.
//
// It deliberately does NOT remap anything. The team members are DESIRED state,
// and picking a replacement NIC is a decision about intent — which fabric a NIC
// belongs to is not visible from its name. The agent never invents intent; it
// states what is missing, what is available, and leaves the choice where it
// belongs. That is also why the adapters are listed with what already claims
// them: choosing needs that, and guessing cannot be done safely without it.
//
// Best effort. If the adapters cannot be read, or nothing is actually missing,
// the original error stands unchanged — a diagnosis that cannot be made must not
// displace the evidence that something failed.
func (p *PowerShell) explainTeamMembers(ctx context.Context, spec types.VirtualSwitchSpec, cause error) error {
	present, aerr := p.adaptersPresent(ctx)
	if aerr != nil || len(present) == 0 {
		return cause
	}
	have := make(map[string]bool, len(present))
	for _, a := range present {
		have[strings.ToLower(a.Name)] = true
	}
	var missing []string
	for _, m := range spec.TeamMembers {
		if !have[strings.ToLower(m)] {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return cause
	}

	var b strings.Builder
	b.WriteString("this host has no network adapter named ")
	b.WriteString(quoteList(missing))
	b.WriteString(", which the switch declares as a team member. A replaced or re-seated NIC comes back under a new name, so the switch is pointing at hardware that is no longer there. The adapters it does have are: ")
	for i, a := range present {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(a.Name)
		var notes []string
		if a.MAC != "" {
			notes = append(notes, a.MAC)
		}
		if a.Up {
			notes = append(notes, "up")
		} else {
			notes = append(notes, "down")
		}
		if a.InUseBy != "" {
			notes = append(notes, "already teamed in "+a.InUseBy)
		} else {
			notes = append(notes, "free")
		}
		if a.IPv4 != "" {
			notes = append(notes, "carries "+a.IPv4)
		}
		b.WriteString(" (" + strings.Join(notes, ", ") + ")")
	}
	b.WriteString(". Edit the switch and set its team members to the adapters that should carry it. Ballast will not choose for you: which fabric a NIC belongs to is not something its name says, and teaming the wrong one takes the host off the network.")
	// The cmdlet's own words last, for whoever is reading the log rather than the
	// console. The cause leads; the evidence follows.
	return fmt.Errorf("%s (%w)", b.String(), cause)
}

// quoteList renders names as "a", "b" and "c" — a list a person reads.
func quoteList(names []string) string {
	q := make([]string, 0, len(names))
	for _, n := range names {
		q = append(q, strconv.Quote(n))
	}
	switch len(q) {
	case 0:
		return ""
	case 1:
		return q[0]
	}
	return strings.Join(q[:len(q)-1], ", ") + " or " + q[len(q)-1]
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
	// TeamMember is the physical adapter this vNIC is pinned to inside the
	// switch's SET team, empty when SET is free to place it anywhere. Observed
	// because an unpinned storage vNIC is indistinguishable from a pinned one
	// everywhere else — same address, same session, same MPIO — right up to the
	// point where the one shared cable fails.
	TeamMember string `json:"teamMember"`
	// RDMA is whether RDMA is enabled on the vNIC's projected interface.
	// RDMAKnown is whether the interface exposes RDMA at all: an adapter with no
	// RDMA capability reports nothing, which is not the same as reporting off,
	// and treating it as off would make the reconcile try to enable it for ever.
	RDMA      bool `json:"rdma"`
	RDMAKnown bool `json:"rdmaKnown"`
	// Known is false when Hyper-V could not be read at all; see
	// ErrHyperVUnavailable. Exists is then meaningless.
	Known bool `json:"known"`
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
	// A pin that has drifted — or was never applied — is drift like any other.
	// Nothing above this layer can see it: the vNIC is up, addressed and
	// carrying traffic either way, and only a failed cable tells you which.
	if !strings.EqualFold(obs.TeamMember, spec.TeamMemberAdapter) {
		return vnicUpdate
	}
	// RDMA is compared only where the adapter can answer. An adapter with no
	// RDMA capability reports nothing; reading that as "off" would make every
	// pass try to enable it, report Updated, and change nothing — a reconcile
	// that never settles. That mismatch is reported instead; see rdmaRefused.
	if spec.RDMA != nil && obs.RDMAKnown && *spec.RDMA != obs.RDMA {
		return vnicUpdate
	}
	return vnicNoop
}

// rdmaRefused reports a spec that asks for RDMA on an interface that has none.
//
// Silence here would be the worse failure: the console would show RDMA declared
// and settled, and SMB Direct would simply never engage — storage that works,
// slowly, for reasons nothing on the host explains. It is not fatal to the vNIC,
// which is otherwise correct, so it is reported after the rest is applied.
func rdmaRefused(spec types.ManagementVNICSpec, obs vnicObservation) error {
	if spec.RDMA == nil || !*spec.RDMA || !obs.Exists || obs.RDMAKnown {
		return nil
	}
	return fmt.Errorf("vNIC %q asks for RDMA, but the interface %q reports no RDMA capability — "+
		"either the physical adapter it lands on does not support it, or its driver has RDMA disabled. "+
		"SMB Direct will not engage on this path; everything else about the vNIC is configured",
		spec.Name, "vEthernet ("+spec.Name+")")
}

func (p *PowerShell) queryVNIC(ctx context.Context, name string) (vnicObservation, error) {
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
# Enumerate without -Name so "no such vNIC" (empty list) separates from "Hyper-V
# did not answer" (throws). See ErrHyperVUnavailable.
try { $all = @(Get-VMNetworkAdapter -ManagementOS -ErrorAction Stop) }
catch { [pscustomobject]@{ exists = $false; known = $false } | ConvertTo-Json -Compress; return }
$a = @($all | Where-Object { $_.Name -eq %[1]s })[0]
if (-not $a) { [pscustomobject]@{ exists = $false; known = $true } | ConvertTo-Json -Compress; return }
$vid = 0
$v = Get-VMNetworkAdapterVlan -ManagementOS -VMNetworkAdapterName %[1]s -ErrorAction SilentlyContinue
if ($v -and $v.OperationMode -eq 'Access') { $vid = [int]$v.AccessVlanId }
$map = @(Get-VMNetworkAdapterTeamMapping -ManagementOS -VMNetworkAdapterName %[1]s -ErrorAction SilentlyContinue)[0]
$rdma = Get-NetAdapterRdma -Name ('vEthernet (' + %[1]s + ')') -ErrorAction SilentlyContinue
[pscustomobject]@{
  exists     = $true
  known      = $true
  switchName = [string]$a.SwitchName
  vlanID     = $vid
  teamMember = [string]$map.NetAdapterName
  rdma       = [bool]$rdma.Enabled
  rdmaKnown  = [bool]$rdma
} | ConvertTo-Json -Compress
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
	return p.ensureMgmtVNICFrom(ctx, spec, obs, nil)
}

// EnsureMgmtVNICs reconciles every management vNIC from ONE observation pass.
//
// The per-vNIC path costs two PowerShell invocations before it decides it has
// nothing to do — queryVNIC and queryVNICIP — and each pays a fresh module load
// (the IP query alone touches Hyper-V, NetTCPIP, NetRoute, DnsClient and
// FailoverClusters). Measured on the rig, that is ~7s per vNIC, and a three-vNIC
// converged host spent ~21s of a 40s host reconcile deciding nothing had
// changed.
//
// Observing all of them in one script pays that once. Applies are still made
// per vNIC, because they are rare, individually meaningful, and must be able to
// fail one at a time — a batched apply would turn one bad spec into three
// failures and make the condition report unattributable.
//
// Returns one Outcome per spec, in order, plus the first error encountered. A
// failure on one vNIC does not stop the others: they are independent, and
// stopping early would leave later ones unreconciled with nothing said about
// them.
func (p *PowerShell) EnsureMgmtVNICs(ctx context.Context, specs []types.ManagementVNICSpec) ([]Outcome, []error) {
	outs := make([]Outcome, len(specs))
	errs := make([]error, len(specs))
	if len(specs) == 0 {
		return outs, errs
	}
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	adapters, ips, err := p.queryVNICsBatch(ctx, names)
	if errors.Is(err, ErrHyperVUnavailable) {
		// Hyper-V is down, not merely unhelpful. The per-vNIC fallback would ask
		// the same dead service the same question three more times and reach the
		// same place slower, so report it once and defer the pass.
		for i, s := range specs {
			outs[i] = OutcomeUnchanged
			errs[i] = fmt.Errorf("vNIC %q: %w", s.Name, ErrHyperVUnavailable)
		}
		return outs, errs
	}
	if err != nil {
		// The batch is an optimisation, not a semantic change. If it fails —
		// an unexpected shape, a module missing on an odd host — fall back to
		// the per-vNIC path rather than failing a reconcile that would have
		// worked before.
		for i, s := range specs {
			outs[i], errs[i] = p.EnsureMgmtVNIC(ctx, s)
		}
		return outs, errs
	}
	for i, s := range specs {
		obs := adapters[strings.ToLower(s.Name)]
		ipObs, haveIP := ips[strings.ToLower(s.Name)]
		var ipPtr *ipObservation
		if haveIP {
			ipPtr = &ipObs
		}
		outs[i], errs[i] = p.ensureMgmtVNICFrom(ctx, s, obs, ipPtr)
	}
	return outs, errs
}

// ensureMgmtVNICFrom is the shared body: decide and apply, given observations
// someone else has already made. observedIP may be nil, in which case the IP is
// queried on demand (the single-vNIC path).
func (p *PowerShell) ensureMgmtVNICFrom(ctx context.Context, spec types.ManagementVNICSpec, obs vnicObservation, observedIP *ipObservation) (Outcome, error) {
	// Nothing observed, so nothing concluded — and above all no create. See
	// ErrHyperVUnavailable.
	if !obs.Known {
		return OutcomeUnchanged, fmt.Errorf("vNIC %q: %w", spec.Name, ErrHyperVUnavailable)
	}

	adapter := OutcomeUnchanged
	// Whether this pass moves the vNIC to a different switch, which is the one
	// update that destroys and recreates it. Decided from the observation rather
	// than from what the script did, because the script is where the guard lives
	// and this has to agree with it.
	movedSwitch := obs.Exists && obs.SwitchName != spec.SwitchName
	switch planVNIC(spec, obs) {
	case vnicCreate:
		if err := p.run2(ctx, createVNICScript(spec)); err != nil {
			return OutcomeUnchanged, fmt.Errorf("create vNIC %q: %w", spec.Name, err)
		}
		adapter = OutcomeCreated
	case vnicUpdate:
		if err := p.run2(ctx, updateVNICScript(spec, obs)); err != nil {
			return OutcomeUnchanged, fmt.Errorf("update vNIC %q: %w", spec.Name, err)
		}
		adapter = OutcomeUpdated
	}

	ipChanged := false
	if spec.IPConfig != nil {
		// A vNIC just created has no IP yet, and the batched observation was
		// taken before it existed — re-observe rather than trust it.
		//
		// The same is true of one that just MOVED switches: that move is a remove
		// and an add, so the address went with it. Trusting the batch here would
		// compare the desired address against a reading taken before the vNIC was
		// destroyed, find it matches, skip the apply, and leave the interface with
		// no address at all — on the management vNIC, that is the host.
		if adapter == OutcomeCreated || movedSwitch {
			observedIP = nil
		}
		changed, err := p.reconcileVNICIP(ctx, spec.Name, spec.IPConfig, observedIP)
		if err != nil {
			return adapter, fmt.Errorf("ensure vNIC %q IP: %w", spec.Name, err)
		}
		ipChanged = changed
	}

	outcome := OutcomeUnchanged
	switch {
	case adapter == OutcomeCreated:
		outcome = OutcomeCreated
	case adapter == OutcomeUpdated || ipChanged:
		outcome = OutcomeUpdated
	}
	// Reported last, and without undoing anything: the vNIC is configured, and
	// the one thing that cannot be honoured is named rather than left to be
	// discovered by a slow storage fabric.
	return outcome, rdmaRefused(spec, obs)
}

// reconcileVNICIP observes the vNIC's current IPv4 config and applies the
// desired one only if it differs. Returns whether it changed anything.
// observed, when non-nil, is an observation the caller has already made (the
// batched path); nil means query it here.
func (p *PowerShell) reconcileVNICIP(ctx context.Context, name string, desired *types.IPConfig, observed *ipObservation) (bool, error) {
	// Validate the desired address up front so we never run a destructive apply
	// with a malformed spec.
	ip, prefix, err := parseCIDR(desired.Address)
	if err != nil {
		return false, err
	}

	var obs ipObservation
	if observed != nil {
		obs = *observed
	} else {
		obs, err = p.queryVNICIP(ctx, name)
		if err != nil {
			return false, fmt.Errorf("query IP: %w", err)
		}
	}
	if !ipDiffers(desired, obs) {
		return false, nil
	}
	if err := p.run2(ctx, applyIPScript(name, ip, prefix, desired)); err != nil {
		return false, err
	}
	return true, nil
}

// queryVNICsBatch observes the adapter and IPv4 state of every named management
// vNIC in a single PowerShell invocation, keyed by lower-cased name.
//
// Everything expensive is hoisted out of the per-vNIC loop: the module loads
// happen once for the process, and the cluster IP-resource enumeration — which
// is per-CLUSTER, not per-vNIC, and was being repeated for each one — happens
// once for the script. Adapters absent from the host are simply missing from the
// map, which reads as vnicObservation{Exists:false} on lookup.
func (p *PowerShell) queryVNICsBatch(ctx context.Context, names []string) (map[string]vnicObservation, map[string]ipObservation, error) {
	out, err := p.run(ctx, vnicsBatchScript(names))
	if err != nil {
		return nil, nil, err
	}
	var got struct {
		Known    bool                       `json:"known"`
		Adapters map[string]vnicObservation `json:"adapters"`
		IPs      map[string]ipObservation   `json:"ips"`
	}
	if err := decodeJSON(out, &got); err != nil {
		return nil, nil, err
	}
	if !got.Known {
		return nil, nil, ErrHyperVUnavailable
	}
	if got.Adapters == nil {
		got.Adapters = map[string]vnicObservation{}
	}
	if got.IPs == nil {
		got.IPs = map[string]ipObservation{}
	}
	// A vNIC that does not exist is simply missing from the map, and a map miss
	// yields the zero observation — whose Known is false, which now means "not
	// observed". Fill the absences in explicitly: Hyper-V answered, so absence
	// here is a fact and creating is the right response. Leaving this to the zero
	// value would turn every legitimate create into a permanent deferral.
	for _, n := range names {
		k := strings.ToLower(n)
		if _, ok := got.Adapters[k]; !ok {
			got.Adapters[k] = vnicObservation{Exists: false, Known: true}
		}
	}
	return got.Adapters, got.IPs, nil
}

// vnicsBatchScript is built by a pure function so its content is pinned by tests
// without a host, like the maintenance and template scripts.
func vnicsBatchScript(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, psQuote(n))
	}
	return fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$names = @(%[1]s)
# One enumeration for every name, and the single place Hyper-V's availability is
# established. An empty list means the vNICs are absent; a throw means Hyper-V
# did not answer, and the caller must defer rather than create. See
# ErrHyperVUnavailable.
try { $allAdapters = @(Get-VMNetworkAdapter -ManagementOS -ErrorAction Stop) }
catch { [pscustomobject]@{ known = $false; adapters = @{}; ips = @{} } | ConvertTo-Json -Depth 6 -Compress; return }
# Cluster IP resources are a property of the CLUSTER, so read them once rather
# than once per vNIC. Excluded from the observation by ADDRESS: a cluster-owned
# IP on a management interface must never be mistaken for the declared one and
# reconciled away.
$clusterIps = @()
try { $clusterIps = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -like 'IP Address*' } | ForEach-Object { [string]($_ | Get-ClusterParameter -Name Address -ErrorAction SilentlyContinue).Value }) } catch {}
# Every network adapter, hidden ones included, enumerated ONCE. The per-name
# helper is deliberately not used here: it asks Hyper-V again for each vNIC, and
# this batch exists precisely to pay that cost once. Same resolution, hoisted.
$allNet = @()
try { $allNet = @(Get-NetAdapter -IncludeHidden -ErrorAction SilentlyContinue) } catch {}
function Resolve-BallastAlias($vnicObj, [string]$nm) {
  # Name first, MAC only to disambiguate. A management OS vNIC on a SET team
  # shares its MAC with a physical team member, so a MAC-first match resolves to
  # the physical NIC. See Get-BallastVNICAlias.
  $cands = @($allNet | Where-Object { $_.Name -eq ('vEthernet (' + $nm + ')') -or $_.Name -like ('vEthernet (' + $nm + ') *') })
  if ($cands.Count -eq 0) { return 'vEthernet (' + $nm + ')' }
  if ($cands.Count -eq 1) { return [string]$cands[0].Name }
  if ($vnicObj) {
    $mac = ([string]$vnicObj.MacAddress) -replace '[-:]',''
    if ($mac -and $mac -ne '000000000000') {
      $hit = @($cands | Where-Object { (([string]$_.MacAddress) -replace '[-:]','') -eq $mac })[0]
      if ($hit) { return [string]$hit.Name }
    }
  }
  $live = @($cands | Where-Object { [string]$_.Status -ne 'Disconnected' })[0]
  if ($live) { return [string]$live.Name }
  return [string]$cands[0].Name
}
$adapters = @{}
$ips = @{}
foreach ($n in $names) {
  $key = $n.ToLower()
  $a = @($allAdapters | Where-Object { $_.Name -eq $n })[0]
  if ($a) {
    $vid = 0
    $v = Get-VMNetworkAdapterVlan -ManagementOS -VMNetworkAdapterName $n -ErrorAction SilentlyContinue
    if ($v -and $v.OperationMode -eq 'Access') { $vid = [int]$v.AccessVlanId }
    # Pin and RDMA are read here rather than per vNIC: both cmdlets come from
    # modules this script has already paid to load, so the marginal cost is the
    # call itself.
    $map = @(Get-VMNetworkAdapterTeamMapping -ManagementOS -VMNetworkAdapterName $n -ErrorAction SilentlyContinue)[0]
    $rdma = Get-NetAdapterRdma -Name (Resolve-BallastAlias $a $n) -ErrorAction SilentlyContinue
    $adapters[$key] = [pscustomobject]@{
      exists     = $true
      known      = $true
      switchName = [string]$a.SwitchName
      vlanID     = $vid
      teamMember = [string]$map.NetAdapterName
      rdma       = [bool]$rdma.Enabled
      rdmaKnown  = [bool]$rdma
    }
  }
  $alias = Resolve-BallastAlias $a $n
  $ip = @(Get-NetIPAddress -InterfaceAlias $alias -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.PrefixOrigin -eq 'Manual' -and $_.IPAddress -notlike '169.254.*' -and ($clusterIps -notcontains $_.IPAddress) })[0]
  $gw = @(Get-NetRoute -InterfaceAlias $alias -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue)[0].NextHop
  $dns = @((Get-DnsClientServerAddress -InterfaceAlias $alias -AddressFamily IPv4 -ErrorAction SilentlyContinue).ServerAddresses)
  $reg = [bool](Get-DnsClient -InterfaceAlias $alias -ErrorAction SilentlyContinue).RegisterThisConnectionsAddress
  $ips[$key] = [pscustomobject]@{
    address    = if ($ip) { "$($ip.IPAddress)/$($ip.PrefixLength)" } else { '' }
    gateway    = if ($gw) { [string]$gw } else { '' }
    dnsServers = @($dns)
    registers  = $reg
  }
}
[pscustomobject]@{ known = $true; adapters = $adapters; ips = $ips } | ConvertTo-Json -Depth 6 -Compress
`, strings.Join(quoted, ","))
}

func (p *PowerShell) queryVNICIP(ctx context.Context, name string) (ipObservation, error) {
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'`+vnicAliasHelper+`
# Resolved, not constructed — and it must resolve the SAME way the apply does,
# or the reconcile reads one adapter and writes another and never settles.
$alias = Get-BallastVNICAlias '%[1]s'
$clusterIps = @(); try { $clusterIps = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -like 'IP Address*' } | ForEach-Object { [string]($_ | Get-ClusterParameter -Name Address -ErrorAction SilentlyContinue).Value }) } catch {}
$ip = @(Get-NetIPAddress -InterfaceAlias $alias -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.PrefixOrigin -eq 'Manual' -and $_.IPAddress -notlike '169.254.*' -and ($clusterIps -notcontains $_.IPAddress) })[0]
$gw = @(Get-NetRoute -InterfaceAlias $alias -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue)[0].NextHop
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
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	// The adapter is RESOLVED, never named by construction. See vnicAliasHelper:
	// a dead "vEthernet (X)" left behind by a removed vNIC accepts every call in
	// this script and changes nothing on the interface that matters.
	b.WriteString(vnicAliasHelper)
	fmt.Fprintf(&b, "$alias = Get-BallastVNICAlias %s\n", psQuote(name))
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
	// A GHOST adapter still owns the address. The removal above only reaches
	// addresses on adapters that still exist; a non-present device keeps its
	// static address in the registry alone, where Get-NetIPAddress cannot see
	// it — and New-NetIPAddress still refuses with "object already exists".
	//
	// This is what a NIC swap leaves behind. Changing a VM's adapter type (or
	// replacing a physical card) gives Windows new devices and ghosts the old
	// ones, which is also why the new adapters arrive named "Ethernet0 2".
	// Observed on the rig 2026-08-05: the fabric vNICs came back with no
	// address at all, the cluster's storage network partitioned, and the agent
	// reported "exit status 1:" with nothing after the colon.
	//
	// Only NON-PRESENT interfaces are touched, and only the address value that
	// actually collides — a live adapter's configuration is never altered here.
	b.WriteString(`
function Clear-BallastGhostAddress([string]$want) {
  $present = @(Get-NetAdapter -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.InterfaceGuid })
  $base = 'HKLM:\SYSTEM\CurrentControlSet\Services\Tcpip\Parameters\Interfaces'
  $freed = @()
  foreach ($k in @(Get-ChildItem $base -ErrorAction SilentlyContinue)) {
    if ($present -contains [string]$k.PSChildName) { continue }
    $props = Get-ItemProperty $k.PSPath -ErrorAction SilentlyContinue
    if ($props -and $props.IPAddress -and (@($props.IPAddress) -contains $want)) {
      Remove-ItemProperty -Path $k.PSPath -Name IPAddress -Force -ErrorAction SilentlyContinue
      Remove-ItemProperty -Path $k.PSPath -Name SubnetMask -Force -ErrorAction SilentlyContinue
      $freed += [string]$k.PSChildName
    }
  }
  return $freed
}
`)
	// DHCP off AGAIN, immediately before the address is applied and scoped to
	// IPv4.
	//
	// It is already switched off at the top of this script, and that was not
	// enough: removing the last manual address above lets the interface fall back
	// to DHCP, so by the time New-NetIPAddress runs the flag is on again and
	// Windows refuses with "Inconsistent parameters PolicyStore PersistentStore
	// and Dhcp Enabled" — a persistent static address cannot be added to a DHCP
	// interface. Observed on HVNEW02 2026-08-24, on the vNIC carrying the host's
	// management address.
	//
	// The earlier call also omitted -AddressFamily, so it targeted IPv6 as well,
	// where Dhcp is not the mechanism; scoping it here keeps the failure that
	// matters visible instead of mixed in with one that never applied.
	//
	// And it REPORTS ITSELF. Scoping the call to IPv4 was not enough: HVNEW02 on
	// 0.4.138 — which had that fix — went on failing with the same "Inconsistent
	// parameters PolicyStore PersistentStore and Dhcp Enabled". The disable runs
	// with -ErrorAction SilentlyContinue under an $ErrorActionPreference of Stop,
	// so if it fails, nothing anywhere says so and the operator is handed a
	// downstream complaint about the ADDRESS instead of the reason.
	//
	// So: let it throw, keep the reason, then read the state back rather than
	// assume the write took. Whatever the cause turns out to be, the next report
	// names it instead of pointing at New-NetIPAddress.
	b.WriteString(`$dhcpErr = ''
try { Set-NetIPInterface -InterfaceAlias $alias -AddressFamily IPv4 -Dhcp Disabled -ErrorAction Stop }
catch { $dhcpErr = ([string]$_.Exception.Message).Trim() }
$dhcpState = ''
try { $dhcpState = [string](Get-NetIPInterface -InterfaceAlias $alias -AddressFamily IPv4 -ErrorAction Stop).Dhcp } catch {}
# One retry against the ACTIVE store. A write that lands in PersistentStore only
# leaves the running interface still on DHCP, which is exactly the state
# New-NetIPAddress refuses — and the two stores disagreeing is the leading
# suspect for a disable that reports success and changes nothing.
if ($dhcpState -eq 'Enabled') {
  try { Set-NetIPInterface -InterfaceAlias $alias -AddressFamily IPv4 -Dhcp Disabled -PolicyStore ActiveStore -ErrorAction Stop } catch { $dhcpErr = ($dhcpErr + ' / ActiveStore: ' + ([string]$_.Exception.Message).Trim()).Trim(' /') }
  try { $dhcpState = [string](Get-NetIPInterface -InterfaceAlias $alias -AddressFamily IPv4 -ErrorAction Stop).Dhcp } catch {}
}
`)

	newIP := fmt.Sprintf("New-NetIPAddress -InterfaceAlias $alias -IPAddress %s -PrefixLength %d", psQuote(ip), prefix)
	if cfg.Gateway != "" {
		newIP += " -DefaultGateway " + psQuote(cfg.Gateway)
	}
	// Try, then free a ghost's claim and try once more. The catch also supplies
	// the error TEXT: relying on stderr produced a bare "exit status 1:" with
	// nothing after it, which told an operator only that something failed.
	fmt.Fprintf(&b, `
# What was actually observed about DHCP, carried into whatever fails below. The
# "Inconsistent parameters PolicyStore PersistentStore and Dhcp Enabled" message
# describes the ADDRESS call; the cause is here.
$dhcpNote = ''
if ($dhcpState -eq 'Enabled') {
  $dhcpNote = '. DHCP is still enabled on ' + $alias + ' — a persistent static address cannot be added to a DHCP interface'
  if ($dhcpErr) { $dhcpNote = $dhcpNote + ', and disabling it failed: ' + $dhcpErr }
  else { $dhcpNote = $dhcpNote + ', and disabling it reported success but did not take' }
} elseif ($dhcpErr) {
  $dhcpNote = '. Disabling DHCP on ' + $alias + ' reported: ' + $dhcpErr
}
try { %[1]s | Out-Null }
catch {
  $first = [string]$_.Exception.Message
  $freed = Clear-BallastGhostAddress %[2]s
  if ($freed.Count -gt 0) {
    try { %[1]s | Out-Null }
    catch { throw ('could not apply ' + %[2]s + ' to ' + $alias + ' even after releasing it from removed adapter(s) ' + ($freed -join ', ') + ': ' + [string]$_.Exception.Message + $dhcpNote) }
  } else {
    throw ('could not apply ' + %[2]s + ' to ' + $alias + ': ' + $first + $dhcpNote)
  }
}
`, newIP, psQuote(ip))
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
	b.WriteString(affinityCommand(spec))
	b.WriteString(rdmaCommand(spec))
	return b.String()
}

// updateVNICScript changes ONLY what has drifted.
//
// It used to rewrite every setting on every update. That was wasteful before the
// pin existed and harmful once it did: changing a pin re-applied the VLAN, and
// Set-VMNetworkAdapterVlan threw "Object reference not set to an instance of an
// object" on HVNEW02's storage vNICs on 2026-08-24 — a vNIC whose VLAN was
// already correct, failed by a call that had no reason to run. Making the pin
// reconcilable is what made that path reachable.
//
// Beyond avoiding the fault, it is the right shape anyway: an update that
// rewrites everything to change one field turns a pin adjustment into a VLAN
// change and a QoS change on a live interface.
func updateVNICScript(spec types.ManagementVNICSpec, obs vnicObservation) string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	// Moving a management vNIC between switches, and ONLY when it has to move.
	//
	// This used to run unconditionally on every update, and it could not work at
	// all: Get-VMNetworkAdapter -ManagementOS returns a VMInternalNetworkAdapter,
	// and Connect-VMNetworkAdapter -VMNetworkAdapter is typed to VMNetworkAdapter,
	// so the bind failed with "Cannot convert ... VMInternalNetworkAdapter". It
	// was invisible while the only updates were creates; making the pin and RDMA
	// reconcilable meant an already-correct vNIC could need an update, and every
	// one of those failed. Observed on HVNEW02 2026-08-24 for Storage01 and
	// Storage02, whose switch was right and whose pin was not.
	//
	// Two changes. It is guarded on the switch actually differing — reconnecting
	// a management vNIC drops its network, and doing that to adjust a VLAN or a
	// pin is damage for nothing. And the move is remove-then-add, which is what
	// moving a management OS vNIC between switches actually is; the comment here
	// always said so.
	//
	// The guard against stranding the host is the important part: the target
	// switch must already exist before the vNIC is removed, or a typo takes the
	// management address away with nothing to put it back on. The IP is
	// reapplied by the step that follows, which re-observes rather than trusting
	// the batch taken before the move.
	fmt.Fprintf(&b, `$a = @(Get-VMNetworkAdapter -ManagementOS -Name %[1]s -ErrorAction SilentlyContinue)[0]
if ($a -and [string]$a.SwitchName -ne %[2]s) {
  if (-not (Get-VMSwitch -Name %[2]s -ErrorAction SilentlyContinue)) {
    throw ('cannot move vNIC ' + %[1]s + ' to switch ' + %[2]s + ': that switch does not exist on this host, and removing the vNIC first would leave nothing to re-add it to')
  }
  Remove-VMNetworkAdapter -ManagementOS -Name %[1]s
  Add-VMNetworkAdapter -ManagementOS -Name %[1]s -SwitchName %[2]s | Out-Null
}
`, psQuote(spec.Name), psQuote(spec.SwitchName))
	if obs.VlanID != spec.VLANID {
		b.WriteString(vlanCommand(spec))
	}
	// The weight is not observed, so it is applied whenever anything else is —
	// there is no reading to compare against, and claiming it matches would be
	// the absent-is-not-zero trap in miniature.
	b.WriteString(weightCommand(spec))
	if !strings.EqualFold(obs.TeamMember, spec.TeamMemberAdapter) {
		b.WriteString(affinityCommand(spec))
	}
	if spec.RDMA != nil && obs.RDMAKnown && *spec.RDMA != obs.RDMA {
		b.WriteString(rdmaCommand(spec))
	}
	return b.String()
}

// affinityCommand pins a vNIC to ONE physical adapter in the switch's SET team.
//
// This is what makes a second storage path a second path. Without it SET places
// each vNIC on whichever uplink it likes, so two storage vNICs on two subnets —
// with MPIO or SMB Multichannel dutifully running across both — can share a
// single physical port. One cable then takes the lot, and every layer above goes
// on reporting redundancy. Seen 2026-08-24: three hosts, three declared portals
// each, one path each, MPIO "in effect" with nothing to fail over to.
//
// Idempotent: the mapping is removed and re-added, because Set-VMNetworkAdapter-
// TeamMapping cannot move an existing mapping to a different adapter and errors
// if one is already present. Removing a mapping does not disturb traffic — it
// returns placement to SET's own choice for the moment in between.
//
// An empty adapter clears any mapping rather than leaving a stale one: a vNIC
// whose pin was deliberately removed must actually come unpinned, or the spec
// and the host disagree for ever.
func affinityCommand(spec types.ManagementVNICSpec) string {
	name := psQuote(spec.Name)
	if spec.TeamMemberAdapter == "" {
		return fmt.Sprintf("Remove-VMNetworkAdapterTeamMapping -ManagementOS -VMNetworkAdapterName %s -ErrorAction SilentlyContinue | Out-Null\n", name)
	}
	return fmt.Sprintf(`Remove-VMNetworkAdapterTeamMapping -ManagementOS -VMNetworkAdapterName %[1]s -ErrorAction SilentlyContinue | Out-Null
Set-VMNetworkAdapterTeamMapping -ManagementOS -VMNetworkAdapterName %[1]s -PhysicalNetAdapterName %[2]s | Out-Null
`, name, psQuote(spec.TeamMemberAdapter))
}

// rdmaCommand turns RDMA on or off for a vNIC.
//
// Only when the spec has an opinion. Nil means nobody has decided, and enabling
// RDMA on adapters that cannot do it — or, for RoCE, without DCB configured on
// the switches Ballast does not administer — produces a fabric that works until
// it is loaded and then does not. So it is declared, never inferred.
func rdmaCommand(spec types.ManagementVNICSpec) string {
	if spec.RDMA == nil {
		return ""
	}
	// The management OS vNIC appears to the network stack as "vEthernet (name)".
	iface := psQuote("vEthernet (" + spec.Name + ")")
	if !*spec.RDMA {
		return fmt.Sprintf("Disable-NetAdapterRdma -Name %s -ErrorAction SilentlyContinue | Out-Null\n", iface)
	}
	return fmt.Sprintf("Enable-NetAdapterRdma -Name %s -ErrorAction SilentlyContinue | Out-Null\n", iface)
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
func (p *PowerShell) PruneManagementVNICs(ctx context.Context, switches, keep []string) (Outcome, []string, error) {
	if len(switches) == 0 {
		return OutcomeUnchanged, nil, nil
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

# DUPLICATES of a DECLARED vNIC, which the loop above can never see.
#
# Hyper-V allows two management OS vNICs with the same name on one switch. Only
# the network adapter ALIAS is disambiguated, to "vEthernet (Name) 2" — the vNIC
# name stays identical. So "$keep -contains $nm" matches the duplicate too and
# skips it as declared, and every alias this file builds by concatenation
# addresses the FIRST one, whichever that happens to be.
#
# HVNEW02, 2026-08-24: "vEthernet (ConvergedSwitch) 2" on adapter #2 holding a
# DHCP lease of 192.168.1.177, while the declared vNIC held 192.168.1.72. The
# cluster used the .177 interface for Cluster Network 1, Ballast reported the
# host settled, and nothing anywhere mentioned there were two.
#
# Reported, never auto-removed. Both carry a real address, and choosing which to
# delete on a live cluster member is a decision — deleting the one the cluster is
# actually using takes the node off its heartbeat network.
$dups = @()
foreach ($g in @($all | Group-Object { [string]$_.SwitchName + '/' + [string]$_.Name })) {
  if ($g.Count -le 1) { continue }
  $first = $g.Group[0]
  if ($switches -notcontains [string]$first.SwitchName) { continue }
  # The real adapter aliases, read rather than constructed, so the report names
  # what an operator will actually find on the host.
  $aliases = @()
  foreach ($d in @($g.Group)) {
    $na = Get-NetAdapter -InterfaceDescription ([string]$d.DeviceId) -ErrorAction SilentlyContinue
    if (-not $na) { $na = @(Get-NetAdapter -ErrorAction SilentlyContinue | Where-Object { $_.Name -like ('vEthernet (' + [string]$d.Name + ')*') }) }
    foreach ($x in @($na)) {
      $ip = @(Get-NetIPAddress -InterfaceAlias ([string]$x.Name) -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -notlike '169.254.*' })[0]
      $aliases += ([string]$x.Name + $(if ($ip) { ' = ' + [string]$ip.IPAddress } else { ' = no address' }))
    }
  }
  $dups += ([string]$first.Name + ' on ' + [string]$first.SwitchName + ': ' + (($aliases | Sort-Object -Unique) -join '; '))
}
$res = [ordered]@{ removed = $removed; duplicates = $dups }
'RESULT=' + ($res | ConvertTo-Json -Compress -Depth 3)`,
		psStringList(switches), psStringList(keep))
	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, nil, fmt.Errorf("prune management vNICs: %w", err)
	}
	var res struct {
		Removed    []string `json:"removed"`
		Duplicates []string `json:"duplicates"`
	}
	if derr := json.Unmarshal([]byte(resultJSON(string(out))), &res); derr != nil {
		return OutcomeUnchanged, nil, fmt.Errorf("prune management vNICs: could not read the result: %w", derr)
	}
	if len(res.Removed) > 0 {
		return OutcomeUpdated, res.Duplicates, nil
	}
	return OutcomeUnchanged, res.Duplicates, nil
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

// vnicAliasHelper is the PowerShell that resolves a management OS vNIC's REAL
// network adapter name, emitted into every script that needs one.
//
// The alias is NOT reliably "vEthernet (<name>)". Windows appends " 2" when a
// device already holds that name, and a vNIC that was removed and re-added
// leaves the old device behind — present, Disconnected, and still owning the
// original alias. Constructing the name then addresses the corpse.
//
// HVNEW02, 2026-08-24:
//
//	vEthernet (ConvergedSwitch)     Hyper-V Virtual Ethernet Adapter     Disconnected  5
//	vEthernet (ConvergedSwitch) 2   Hyper-V Virtual Ethernet Adapter #2  Up           24
//
// Ballast disabled DHCP on ifIndex 5, read DHCP back from ifIndex 5, and applied
// the address to ifIndex 5 — all of it on a dead adapter that reported success
// and changed nothing, while the live vNIC sat on a DHCP lease of 192.168.1.177.
// The whole "disabling it reported success but did not take" mystery was this.
//
// Resolved by MAC, which belongs to the vNIC itself and is unique. The name
// fallback prefers a connected adapter over a disconnected one, so even without
// a MAC the dead device is the last thing chosen rather than the first.
const vnicAliasHelper = `
function Get-BallastVNICAlias([string]$vnic) {
  # Candidates come from the NAME, always. The MAC only chooses between them.
  #
  # Matching on MAC first was wrong and dangerous: a management OS vNIC on a SET
  # team shares its MAC with a physical team member, so resolving
  # "ConvergedSwitch" found Ethernet1 — a teamed physical NIC — and the apply
  # then tried to remove addresses from it and put the host management address
  # on it. It failed only because a teamed adapter has no IPv4 interface.
  # HVNEW02, 2026-08-24.
  $cands = @(Get-NetAdapter -IncludeHidden -ErrorAction SilentlyContinue | Where-Object {
    $_.Name -eq ('vEthernet (' + $vnic + ')') -or $_.Name -like ('vEthernet (' + $vnic + ') *')
  })
  if ($cands.Count -eq 0) { return 'vEthernet (' + $vnic + ')' }
  if ($cands.Count -eq 1) { return [string]$cands[0].Name }
  # More than one: a vNIC removed and re-added leaves the old device present and
  # Disconnected, still holding the canonical name, while the live one becomes
  # "vEthernet (<name>) 2". The vNIC's own MAC says which is which.
  $vn = @(Get-VMNetworkAdapter -ManagementOS -Name $vnic -ErrorAction SilentlyContinue)[0]
  if ($vn) {
    $mac = ([string]$vn.MacAddress) -replace '[-:]',''
    if ($mac -and $mac -ne '000000000000') {
      $hit = @($cands | Where-Object { (([string]$_.MacAddress) -replace '[-:]','') -eq $mac })[0]
      if ($hit) { return [string]$hit.Name }
    }
  }
  $live = @($cands | Where-Object { [string]$_.Status -ne 'Disconnected' })[0]
  if ($live) { return [string]$live.Name }
  return [string]$cands[0].Name
}
`
