package hyperv

import (
	"context"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* Updating a management vNIC that already exists.

   The update script reconnected the vNIC on EVERY update, with a call that
   could never work: Get-VMNetworkAdapter -ManagementOS returns a
   VMInternalNetworkAdapter, and Connect-VMNetworkAdapter -VMNetworkAdapter is
   typed to VMNetworkAdapter, so the bind failed outright.

   It stayed invisible while the only updates were creates. Making the pin and
   RDMA reconcilable meant an already-correct vNIC could need an update, and
   every one of those failed — HVNEW02, 2026-08-24, Storage01 and Storage02,
   whose switch was right and whose pin was not. */

func mgmtSpec(name, sw string) types.ManagementVNICSpec {
	return types.ManagementVNICSpec{Name: name, SwitchName: sw}
}

func TestAnUpdateDoesNotReconnectAVNICThatIsAlreadyOnItsSwitch(t *testing.T) {
	got := updateVNICScript(mgmtSpec("Storage01", "ConvergedSwitch"), vnicObservation{Exists: true, Known: true, SwitchName: "ConvergedSwitch"})

	// The call that could not bind must be gone entirely.
	if strings.Contains(got, "Connect-VMNetworkAdapter") {
		t.Fatalf("Connect-VMNetworkAdapter cannot take a management OS vNIC: %s", got)
	}
	// And the move must be guarded on the switch actually differing.
	// Reconnecting a management vNIC drops its network, so doing it to adjust a
	// VLAN or a pin is damage for nothing.
	if !strings.Contains(got, "if ($a -and [string]$a.SwitchName -ne 'ConvergedSwitch')") {
		t.Errorf("the move must be conditional on the switch differing: %s", got)
	}
}

// Moving a management OS vNIC between switches IS a remove and an add. The old
// comment said so while the code tried to do it in place.
func TestMovingASwitchRemovesAndReadds(t *testing.T) {
	got := updateVNICScript(mgmtSpec("Mgmt", "NewSwitch"), vnicObservation{Exists: true, Known: true, SwitchName: "Old"})
	rm := strings.Index(got, "Remove-VMNetworkAdapter -ManagementOS -Name 'Mgmt'")
	add := strings.Index(got, "Add-VMNetworkAdapter -ManagementOS -Name 'Mgmt' -SwitchName 'NewSwitch'")
	if rm < 0 || add < 0 {
		t.Fatalf("a switch move must remove then re-add: %s", got)
	}
	if rm > add {
		t.Error("the remove must come first, or the add collides with the existing vNIC")
	}
}

// The guard that matters most. A typo in a switch name must not take the
// management address away with nothing to put it back on.
func TestAMoveRefusesWhenTheTargetSwitchIsMissing(t *testing.T) {
	got := updateVNICScript(mgmtSpec("Mgmt", "Typo"), vnicObservation{Exists: true, Known: true, SwitchName: "Old"})
	check := strings.Index(got, "if (-not (Get-VMSwitch -Name 'Typo'")
	rm := strings.Index(got, "Remove-VMNetworkAdapter")
	if check < 0 {
		t.Fatalf("the target switch must be checked to exist: %s", got)
	}
	if check > rm {
		t.Error("the check must happen BEFORE the removal, or the refusal comes too late to help")
	}
	if !strings.Contains(got, "would leave nothing to re-add it to") {
		t.Error("the refusal must say what it is protecting against")
	}
}

// Everything that HAS drifted is still applied. The selectivity below is about
// leaving settled fields alone, never about skipping work that is owed.
func TestAnUpdateStillAppliesVlanPinAndRDMA(t *testing.T) {
	on := true
	spec := mgmtSpec("Storage01", "ConvergedSwitch")
	spec.VLANID = 41
	spec.TeamMemberAdapter = "Ethernet1"
	spec.RDMA = &on

	// Every field differs from what the host reports. RDMAKnown is true because
	// an adapter that cannot report RDMA is deliberately not reconciled — see
	// rdmaRefused — so a drift test has to use one that can.
	drifted := vnicObservation{
		Exists: true, Known: true, SwitchName: "ConvergedSwitch",
		VlanID: 0, TeamMember: "", RDMA: false, RDMAKnown: true,
	}
	got := updateVNICScript(spec, drifted)
	for _, want := range []string{
		"Set-VMNetworkAdapterVlan -ManagementOS -VMNetworkAdapterName 'Storage01' -Access -VlanId 41",
		"Set-VMNetworkAdapterTeamMapping -ManagementOS -VMNetworkAdapterName 'Storage01' -PhysicalNetAdapterName 'Ethernet1'",
		"Enable-NetAdapterRdma -Name 'vEthernet (Storage01)'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("update must still apply %q:\n%s", want, got)
		}
	}
}

