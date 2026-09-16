package hyperv

import (
	"context"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

/*
The rule that matters most here: do not write unless it differs.

	Writing *JumboPacket resets the miniport, so the link bounces for a second or
	two. On a converged switch that carries management, cluster heartbeat and
	storage on the same uplinks, an apply that ran on every 40-second pass would
	bounce every uplink in the fleet, for ever, for no change. Idempotency is a
	non-negotiable everywhere in this codebase; here it is also the difference
	between a working cluster and one that flaps.
*/
func TestAdapterMTUIsWrittenOnceAndNeverAgain(t *testing.T) {
	s := &Stub{}
	ctx := context.Background()

	out, err := s.EnsureAdapterMTU(ctx, []string{"NIC1", "NIC2"}, 9000)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if out != OutcomeUpdated {
		t.Fatalf("first pass reported %v, want Updated", out)
	}
	if len(s.MTUWrites) != 2 {
		t.Fatalf("first pass wrote %v, want both adapters", s.MTUWrites)
	}

	// Every pass after this must touch nothing.
	for i := 0; i < 3; i++ {
		out, err := s.EnsureAdapterMTU(ctx, []string{"NIC1", "NIC2"}, 9000)
		if err != nil {
			t.Fatalf("steady pass %d: %v", i, err)
		}
		if out != OutcomeUnchanged {
			t.Errorf("steady pass %d reported %v, want Unchanged", i, out)
		}
	}
	if len(s.MTUWrites) != 2 {
		t.Fatalf("a steady pass bounced a live uplink: writes are now %v", s.MTUWrites)
	}
}

/*
An adapter whose driver cannot reach the size does not stop the others.

	A mixed fleet is the normal case — a host with two 25G cards and an onboard
	1G — and refusing the whole team because one member cannot do jumbo would
	leave a switch that could have carried it at 1500. So what can be applied is
	applied, and what could not is named.
*/
func TestOneIncapableAdapterDoesNotStopTheRest(t *testing.T) {
	s := &Stub{AdapterMTU: map[string]StubAdapterMTU{
		"nic2": {NlMtu: 1500, Keyword: "*JumboPacket", Setting: "Disabled",
			Values: []string{"Disabled", "4088 Bytes"}},
	}}
	out, err := s.EnsureAdapterMTU(context.Background(), []string{"NIC1", "NIC2"}, 9000)

	if err == nil {
		t.Fatal("an adapter that cannot carry 9000 was not reported")
	}
	if !strings.Contains(err.Error(), "largest its driver allows is 4088") {
		t.Errorf("the refusal does not name the actual ceiling: %v", err)
	}
	// The capable one was still configured, and the outcome says so.
	if out != OutcomeUpdated {
		t.Errorf("outcome %v: the capable adapter's change was not reported", out)
	}
	if len(s.MTUWrites) != 1 || !strings.EqualFold(s.MTUWrites[0], "NIC1") {
		t.Errorf("wrote %v, want only NIC1", s.MTUWrites)
	}
}

// An adapter with no jumbo setting at all is a fact about the card, and says so
// rather than reporting a generic failure.
func TestAdapterWithNoJumboSupport(t *testing.T) {
	s := &Stub{AdapterMTU: map[string]StubAdapterMTU{
		"nic1": {NlMtu: 1500, Keyword: "", Values: nil},
	}}
	_, err := s.EnsureAdapterMTU(context.Background(), []string{"NIC1"}, 9000)
	if err == nil || !strings.Contains(err.Error(), "no jumbo-frame setting") {
		t.Fatalf("an adapter with no jumbo support is not explained: %v", err)
	}
	if len(s.MTUWrites) != 0 {
		t.Errorf("wrote to an adapter that cannot take the value: %v", s.MTUWrites)
	}
}

// Declaring nothing does nothing. Most hosts will never declare an MTU and must
// not pay a read, let alone a write, for it.
func TestNoMTUDeclaredTouchesNothing(t *testing.T) {
	s := &Stub{}
	out, err := s.EnsureAdapterMTU(context.Background(), []string{"NIC1"}, 0)
	if err != nil || out != OutcomeUnchanged || len(s.MTUWrites) != 0 {
		t.Fatalf("an undeclared MTU did something: out=%v err=%v writes=%v", out, err, s.MTUWrites)
	}
}

/*
Which probes are worth running.

	A jumbo probe that crosses a router measures the ROUTER's MTU, and would then
	report a perfectly good switch as the culprit. So a pair is only tested where
	source and target share a subnet — and a vNIC whose address carries no prefix
	is skipped rather than assumed to be a /24, because guessing a mask runs
	probes across unrelated networks.
*/
func TestJumboPairsOnlyProbeWithinASubnet(t *testing.T) {
	vnics := []types.ManagementVNICSpec{
		{Name: "Storage1", IPConfig: &types.IPConfig{Address: "10.0.1.11/24"}},
		{Name: "Storage2", IPConfig: &types.IPConfig{Address: "10.0.2.11/24"}},
		{Name: "NoPrefix", IPConfig: &types.IPConfig{Address: "10.0.1.99"}},
		{Name: "DHCP"},
	}
	targets := []string{"10.0.1.12", "10.0.2.12", "192.168.1.1", "not-an-address"}

	got := jumboPairs(vnics, targets)
	if len(got) != 2 {
		t.Fatalf("ran %d probe(s), want 2: %+v", len(got), got)
	}
	for _, p := range got {
		switch p.VNIC {
		case "Storage1":
			if p.Source != "10.0.1.11" || p.Target != "10.0.1.12" {
				t.Errorf("wrong pair: %+v", p)
			}
		case "Storage2":
			if p.Source != "10.0.2.11" || p.Target != "10.0.2.12" {
				t.Errorf("wrong pair: %+v", p)
			}
		default:
			t.Errorf("probed from a vNIC that could not be compared: %+v", p)
		}
	}
}

// The payload is the MTU less 28 — 20 bytes of IPv4 header and 8 of ICMP.
// Getting this wrong by 28 bytes reports failure on a path that works.
func TestJumboScriptSendsTheRightPayloadWithDontFragment(t *testing.T) {
	script := jumboPathScript([]jumboPair{{VNIC: "S1", Source: "10.0.1.11", Target: "10.0.1.12"}}, 9000)
	if !strings.Contains(script, "-f -l 8972") {
		t.Errorf("payload or don't-fragment wrong; without -f the stack splits the packet and this passes on a 1500 path:\n%s", script)
	}
	if !strings.Contains(script, "-S '10.0.1.11'") {
		t.Errorf("the source is not pinned, so the probe may leave by the management path:\n%s", script)
	}
	// The ordinary ping runs too, and must NOT carry -f: it is what makes a
	// failure attributable to MTU rather than to an unreachable peer.
	std := script[strings.Index(script, "$s = "):]
	if strings.Contains(std[:strings.Index(std, "\n")], "-f") {
		t.Errorf("the control ping is also don't-fragment, so it cannot distinguish an MTU fault:\n%s", script)
	}
}

/*
Naming the culprit rather than reporting a failure.

	This is what the job is for. A list of failed pings is a symptom; which of
	the three layers is wrong is the diagnosis, and it is knowable from whether
	an ordinary ping crossed.
*/
func TestDescribeJumboProbesNamesTheSwitch(t *testing.T) {
	probes := []JumboProbe{
		{VNIC: "Storage1", Target: "10.0.1.12", OK: false, StandardOK: true},
	}
	msg := DescribeJumboProbes(probes, 9000)
	if !strings.Contains(msg, "physical switch between them") {
		t.Errorf("a reachable path that cannot carry the frame does not name the switch: %q", msg)
	}
	if !strings.Contains(msg, "Ballast has no presence on the switch") {
		t.Errorf("it does not say the one step is outside Ballast: %q", msg)
	}
	if !JumboProbesFailed(probes) {
		t.Error("a genuine MTU fault did not fail the job")
	}
}

/*
An unreachable peer is NOT an MTU fault, and must not be reported as one.

	Both pings failing means the peer is down, misaddressed or firewalled.
	Calling that "jumbo frames are not working" sends an operator to reconfigure
	a switch that was never the problem — a wrong diagnosis stated confidently,
	which is worse than no diagnosis.
*/
func TestUnreachablePeerIsNotAnMTUFault(t *testing.T) {
	probes := []JumboProbe{
		{VNIC: "Storage1", Target: "10.0.1.12", OK: false, StandardOK: false},
	}
	msg := DescribeJumboProbes(probes, 9000)
	if strings.Contains(msg, "physical switch") {
		t.Errorf("an unreachable peer was blamed on the switch: %q", msg)
	}
	if !strings.Contains(msg, "not an MTU fault") {
		t.Errorf("it does not say this is not an MTU fault: %q", msg)
	}
	if JumboProbesFailed(probes) {
		t.Error("an unreachable peer failed the job, which sends the operator to the wrong place")
	}
}

func TestDescribeJumboProbesWithNothingToTest(t *testing.T) {
	msg := DescribeJumboProbes(nil, 9000)
	if !strings.Contains(msg, "share a subnet") {
		t.Errorf("an empty test does not say why nothing ran: %q", msg)
	}
}

// ping repeats itself; the useful line is the one naming the fragmentation.
func TestFirstPingLineFindsTheVerdict(t *testing.T) {
	out := "\r\nPinging 10.0.1.12 with 8972 bytes of data:\r\n" +
		"Packet needs to be fragmented but DF set.\r\n" +
		"Packet needs to be fragmented but DF set.\r\n"
	if got := firstPingLine(out); got != "Packet needs to be fragmented but DF set." {
		t.Errorf("got %q", got)
	}
}

func TestParseMTUParam(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"9000", 9000}, {"1500", 1500}, {"", types.DefaultJumboMTU},
		{"nonsense", types.DefaultJumboMTU}, {"70000", types.DefaultJumboMTU}, {"28", types.DefaultJumboMTU},
	} {
		if got := ParseMTUParam(tc.in); got != tc.want {
			t.Errorf("ParseMTUParam(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

/*
A teamed uplink has no IP interface, and judging it by one never settles.

	A physical NIC bound into a vSwitch holds no IP address by design, so
	Get-NetIPInterface returns nothing for it and NlMtu reads 0. The check read
	that as "0 < 9000, not carrying it" and then found the right value already
	selected in the driver, so it reported "not applied — its IP interface still
	reports an MTU of 0" on every pass, for ever.

	Observed on HVNEW01, 2026-09-07: all four uplinks correctly at 9014, the
	condition permanently amber. A reconcile that can never settle is precisely
	what this feature exists to prevent, produced by the check itself.
*/
func TestATeamedUplinkWithNoIPInterfaceStillSettles(t *testing.T) {
	teamed := StubAdapterMTU{
		NlMtu:   0, // bound into a vSwitch: no IP interface at all
		Keyword: "*JumboPacket", Setting: "9014 Bytes",
		Values: []string{"Disabled", "4088 Bytes", "9014 Bytes"},
	}
	s := &Stub{AdapterMTU: map[string]StubAdapterMTU{"nic1": teamed, "nic2": teamed}}

	for i := 0; i < 3; i++ {
		out, err := s.EnsureAdapterMTU(context.Background(), []string{"NIC1", "NIC2"}, 9000)
		if err != nil {
			t.Fatalf("pass %d reported a problem with a correctly configured team: %v", i, err)
		}
		if out != OutcomeUnchanged {
			t.Fatalf("pass %d reported %v; a team already at 9014 has nothing to do", i, out)
		}
	}
	if len(s.MTUWrites) != 0 {
		t.Errorf("rewrote a correct value and bounced the uplinks: %v", s.MTUWrites)
	}
}

/*
The driver's value is only trusted where there is nothing better.

	An adapter WITH an IP interface reporting less than asked is a real fault —
	the driver accepted a value that is not in force — and must keep saying so.
	Trusting the selected value everywhere would trade one silence for another.
*/
func TestAnAdapterWithAnIPInterfaceIsStillJudgedByIt(t *testing.T) {
	s := &Stub{AdapterMTU: map[string]StubAdapterMTU{
		"nic1": {NlMtu: 1500, Keyword: "*JumboPacket", Setting: "9014 Bytes",
			Values: []string{"Disabled", "9014 Bytes"}},
	}}
	_, err := s.EnsureAdapterMTU(context.Background(), []string{"NIC1"}, 9000)
	if err == nil {
		t.Fatal("an adapter whose IP interface is still at 1500 was reported as settled")
	}
	if !strings.Contains(err.Error(), "still reports an MTU of 1500") {
		t.Errorf("the message does not say what the interface actually reports: %v", err)
	}
}

// The size is compared, not the string: a card whose menu only reaches 9014 is
// carrying the 9000-byte payload that was asked for.
func TestJumboValueSizeIgnoresTheUnits(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"9014 Bytes", 9014, true}, {"9014", 9014, true}, {"Disabled", 0, false}, {"", 0, false},
	} {
		got, ok := types.JumboValueSize(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("JumboValueSize(%q) = %d,%v want %d,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

/*
The HPE FlexibleLOM: a numeric property, and it is applied differently.

	Seen on real hardware 2026-09-07. Both ports reported "*JumboPacket ... no
	size this agent could read. It offered: ." for ever, because only the
	enumerated list was read and a numeric property has none — it has a range.

	The write differs too: a numeric property takes -RegistryValue, and passing a
	bare number as a display value is rejected by some drivers and silently
	ignored by others. The second is the dangerous one, so the script is checked
	rather than just the choice.
*/
func TestANumericJumboPropertyIsChosenAndWrittenCorrectly(t *testing.T) {
	a := AdapterMTU{
		Name: "Embedded FlexibleLOM 1 Port 1", Found: true,
		NlMtu: 1500, Keyword: "*JumboPacket", Setting: "1500",
		Values: nil, // numeric: no enumerated list at all
		Min:    1500, Max: 9014, Step: 1,
	}
	apply, refusals := planAdapterMTU([]AdapterMTU{a}, 9000)
	if len(refusals) != 0 {
		t.Fatalf("a card with a perfectly good range was refused: %v", refusals)
	}
	if len(apply) != 1 || apply[0].Value != "9014" {
		t.Fatalf("chose %+v, want 9014 — the payload plus headers", apply)
	}
	if !apply[0].Numeric {
		t.Error("not marked numeric, so it would be written as a display value")
	}
	script := applyMTUScript(apply)
	if !strings.Contains(script, "-RegistryValue '9014'") {
		t.Errorf("a numeric property is not written by registry value:\n%s", script)
	}
	if strings.Contains(script, "-DisplayValue") {
		t.Errorf("written as a display value, which some drivers ignore silently:\n%s", script)
	}
}

// An enumerated property keeps taking a display value. Both shapes exist on one
// fleet and getting either wrong is silent.
func TestAnEnumeratedJumboPropertyStillUsesDisplayValue(t *testing.T) {
	a := AdapterMTU{
		Name: "NIC1", Found: true, NlMtu: 1500, Keyword: "*JumboPacket", Setting: "Disabled",
		Values: []string{"Disabled", "4088 Bytes", "9014 Bytes"},
	}
	apply, _ := planAdapterMTU([]AdapterMTU{a}, 9000)
	if len(apply) != 1 || apply[0].Value != "9014 Bytes" || apply[0].Numeric {
		t.Fatalf("chose %+v, want the enumerated 9014 Bytes", apply)
	}
	script := applyMTUScript(apply)
	if !strings.Contains(script, "-DisplayValue '9014 Bytes'") {
		t.Errorf("an enumerated property is not written by display value:\n%s", script)
	}
}

// A numeric card already at its value settles, like any other.
func TestANumericAdapterAlreadyAtItsValueSettles(t *testing.T) {
	a := AdapterMTU{
		Name: "Embedded FlexibleLOM 1 Port 1", Found: true,
		NlMtu:   0, // teamed: no IP interface
		Keyword: "*JumboPacket", Setting: "9014", Min: 1500, Max: 9014, Step: 1,
	}
	apply, refusals := planAdapterMTU([]AdapterMTU{a}, 9000)
	if len(apply) != 0 || len(refusals) != 0 {
		t.Fatalf("a teamed numeric adapter already at 9014 did not settle: apply=%+v refusals=%v", apply, refusals)
	}
}

/*
The card where both of the other two sources failed at once.

	HPE 631FLR-SFP28 (Marvell FastLinQ) on the physical host, 2026-09-07, read
	straight off the machine:

	  Get-NetIPInterface     — does not list the physical adapters at all, so
	                           NlMtu is 0
	  *JumboPacket           — DisplayValue 1514, no valid values, no min/max
	  Get-NetAdapter MtuSize — 9000

	The card was carrying 9000 and Ballast drew 1514. MtuSize is the only source
	that was both present and true, and it was the one the agent never asked for.
*/
func TestMtuSizeWinsWhenTheDriverPropertyDisagrees(t *testing.T) {
	a := AdapterMTU{
		Name: "Embedded FlexibleLOM 1 Port 1", Found: true,
		MtuSize: 9000, // the card really is at 9000
		NlMtu:   0,    // teamed: no IP interface
		Keyword: "*JumboPacket", Setting: "1514",
		Values: nil, Min: 0, Max: 0,
	}
	if !adapterCarries(a, 9000) {
		t.Fatal("a card carrying 9000 was judged not to be, from a driver property that disagreed")
	}
	apply, refusals := planAdapterMTU([]AdapterMTU{a}, 9000)
	if len(apply) != 0 {
		t.Errorf("would have rewritten a card already at 9000, bouncing a live uplink: %+v", apply)
	}
	if len(refusals) != 0 {
		t.Errorf("reported a problem with a correctly configured card: %v", refusals)
	}
}

// And a card genuinely below what was asked is still caught, from the same
// source. MtuSize being authoritative must not make it permissive.
func TestMtuSizeBelowWhatWasAskedIsStillShort(t *testing.T) {
	a := AdapterMTU{
		Name: "NIC1", Found: true, MtuSize: 1500, NlMtu: 0,
		Keyword: "*JumboPacket", Setting: "9014 Bytes",
		Values: []string{"Disabled", "9014 Bytes"},
	}
	if adapterCarries(a, 9000) {
		t.Fatal("a card at 1500 was judged to carry 9000 because its driver had a value selected")
	}
}

/*
The order matters, and each source is used only where the one above is

	absent — otherwise a stale driver value overrules a live reading.
*/
func TestAdapterCarriesPrefersTheLiveReadings(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    AdapterMTU
		want bool
	}{
		{"MtuSize decides over NlMtu", AdapterMTU{MtuSize: 9000, NlMtu: 1500}, true},
		{"NlMtu decides when there is no MtuSize", AdapterMTU{MtuSize: 0, NlMtu: 9000}, true},
		{"the driver value is the last resort", AdapterMTU{MtuSize: 0, NlMtu: 0, Setting: "9014"}, true},
		{"and nothing at all carries nothing", AdapterMTU{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := adapterCarries(tc.a, 9000); got != tc.want {
				t.Errorf("adapterCarries = %v, want %v", got, tc.want)
			}
		})
	}
}

/*
A management vNIC is an adapter as well as an IP interface.

	Set-NetIPInterface alone was not enough on the physical host: the call is
	accepted and NlMtu stays at 1500, on a host whose uplinks are genuinely at
	9000. The Hyper-V Virtual Ethernet Adapter has its own *JumboPacket, and the
	IP layer cannot exceed what the miniport under it carries.

	The script is asserted rather than the outcome, because the ORDER is the
	fix — adapter first, interface second, the same order as the uplinks and the
	switch above them — and an order is not observable from a return value.
*/
func TestVNICMTURaisesTheAdapterBeforeTheInterface(t *testing.T) {
	var script string
	p := &PowerShell{}
	p.run = func(_ context.Context, s string) ([]byte, error) {
		script = s
		return []byte("RESULT=set"), nil
	}
	if _, err := p.EnsureInterfaceMTU(context.Background(), "Storage01", 9000, true); err != nil {
		t.Fatalf("set: %v", err)
	}

	adapter := strings.Index(script, "Set-NetAdapterAdvancedProperty")
	iface := strings.Index(script, "Set-NetIPInterface")
	if adapter < 0 || iface < 0 {
		t.Fatalf("one of the two layers is not set at all:\n%s", script)
	}
	if adapter > iface {
		t.Errorf("the interface is raised before the adapter under it, which is the order that does not work:\n%s", script)
	}
	// Only when the adapter is actually short: writing it resets the vNIC, and
	// on the management vNIC that drops the host's address for a moment.
	if !strings.Contains(script, "$aMtu -lt $want") {
		t.Errorf("the adapter is written unconditionally, so every pass would reset the vNIC:\n%s", script)
	}
}

/*
The failure has to say WHICH layer is short.

	"the interface accepted an MTU of 9000 and still reports 1500" names a
	symptom. Whether the adapter beneath it or the switch above it is the limit
	is the diagnosis, and this script is the only thing that can see both.
*/
func TestVNICMTUFailureNamesTheLayerThatIsShort(t *testing.T) {
	var script string
	p := &PowerShell{}
	p.run = func(_ context.Context, s string) ([]byte, error) { script = s; return []byte("RESULT=set"), nil }
	if _, err := p.EnsureInterfaceMTU(context.Background(), "Storage01", 9000, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Its adapter carries",
		"The adapter under this vNIC is the limit.",
		"The adapter is not the limit, so the vSwitch or its uplinks are.",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the failure does not say %q:\n%s", want, script)
		}
	}
}

// Nothing declared does nothing, and costs not even a read.
func TestVNICMTUUndeclaredDoesNothing(t *testing.T) {
	called := false
	p := &PowerShell{}
	p.run = func(_ context.Context, _ string) ([]byte, error) { called = true; return nil, nil }
	out, err := p.EnsureInterfaceMTU(context.Background(), "Storage01", 0, true)
	if err != nil || out != OutcomeUnchanged || called {
		t.Fatalf("an undeclared vNIC MTU did something: out=%v err=%v ran=%v", out, err, called)
	}
}

/*
No offload is named on the MTU condition any more.

	It named LSO, because disabling LSO made jumbo start working. The operator
	then re-ENABLED it and jumbo kept working — writing any advanced property
	resets the miniport, and that reset was the whole effect. Naming an innocent
	setting on a condition an operator reads is worse than naming nothing: it
	sends them to change something that cannot help, and it did.
*/
func TestTheMTUConditionDoesNotBlameAnOffload(t *testing.T) {
	s := &Stub{AdapterMTU: map[string]StubAdapterMTU{
		// Both offloads on, and the adapter already carrying 9000.
		"nic1": {NlMtu: 9000, Keyword: "*JumboPacket", Setting: "9014 Bytes",
			Values: []string{"Disabled", "9014 Bytes"}},
	}}
	out, err := s.EnsureAdapterMTU(context.Background(), []string{"NIC1"}, 9000)
	if err != nil {
		t.Fatalf("a correctly configured adapter reported a problem: %v", err)
	}
	if out != OutcomeUnchanged {
		t.Errorf("outcome %v on an adapter already at 9000", out)
	}
}

/*
Silence is not "off", and reporting it as off is a false success.

	The first version read a cmdlet that returned nothing as "disabled", so on
	HVNEW04 the job reported LSO off on four uplinks it had not changed. The
	operator was told the fix had been applied, went looking at their switch, and
	the inventory eight minutes later still read LSO enabled on all four. This
	codebase names absent-read-as-a-value as its most expensive bug class; this
	was it, written fresh, in the very job meant to close an invisible fault.
*/
func TestUnreadableLSOIsNotReportedAsOff(t *testing.T) {
	s := &Stub{Offloads: map[string]StubOffload{
		"ethernet 2": {Unreadable: true},
		"ethernet 3": {Unreadable: true},
	}}
	_, err := s.DisableLSO(context.Background(), []string{"Ethernet 2", "Ethernet 3"}, nil)
	if err == nil {
		t.Fatal("adapters that reported nothing were counted as successfully disabled")
	}
	if !strings.Contains(err.Error(), "could be read") {
		t.Errorf("the failure does not say the setting was unreadable: %v", err)
	}
	if len(s.OffloadWrites) != 0 {
		t.Errorf("claimed to have written to adapters that answer nothing: %v", s.OffloadWrites)
	}
}

/*
A driver that accepts the call and declines it — HVNEW04's, on every uplink.

	Reporting this as done is what sent the operator to look at their switch for
	a fault on their host, so it fails, and it says the host still cannot carry
	jumbo frames rather than leaving that to be inferred.
*/
func TestLSOStillOnAfterDisablingIsAFailure(t *testing.T) {
	s := &Stub{Offloads: map[string]StubOffload{
		"ethernet 2": {LSO: true, Stuck: true},
		"ethernet 3": {LSO: true},
	}}
	msg, err := s.DisableLSO(context.Background(), []string{"Ethernet 2", "Ethernet 3"}, nil)
	if err == nil {
		t.Fatal("a driver that declined was reported as success")
	}
	if !strings.Contains(err.Error(), "Ethernet 2") {
		t.Errorf("the refusal does not name the adapter that declined: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot carry jumbo frames") {
		t.Errorf("does not say what it means for the operator: %v", err)
	}
	// What DID work is still said: half a team fixed is worth having.
	if !strings.Contains(msg, "Ethernet 3") {
		t.Errorf("the adapter that was fixed is not reported: %q", msg)
	}
}

func TestDisablingLSOReportsWhatIsOffAndWhatIsNext(t *testing.T) {
	s := &Stub{Offloads: map[string]StubOffload{"ethernet 2": {LSO: true}}}
	msg, err := s.DisableLSO(context.Background(), []string{"Ethernet 2"}, nil)
	if err != nil {
		t.Fatalf("a clean disable failed: %v", err)
	}
	if !strings.Contains(msg, "LSO is off on Ethernet 2") {
		t.Errorf("does not say what is off: %q", msg)
	}
	// Turning it off is not proof jumbo works; only a frame is.
	if !strings.Contains(msg, "jumbo path test") {
		t.Errorf("does not name the step that actually confirms it: %q", msg)
	}
}

// The management vNICs over a switch are covered too: the operator's own
// working command had no -Name and so hit both.
func TestDisablingLSOCoversTheSwitchesVNICs(t *testing.T) {
	script := disableLSOScript([]string{"Ethernet 2"}, []string{"ConvergedSwitch"})
	if !strings.Contains(script, "Get-VMNetworkAdapter -ManagementOS -SwitchName $sw") {
		t.Errorf("the switch's management vNICs are not included:\n%s", script)
	}
	// Every sub-setting named: the cmdlet's defaults vary by driver, and V1IPv4
	// in particular is commonly left enabled.
	if !strings.Contains(script, "-IPv4 -IPv6") || !strings.Contains(script, "-V1IPv4") {
		t.Errorf("not every LSO sub-setting is disabled explicitly:\n%s", script)
	}
	// Verified against a FRESH read: asking in the same breath returns what was
	// requested rather than what took.
	if !strings.Contains(script, "Start-Sleep") {
		t.Errorf("reads back without letting the change settle:\n%s", script)
	}
}

func TestDisablingLSONeedsSomethingToActOn(t *testing.T) {
	s := &Stub{}
	if _, err := s.DisableLSO(context.Background(), nil, nil); err == nil {
		t.Fatal("disabling nothing was reported as success")
	}
}

/*
What actually puts an MTU change in force: restarting the adapter.

	Chased at length as an offload problem. Disabling LSO made jumbo start
	working, so LSO looked like the cause — until the operator re-ENABLED it and
	jumbo kept working. Writing any advanced property resets the miniport, and
	that reset was the whole effect; the offload was never involved in either
	direction.

	Set-NetIPInterface resets nothing, so a vNIC's MTU is set and not in force,
	and NO reading shows the difference — HVNEW04 reported NlMtu 9000 on every
	vNIC while a 9000-byte frame would not cross. Only the path test can tell.
*/
func TestAFabricVNICIsCycledSoTheChangeTakesEffect(t *testing.T) {
	var script string
	p := &PowerShell{}
	p.run = func(_ context.Context, s string) ([]byte, error) { script = s; return []byte("RESULT=set"), nil }
	if _, err := p.EnsureInterfaceMTU(context.Background(), "Storage01", 9000, true); err != nil {
		t.Fatalf("set: %v", err)
	}
	if !strings.Contains(script, "Restart-NetAdapter") {
		t.Fatalf("the adapter is never cycled, so the MTU is set and never in force:\n%s", script)
	}
	// After the interface is set, not before: cycling first would discard it.
	if strings.Index(script, "Restart-NetAdapter") < strings.Index(script, "Set-NetIPInterface") {
		t.Errorf("the adapter is cycled before the MTU is applied:\n%s", script)
	}
}

/*
The management vNIC is set and NOT cycled.

	It carries the host's address and the agent's link to the centre, so
	restarting it is a self-inflicted outage on the path Ballast is managed
	over. It is left for an operator to do deliberately — and said plainly,
	because an MTU that reads correct and does not work is exactly the silence
	this whole feature exists to break.
*/
func TestTheManagementVNICIsNotCycledOnBallastsOwnInitiative(t *testing.T) {
	var script string
	p := &PowerShell{}
	p.run = func(_ context.Context, s string) ([]byte, error) {
		script = s
		return []byte("RESULT=set-needs-restart"), nil
	}
	_, err := p.EnsureInterfaceMTU(context.Background(), "Management", 9000, false)
	if err == nil {
		t.Fatal("the management vNIC was reported as done when its MTU is not in force")
	}
	for _, want := range []string{"will not take effect until the adapter is restarted", "drops this host's address"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not say %q: %v", want, err)
		}
	}
	if !strings.Contains(script, "$mayCycle = $false") {
		t.Errorf("the script was not told to leave the adapter alone:\n%s", script)
	}
}

// And the reconciler decides which is which by purpose, so a storage vNIC is
// cycled and the management one is not.
func TestOnlyFabricVNICsAreCycledByTheReconciler(t *testing.T) {
	s := &Stub{}
	if _, err := s.EnsureInterfaceMTU(context.Background(), "Storage01", 9000, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureInterfaceMTU(context.Background(), "Management", 9000, false); err != nil {
		t.Fatal(err)
	}
	if len(s.VNICCycles) != 1 || s.VNICCycles[0] != "Storage01" {
		t.Fatalf("cycled %v; only the fabric vNIC should be restarted", s.VNICCycles)
	}
}

// Cycling happens on the change and never again: a settled vNIC is read and
// left alone, so a steady pass cannot bounce a live interface.
func TestASettledVNICIsNotCycledAgain(t *testing.T) {
	s := &Stub{}
	for i := 0; i < 3; i++ {
		if _, err := s.EnsureInterfaceMTU(context.Background(), "Storage01", 9000, true); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.VNICCycles) != 1 {
		t.Fatalf("cycled %d times; a steady pass must not bounce a live interface", len(s.VNICCycles))
	}
}
