package types

import (
	"strings"
	"testing"
)

/*
Picking a value the driver will actually accept.

	The reason a bare 9000 cannot simply be written. Intel offers 9014, Mellanox
	9614, Broadcom 9216 — all of which carry a 9000-byte payload, all of which
	count the headers differently. A number that is right on one card is rejected
	or silently wrong on the next, so the only correct value is one the driver
	itself offered.
*/
func TestJumboValueFor(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   int
		expect string
		ok     bool
	}{
		{
			name:   "Intel: 9014 carries a 9000 payload",
			values: []string{"Disabled", "4088 Bytes", "9014 Bytes"},
			want:   9000, expect: "9014 Bytes", ok: true,
		},
		{
			name:   "Mellanox counts higher again, and is still the only option",
			values: []string{"1514", "9614"},
			want:   9000, expect: "9614", ok: true,
		},
		{
			// The smallest that fits, not the largest available: the switch
			// between the hosts has to accommodate whatever is chosen.
			name:   "takes the smallest offered value that carries the payload",
			values: []string{"1514", "4088", "9014", "9216"},
			want:   4000, expect: "4088", ok: true,
		},
		{
			name:   "a driver whose largest size falls short offers nothing",
			values: []string{"Disabled", "4088"},
			want:   9000, ok: false,
		},
		{
			// "Disabled" is how a driver spells off. Ignored, not treated as an
			// error — refusing to parse it would make an ordinary card look broken.
			name:   "non-numeric entries are ignored rather than rejected",
			values: []string{"Disabled", "Enabled", "9014"},
			want:   9000, expect: "9014", ok: true,
		},
		{
			name:   "an adapter offering nothing at all",
			values: nil, want: 9000, ok: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := JumboValueFor(tc.values, tc.want)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tc.ok, got)
			}
			if ok && got != tc.expect {
				t.Errorf("chose %q, want %q", got, tc.expect)
			}
		})
	}
}

func switchAt(name string, mtu int, members ...string) VirtualSwitchSpec {
	return VirtualSwitchSpec{Name: name, TeamMembers: members, MTUBytes: mtu}
}

func mtuVNIC(name, sw string, mtu int, addr string) ManagementVNICSpec {
	return ManagementVNICSpec{
		Name: name, SwitchName: sw, MTUBytes: mtu, Purpose: VNICStorage,
		IPConfig: &IPConfig{Address: addr},
	}
}

/*
A vNIC asking for more than its uplink carries is refused, not reported.

	Windows accepts the interface MTU whatever the adapter beneath it is set to.
	The host then reports 9000 on both, and the miniport drops every frame above
	the uplink's real limit with nothing surfaced to the IP stack. Unlike a single
	storage path, there is no arrangement in which this is what somebody meant.
*/
func TestVNICAboveItsUplinkIsFatal(t *testing.T) {
	n := HostNetworkingSpec{
		Switches:        []VirtualSwitchSpec{switchAt("Converged", 0, "NIC1", "NIC2")},
		ManagementVNICs: []ManagementVNICSpec{mtuVNIC("Storage1", "Converged", 9000, "10.0.1.5/24")},
	}
	ps := ValidateMTU(n)
	if len(FatalStorageNetworkProblems(ps)) == 0 {
		t.Fatalf("a 9000 vNIC on a 1500 uplink was allowed: %s", whys(ps))
	}
	// And it says where jumbo frames are actually enabled, which is the thing
	// an operator gets wrong.
	if !strings.Contains(whys(ps), "on the physical adapters") {
		t.Errorf("the message does not say where the setting lives: %s", whys(ps))
	}
}

// The same vNIC is fine once the switch carries it.
func TestVNICAtItsUplinkIsAccepted(t *testing.T) {
	n := HostNetworkingSpec{
		Switches:        []VirtualSwitchSpec{switchAt("Converged", 9000, "NIC1", "NIC2")},
		ManagementVNICs: []ManagementVNICSpec{mtuVNIC("Storage1", "Converged", 9000, "10.0.1.5/24")},
	}
	if ps := ValidateMTU(n); len(ps) != 0 {
		t.Fatalf("a matched pair was rejected: %s", whys(ps))
	}
}