/*
Applying a static address to an interface that fell back to DHCP.

	"Inconsistent parameters PolicyStore PersistentStore and Dhcp Enabled" is
	Windows refusing a persistent static address on a DHCP interface. DHCP was
	already switched off at the top of the script — and then the address removals
	below it let the interface fall back, so by the time New-NetIPAddress ran the
	flag was on again. HVNEW02, 2026-08-24, on the vNIC carrying the host's
	management address.
*/
func TestDHCPIsSwitchedOffImmediatelyBeforeTheAddressIsApplied(t *testing.T) {
	got := applyIPScript("ConvergedSwitch", "192.168.1.72", 24, &types.IPConfig{Address: "192.168.1.72/24", Gateway: "192.168.1.1"})

	scoped := strings.Index(got, "Set-NetIPInterface -InterfaceAlias $alias -AddressFamily IPv4 -Dhcp Disabled")
	if scoped < 0 {
		t.Fatalf("DHCP must be switched off for IPv4 specifically: %s", got)
	}
	newIP := strings.Index(got, "New-NetIPAddress")
	if scoped > newIP {
		t.Fatal("switching DHCP off after applying the address is too late — that is the failure")
	}
	// And it must come after the removals, which are what re-enable it.
	removals := strings.LastIndex(got[:newIP], "Remove-NetIPAddress")
	if removals > 0 && scoped < removals {
		t.Error("the address removals re-enable DHCP, so the disable must come after them")
	}
}

/*
After a switch move the vNIC has been destroyed and recreated, so its address

	is gone. The batched IP observation was taken BEFORE that, and trusting it
	would compare the desired address against a reading of an interface that no
	longer exists, find a match, skip the apply, and leave the vNIC with no
	address at all. On the management vNIC, that is the host.
*/
func TestAMovedVNICReappliesItsAddress(t *testing.T) {
	desired := mgmtSpec("Mgmt", "NewSwitch")
	desired.IPConfig = &types.IPConfig{Address: "192.168.1.72/24", Gateway: "192.168.1.1"}

	// The pre-move reading MATCHES the desired address, which is exactly what
	// makes the stale-observation trap dangerous: nothing looks wrong.
	stale := &ipObservation{Address: "192.168.1.72/24", Gateway: "192.168.1.1"}

	run := func(obs vnicObservation, observed *ipObservation) []string {
		var scripts []string
		var p PowerShell
		p.run = func(_ context.Context, script string) ([]byte, error) {
			scripts = append(scripts, script)
			// Whatever is asked of the host after the move: nothing is there.
			return []byte(`{"address":"","gateway":"","dnsServers":[],"registers":false}`), nil
		}
		_, _ = p.ensureMgmtVNICFrom(context.Background(), desired, obs, observed)
		return scripts
	}

	applied := func(scripts []string) bool {
		for _, s := range scripts {
			if strings.Contains(s, "New-NetIPAddress") {
				return true
			}
		}
		return false
	}

	moved := vnicObservation{Exists: true, Known: true, SwitchName: "OldSwitch"}
	if !applied(run(moved, stale)) {
		t.Fatal("a vNIC that moved switches must reapply its address, not trust the pre-move reading")
	}

	// And a vNIC that did NOT move still trusts the batch, or the optimisation
	// the batch exists for is thrown away on every pass.
	settled := vnicObservation{Exists: true, Known: true, SwitchName: "NewSwitch"}
	if applied(run(settled, stale)) {
		t.Error("an unmoved vNIC whose address already matches must not be rewritten")
	}
}

/*
An update touches only what drifted.

	It used to rewrite every setting every time. Harmless-looking until the pin
	became reconcilable, at which point changing a pin re-applied the VLAN — and
	Set-VMNetworkAdapterVlan threw "Object reference not set to an instance of an
	object" on HVNEW02's storage vNICs on 2026-08-24. A vNIC whose VLAN was
	already correct, failed by a call with no reason to run.
*/
func TestAnUpdateTouchesOnlyWhatDrifted(t *testing.T) {
	on := true
	spec := mgmtSpec("Storage01", "ConvergedSwitch")
	spec.VLANID = 41
	spec.TeamMemberAdapter = "Ethernet1"
	spec.RDMA = &on

	// Only the pin differs.
	settledExceptPin := vnicObservation{
		Exists: true, Known: true, SwitchName: "ConvergedSwitch",
		VlanID: 41, TeamMember: "Ethernet0", RDMA: true, RDMAKnown: true,
	}
	got := updateVNICScript(spec, settledExceptPin)
	if strings.Contains(got, "Set-VMNetworkAdapterVlan") {
		t.Error("a VLAN that already matches must not be rewritten — that is the call that threw")
	}
	if strings.Contains(got, "NetAdapterRdma") {
		t.Error("RDMA that already matches must not be rewritten")
	}
	if !strings.Contains(got, "-PhysicalNetAdapterName 'Ethernet1'") {
		t.Fatalf("the pin that DID drift must still be applied:\n%s", got)
	}

	// Only the VLAN differs.
	vlanDrift := settledExceptPin
	vlanDrift.TeamMember = "Ethernet1"
	vlanDrift.VlanID = 7
	got = updateVNICScript(spec, vlanDrift)
	if !strings.Contains(got, "-Access -VlanId 41") {
		t.Error("a VLAN that drifted must be applied")
	}
	if strings.Contains(got, "TeamMapping") {
		t.Error("a pin that already matches must not be re-applied — remapping moves live traffic")
	}
}

// A vNIC being CREATED has no prior state, so everything is applied. Getting
// this backwards would leave a new storage vNIC unpinned and untagged.
func TestACreateAppliesEverythingUnconditionally(t *testing.T) {
	on := true
	spec := mgmtSpec("Storage01", "ConvergedSwitch")
	spec.VLANID = 41
	spec.TeamMemberAdapter = "Ethernet1"
	spec.RDMA = &on

	got := createVNICScript(spec)
	for _, want := range []string{"Set-VMNetworkAdapterVlan", "-PhysicalNetAdapterName 'Ethernet1'", "Enable-NetAdapterRdma"} {
		if !strings.Contains(got, want) {
			t.Errorf("a new vNIC must have %q applied:\n%s", want, got)
		}
	}
}

/*
The netsh fallback is GONE, and must not come back.

	It was added when a DHCP disable "reported success but did not take" — which
	turned out to be the alias bug: Ballast was disabling DHCP on a dead adapter
	and reading it back from the same dead adapter. Resolving the alias properly
	fixed the cause, leaving the fallback fixing nothing.

	And it was dangerous. "netsh interface ipv4 set address ... source=static"
	REPLACES every address on the interface, not only the one it was asked about.
	On a management vNIC that also carries the cluster IP, it takes the cluster IP
	with it. HVNEW03 went off the network on 2026-08-24 with this in the agent.

	A repair that can silently remove an address nobody asked it to touch is not
	a repair.
*/
func TestThereIsNoNetshFallback(t *testing.T) {
	for _, got := range []string{
		applyIPScript("ConvergedSwitch", "192.168.1.72", 24, &types.IPConfig{Address: "192.168.1.72/24", Gateway: "192.168.1.1"}),
		applyIPScript("Storage01", "10.0.60.72", 24, &types.IPConfig{Address: "10.0.60.72/24"}),
	} {
		if strings.Contains(got, "netsh") {
			t.Fatalf("netsh replaces every address on the interface, including the cluster IP:\n%s", got)
		}
		if strings.Contains(got, "source=static") {
			t.Fatal("no netsh address assignment may be emitted")
		}
	}
}

/*
The DHCP reading is KEPT: it is the diagnosis, and removing the fallback must

	not take away the thing that told us what was wrong.
*/
func TestTheDHCPStateIsStillReadBackAndReported(t *testing.T) {
	got := applyIPScript("ConvergedSwitch", "192.168.1.72", 24, &types.IPConfig{Address: "192.168.1.72/24"})
	if !strings.Contains(got, "Get-NetIPInterface -InterfaceAlias $alias -AddressFamily IPv4") {
		t.Fatalf("the DHCP state must still be read back rather than assumed:\n%s", got)
	}
	if !strings.Contains(got, "reported success but did not take") {
		t.Error("a disable that silently fails must still say so")
	}
	if !strings.Contains(got, "$dhcpNote") {
		t.Error("the reading must still reach the operator's message")
	}
}