/*
A storage vNIC left at the default on a jumbo switch.

	Reported, not refused. 1500 storage works — but a switch declared at 9000
	exists because somebody wanted jumbo storage, and a storage vNIC that did not
	opt in is far likelier to be an omission than a decision. It is invisible
	otherwise: the vNIC is up and carrying traffic.
*/
func TestStorageVNICThatDidNotOptIn(t *testing.T) {
	n := HostNetworkingSpec{
		Switches:        []VirtualSwitchSpec{switchAt("Converged", 9000, "NIC1", "NIC2")},
		ManagementVNICs: []ManagementVNICSpec{mtuVNIC("Storage1", "Converged", 0, "10.0.1.5/24")},
	}
	ps := ValidateMTU(n)
	if len(ps) != 1 {
		t.Fatalf("want exactly one advisory, got: %s", whys(ps))
	}
	if ps[0].Fatal {
		t.Error("an unset storage MTU was made fatal; 1500 storage works")
	}
	if !strings.Contains(ps[0].Why, "never sent") {
		t.Errorf("the message does not say what it costs: %q", ps[0].Why)
	}
}

/*
Management traffic is deliberately NOT nagged about.

	A converged switch commonly carries jumbo storage alongside management that
	must stay at 1500 — a management interface at 9000 talking to a 1500 router is
	a slow, sporadic fault that looks like anything but MTU. Leaving management
	alone on a jumbo switch is the correct arrangement, not an omission.
*/
func TestManagementOnAJumboSwitchIsLeftAlone(t *testing.T) {
	n := HostNetworkingSpec{
		Switches: []VirtualSwitchSpec{switchAt("Converged", 9000, "NIC1", "NIC2")},
		ManagementVNICs: []ManagementVNICSpec{
			{Name: "Mgmt", SwitchName: "Converged", IPConfig: &IPConfig{Address: "192.168.1.70/24", Gateway: "192.168.1.1"}},
			mtuVNIC("Storage1", "Converged", 9000, "10.0.1.5/24"),
		},
	}
	if ps := ValidateMTU(n); len(ps) != 0 {
		t.Fatalf("management was nagged about staying at 1500: %s", whys(ps))
	}
}

func TestMTUOutsideWhatAnyAdapterAccepts(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    HostNetworkingSpec
	}{
		{"switch below the Ethernet minimum", HostNetworkingSpec{
			Switches: []VirtualSwitchSpec{switchAt("Converged", 900, "NIC1")}}},
		{"switch above what any adapter offers", HostNetworkingSpec{
			Switches: []VirtualSwitchSpec{switchAt("Converged", 65536, "NIC1")}}},
		{"vNIC above what any adapter offers", HostNetworkingSpec{
			Switches:        []VirtualSwitchSpec{switchAt("Converged", 9000, "NIC1")},
			ManagementVNICs: []ManagementVNICSpec{mtuVNIC("Storage1", "Converged", 65536, "10.0.1.5/24")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(FatalStorageNetworkProblems(ValidateMTU(tc.n))) == 0 {
				t.Errorf("accepted an impossible MTU: %s", whys(ValidateMTU(tc.n)))
			}
		})
	}
}

// Zero everywhere is the default and must stay silent: most hosts will never
// declare an MTU, and a permanent advisory on every one of them is noise.
func TestNoMTUDeclaredSaysNothing(t *testing.T) {
	n := HostNetworkingSpec{
		Switches:        []VirtualSwitchSpec{switchAt("Converged", 0, "NIC1", "NIC2")},
		ManagementVNICs: []ManagementVNICSpec{mtuVNIC("Storage1", "Converged", 0, "10.0.1.5/24")},
	}
	if ps := ValidateMTU(n); len(ps) != 0 {
		t.Fatalf("an undeclared MTU produced findings: %s", whys(ps))
	}
}

/*
What an adapter that cannot do it is told.

	Both cases are facts Ballast cannot change, and both were previously
	invisible: the reconcile would have written a value, the driver would have
	ignored or rejected it, and the host would report jumbo either way.
*/
func TestDescribeJumboRefusalNamesTheActualLimit(t *testing.T) {
	noKeyword := DescribeJumboRefusal("NIC1", "", nil, 9000)
	if !strings.Contains(noKeyword, "no jumbo-frame setting") || !strings.Contains(noKeyword, "driver update") {
		t.Errorf("an adapter with no jumbo support is not explained: %q", noKeyword)
	}
	tooSmall := DescribeJumboRefusal("NIC1", "*JumboPacket", []string{"Disabled", "4088"}, 9000)
	if !strings.Contains(tooSmall, "largest its driver offers is 4088") {
		t.Errorf("the actual ceiling is not named: %q", tooSmall)
	}
	// Both must say nothing was changed — a refusal that leaves the adapter in
	// an unstated condition is worse than the original silence.
	for _, m := range []string{noKeyword, tooSmall} {
		if !strings.Contains(m, "Nothing was changed") && !strings.Contains(m, "changes this") {
			t.Errorf("a refusal does not say what happened to the adapter: %q", m)
		}
	}
}