/*
A duplicate management vNIC is invisible to everything else.

	Hyper-V allows two management OS vNICs with the same NAME on one switch. Only
	the network adapter alias is disambiguated — "vEthernet (Name) 2" — so the
	prune skips the duplicate (its name is in the keep set) and every alias built
	by string concatenation addresses whichever one Windows lists first.

	HVNEW02, 2026-08-24: a second "ConvergedSwitch" vNIC on adapter #2 holding a
	DHCP lease of 192.168.1.177 while the declared one held .72. Failover Cluster
	Manager used the .177 interface for Cluster Network 1. Ballast reported the
	host settled and said nothing about there being two.
*/
func TestDuplicateManagementVNICsAreFoundAndReported(t *testing.T) {
	var p PowerShell
	var seen string
	p.run = func(_ context.Context, script string) ([]byte, error) {
		seen = script
		return []byte(`RESULT={"removed":[],"duplicates":["ConvergedSwitch on ConvergedSwitch: vEthernet (ConvergedSwitch) = 192.168.1.72; vEthernet (ConvergedSwitch) 2 = 192.168.1.177"]}`), nil
	}
	out, dups, err := p.PruneManagementVNICs(context.Background(), []string{"ConvergedSwitch"}, []string{"ConvergedSwitch", "Storage01"})
	if err != nil {
		t.Fatal(err)
	}

	// Grouped by switch AND name, which is the only way to see a pair the keep
	// set hides.
	if !strings.Contains(seen, "Group-Object { [string]$_.SwitchName + '/' + [string]$_.Name }") {
		t.Fatalf("duplicates must be found by grouping on switch and name:\n%s", seen)
	}
	// The alias is READ from the adapter, not built from the vNIC name — building
	// it is what made the duplicate unaddressable in the first place.
	if !strings.Contains(seen, "Get-NetAdapter -InterfaceDescription") {
		t.Error("the real adapter alias must be read, not constructed")
	}
	if len(dups) != 1 || !strings.Contains(dups[0], "192.168.1.177") {
		t.Fatalf("the duplicate and its address must be reported: %v", dups)
	}
	// Finding a duplicate is not a change to the host.
	if out != OutcomeUnchanged {
		t.Error("reporting a duplicate must not claim the host was modified")
	}
}

// Never removed automatically. Both carry a real address, and deleting the one
// the cluster is actually using takes the node off its heartbeat network.
func TestADuplicateIsReportedNotRemoved(t *testing.T) {
	var p PowerShell
	var seen string
	p.run = func(_ context.Context, script string) ([]byte, error) {
		seen = script
		return []byte(`RESULT={"removed":[],"duplicates":[]}`), nil
	}
	_, _, _ = p.PruneManagementVNICs(context.Background(), []string{"ConvergedSwitch"}, []string{"ConvergedSwitch"})

	// The only removal in the script is the stray-vNIC one, which is guarded on
	// the name NOT being declared and on there being no address.
	dupIdx := strings.Index(seen, "$dups = @()")
	if dupIdx < 0 {
		t.Fatal("no duplicate detection in the script")
	}
	if strings.Contains(seen[dupIdx:], "Remove-VMNetworkAdapter") {
		t.Error("the duplicate branch must not remove anything")
	}
}

/*
The adapter is RESOLVED, never named by construction.

	"vEthernet (<name>)" is not reliable. A vNIC removed and re-added leaves the
	old device present and Disconnected, still holding the original alias, and
	the live one becomes "vEthernet (<name>) 2". HVNEW02, 2026-08-24:

	  vEthernet (ConvergedSwitch)     ... Disconnected  ifIndex 5
	  vEthernet (ConvergedSwitch) 2   ... Up            ifIndex 24

	Ballast disabled DHCP on ifIndex 5, read DHCP back from ifIndex 5, and applied
	the address to ifIndex 5 — every call succeeding against a dead adapter while
	the live vNIC sat on a DHCP lease of 192.168.1.177. That is the whole of
	"disabling it reported success but did not take".
*/
func TestTheAdapterAliasIsResolvedNotConstructed(t *testing.T) {
	apply := applyIPScript("ConvergedSwitch", "192.168.1.72", 24, &types.IPConfig{Address: "192.168.1.72/24"})
	if strings.Contains(apply, "$alias = 'vEthernet (ConvergedSwitch)'") {
		t.Fatal("the apply must not name the adapter by construction — that is how it wrote to a dead one")
	}
	if !strings.Contains(apply, "$alias = Get-BallastVNICAlias 'ConvergedSwitch'") {
		t.Fatalf("the apply must resolve the adapter:\n%s", apply)
	}

	// The observation must resolve the SAME way, or the reconcile reads one
	// adapter and writes another and never settles.
	var p PowerShell
	var seen string
	p.run = func(_ context.Context, script string) ([]byte, error) {
		seen = script
		return []byte(`{"address":"","gateway":"","dnsServers":[],"registers":false}`), nil
	}
	_, _ = p.queryVNICIP(context.Background(), "ConvergedSwitch")
	if !strings.Contains(seen, "$alias = Get-BallastVNICAlias 'ConvergedSwitch'") {
		t.Errorf("the IP observation must resolve the adapter too:\n%s", seen)
	}
}

/*
Candidates come from the NAME. The MAC only chooses BETWEEN them.

	Matching on MAC first resolved "ConvergedSwitch" to "Ethernet1" — a physical
	SET team member — because a management OS vNIC on a SET team shares its MAC
	with one. The apply then tried to strip addresses from a teamed physical NIC
	and put the host's management address on it, and failed only because a teamed
	adapter has no IPv4 interface at all. HVNEW02, 2026-08-24.

	The previous test asserted the MAC line EXISTED. It never asked what the MAC
	was being matched against, so it passed throughout.
*/
func TestTheResolverNeverLeavesTheVNICNameFamily(t *testing.T) {
	apply := applyIPScript("X", "10.0.0.5", 24, &types.IPConfig{Address: "10.0.0.5/24"})

	// Candidates are selected out of ALL adapters by NAME.
	if !strings.Contains(apply, "$cands = @(Get-NetAdapter -IncludeHidden") {
		t.Fatalf("candidates must come from the full adapter list:\n%s", apply)
	}
	if !strings.Contains(apply, "$_.Name -like ('vEthernet (' + $vnic + ') *')") {
		t.Fatalf("candidates must be selected by name:\n%s", apply)
	}
	// And the MAC narrows THAT set, never the whole list. This is the assertion
	// the old test was missing: it checked the MAC line existed, never what it
	// was matched against.
	if !strings.Contains(apply, "$hit = @($cands | Where-Object { (([string]$_.MacAddress) -replace '[-:]','') -eq $mac })[0]") {
		t.Fatalf("the MAC must disambiguate candidates, not search every adapter:\n%s", apply)
	}
	if strings.Contains(apply, "$na = @(Get-NetAdapter -IncludeHidden -ErrorAction SilentlyContinue | Where-Object { (([string]$_.MacAddress)") {
		t.Error("matching MAC against every adapter is how a physical NIC got selected")
	}
	// One candidate needs no MAC lookup at all — the common case must not pay for
	// a Hyper-V query.
	if !strings.Contains(apply, "if ($cands.Count -eq 1) { return [string]$cands[0].Name }") {
		t.Error("a single candidate must short-circuit before querying Hyper-V")
	}
	if !strings.Contains(apply, "Where-Object { [string]$_.Status -ne 'Disconnected' }") {
		t.Error("with no MAC to go on, a connected adapter must beat a dead one")
	}
	if !strings.Contains(apply, "Get-NetAdapter -IncludeHidden") {
		t.Error("hidden adapters must be visible, or a ghost holding the name is invisible")
	}
}

// The batched resolver carries the same rule, and had the same flaw.
func TestTheBatchResolverAlsoStaysInTheNameFamily(t *testing.T) {
	s := vnicsBatchScript([]string{"Mgmt"})
	if !strings.Contains(s, "$cands = @($allNet | Where-Object { $_.Name -eq ('vEthernet (' + $nm + ')') -or $_.Name -like ('vEthernet (' + $nm + ') *') })") {
		t.Fatalf("the batch must select candidates by name:\n%s", s)
	}
	if !strings.Contains(s, "$hit = @($cands | Where-Object { (([string]$_.MacAddress) -replace '[-:]','') -eq $mac })[0]") {
		t.Error("the batch MAC match must be scoped to the candidates")
	}
}

/*
The batch must NOT use the per-name helper: it asks Hyper-V again for each

	vNIC, and the batch exists to pay that cost once. Pass time on this fleet was
	already reported Slow.
*/
func TestTheBatchResolvesWithoutRequeryingHyperV(t *testing.T) {
	s := vnicsBatchScript([]string{"Mgmt", "Storage01", "Storage02"})

	if n := strings.Count(s, "Get-VMNetworkAdapter -ManagementOS"); n != 1 {
		t.Fatalf("the batch must query Hyper-V once, found %d", n)
	}
	if n := strings.Count(s, "Get-NetAdapter -IncludeHidden"); n != 1 {
		t.Fatalf("the adapter enumeration must be hoisted, found %d", n)
	}
	if !strings.Contains(s, "$alias = Resolve-BallastAlias $a $n") {
		t.Error("the batch must resolve from the vNIC object it already holds")
	}
}
