package types

import (
	"strings"
	"testing"
)

/* Both storage models multiplex across INTERFACES — SMB Multichannel for S2D,
   MPIO for iSCSI — and neither can manufacture an interface that is not there.
   So the arrangement both need is the same: two or more storage vNICs, separate
   subnets, separate physical uplinks.

   None of that was expressible, so none of it was checked. On the rig
   2026-08-24 three hosts each declared three array portals, held one path each,
   and reported MPIO "in effect" with nothing to fail over to. Everything looked
   redundant and one cable carried the lot. */

func converged(members ...string) HostNetworkingSpec {
	return HostNetworkingSpec{
		Switches: []VirtualSwitchSpec{{Name: "ConvergedSwitch", TeamMembers: members}},
	}
}

func storageVNIC(name, adapter, cidr string) ManagementVNICSpec {
	return ManagementVNICSpec{
		Name: name, SwitchName: "ConvergedSwitch", Purpose: VNICStorage,
		TeamMemberAdapter: adapter, IPConfig: &IPConfig{Address: cidr},
	}
}

func whys(ps []StorageNetworkProblem) string {
	var b []string
	for _, p := range ps {
		b = append(b, p.Error())
	}
	return strings.Join(b, " | ")
}

// The arrangement that is actually correct must pass silently, or the check is
// noise and gets ignored.
func TestTwoStorageVNICsOnSeparateUplinksAndSubnetsAreClean(t *testing.T) {
	n := converged("Ethernet1", "Ethernet2")
	n.ManagementVNICs = []ManagementVNICSpec{
		storageVNIC("SMB01", "Ethernet1", "10.0.41.11/24"),
		storageVNIC("SMB02", "Ethernet2", "10.0.42.11/24"),
		{Name: "Mgmt", SwitchName: "ConvergedSwitch", IPConfig: &IPConfig{Address: "192.168.1.11/24"}},
	}
	if got := ValidateStorageNetwork(n, true); len(got) != 0 {
		t.Fatalf("a correct converged storage layout must report nothing: %s", whys(got))
	}
}

// The failure this exists to catch: two paths, one cable.
func TestTwoStorageVNICsOnOneUplinkIsReported(t *testing.T) {
	n := converged("Ethernet1", "Ethernet2")
	n.ManagementVNICs = []ManagementVNICSpec{
		storageVNIC("SMB01", "Ethernet1", "10.0.41.11/24"),
		storageVNIC("SMB02", "Ethernet1", "10.0.42.11/24"),
	}
	got := whys(ValidateStorageNetwork(n, true))
	if !strings.Contains(got, "same physical adapter") {
		t.Fatalf("two vNICs on one uplink must be reported: %s", got)
	}
	if !strings.Contains(got, "one path wearing two addresses") {
		t.Errorf("it must say what is actually wrong, not just that it is: %s", got)
	}
	// Both names, in a stable order — a message that shuffles reads as a new one.
	if !strings.Contains(got, "SMB01 and SMB02") {
		t.Errorf("both vNICs must be named, in a stable order: %s", got)
	}
}

// The quieter version of the same mistake.
func TestTwoStorageVNICsOnOneSubnetIsReported(t *testing.T) {
	n := converged("Ethernet1", "Ethernet2")
	n.ManagementVNICs = []ManagementVNICSpec{
		storageVNIC("SMB01", "Ethernet1", "10.0.41.11/24"),
		storageVNIC("SMB02", "Ethernet2", "10.0.41.12/24"),
	}
	got := whys(ValidateStorageNetwork(n, true))
	if !strings.Contains(got, "10.0.41.0/24") {
		t.Fatalf("the shared subnet must be named: %s", got)
	}
	if !strings.Contains(got, "adds an address and no path") {
		t.Errorf("it must say why a second vNIC there buys nothing: %s", got)
	}
}

// An unpinned storage vNIC on a teamed switch is the whole defect in one field.
func TestAnUnpinnedStorageVNICOnATeamIsReported(t *testing.T) {
	n := converged("Ethernet1", "Ethernet2")
	n.ManagementVNICs = []ManagementVNICSpec{
		{Name: "SMB01", SwitchName: "ConvergedSwitch", Purpose: VNICStorage, IPConfig: &IPConfig{Address: "10.0.41.11/24"}},
	}
	got := whys(ValidateStorageNetwork(n, false))
	if !strings.Contains(got, "not pinned to a physical adapter") {
		t.Fatalf("an unpinned storage vNIC must be reported: %s", got)
	}
	// The refusal has to name the choices, or it is a complaint rather than help.
	if !strings.Contains(got, "Ethernet1, Ethernet2") {
		t.Errorf("it must name the adapters available to pin to: %s", got)
	}
}

// A single-member switch has nothing to pin to, so silence is correct there.
func TestAnUnpinnedVNICOnAnUntearmedSwitchIsFine(t *testing.T) {
	n := converged("Ethernet1")
	n.ManagementVNICs = []ManagementVNICSpec{
		{Name: "SMB01", SwitchName: "ConvergedSwitch", Purpose: VNICStorage, IPConfig: &IPConfig{Address: "10.0.41.11/24"}},
	}
	for _, p := range ValidateStorageNetwork(n, false) {
		if strings.Contains(p.Why, "not pinned") {
			t.Fatalf("there is only one uplink, so pinning says nothing: %s", p.Error())
		}
	}
}

// Pinning to an adapter the switch does not have cannot be applied at all, so
// this one is fatal rather than advisory.
func TestPinningToANonMemberIsFatal(t *testing.T) {
	n := converged("Ethernet1", "Ethernet2")
	n.ManagementVNICs = []ManagementVNICSpec{storageVNIC("SMB01", "Ethernet9", "10.0.41.11/24")}
	got := ValidateStorageNetwork(n, false)
	if len(FatalStorageNetworkProblems(got)) != 1 {
		t.Fatalf("a mapping that cannot be applied must be fatal: %s", whys(got))
	}
	if !strings.Contains(whys(got), "not a member of the switch") {
		t.Errorf("it must say why: %s", whys(got))
	}
}

// Storage paths are chosen by subnet, so a storage vNIC on DHCP cannot be
// matched to anything or relied on to keep its path across a reboot.
func TestAStorageVNICWithoutAnAddressIsFatal(t *testing.T) {
	n := converged("Ethernet1", "Ethernet2")
	n.ManagementVNICs = []ManagementVNICSpec{
		{Name: "SMB01", SwitchName: "ConvergedSwitch", Purpose: VNICStorage, TeamMemberAdapter: "Ethernet1"},
	}
	if len(FatalStorageNetworkProblems(ValidateStorageNetwork(n, false))) != 1 {
		t.Fatal("a storage vNIC on DHCP must be refused")
	}
}

// A single storage path is a legitimate lab arrangement and an unacceptable
// production one. Ballast is not the judge of which, so it says so and does not
// refuse.
func TestASingleStoragePathIsReportedButNotFatal(t *testing.T) {
	n := converged("Ethernet1", "Ethernet2")
	n.ManagementVNICs = []ManagementVNICSpec{storageVNIC("SMB01", "Ethernet1", "10.0.41.11/24")}

	got := ValidateStorageNetwork(n, true)
	if len(got) != 1 || len(FatalStorageNetworkProblems(got)) != 0 {
		t.Fatalf("one path must warn and not refuse: %s", whys(got))
	}
	if !strings.Contains(whys(got), "no redundant path") {
		t.Errorf("it must say what is missing: %s", whys(got))
	}
	// And say nothing at all where multipathing was never required.
	if n := ValidateStorageNetwork(n, false); len(n) != 0 {
		t.Errorf("a single path is not a problem when nothing asked for two: %s", whys(n))
	}
}

// Non-storage vNICs are none of this check's business.
func TestManagementVNICsAreNotChecked(t *testing.T) {
	n := converged("Ethernet1", "Ethernet2")
	n.ManagementVNICs = []ManagementVNICSpec{
		{Name: "Mgmt", SwitchName: "ConvergedSwitch"},
		{Name: "LM", SwitchName: "ConvergedSwitch", Purpose: VNICLiveMigration},
	}
	if got := ValidateStorageNetwork(n, true); len(got) != 0 {
		t.Fatalf("only storage vNICs are in scope: %s", whys(got))
	}
	if len(n.StorageVNICs()) != 0 {
		t.Error("StorageVNICs must return only storage-purpose vNICs")
	}
}

// Which local address a session leaves through is decided by subnet, because
// that is the only thing that distinguishes one storage path from another.
func TestInitiatorForMatchesThePortalsSubnet(t *testing.T) {
	addrs := []string{"10.0.41.11/24", "10.0.42.11/24"}

	if got := InitiatorFor("10.0.42.10", addrs); got != "10.0.42.11" {
		t.Fatalf("the portal must be bound to the vNIC on its own subnet, got %q", got)
	}
	// A port is allowed on a portal and is not part of the address.
	if got := InitiatorFor("10.0.41.10:3260", addrs); got != "10.0.41.11" {
		t.Errorf("a portal with a port must still match, got %q", got)
	}
	// Nothing is guessed. Binding a session to the wrong source address does not
	// fail cleanly — it fails at login with a message about the array.
	if got := InitiatorFor("10.0.99.10", addrs); got != "" {
		t.Errorf("a portal no storage vNIC can reach must bind to nothing, got %q", got)
	}
	if got := InitiatorFor("not-an-address", addrs); got != "" {
		t.Errorf("an unparseable portal must bind to nothing, got %q", got)
	}
}

func TestStorageAddressesAreOnlyStorageVNICsThatHaveOne(t *testing.T) {
	n := converged("Ethernet1", "Ethernet2")
	n.ManagementVNICs = []ManagementVNICSpec{
		storageVNIC("SMB01", "Ethernet1", "10.0.41.11/24"),
		{Name: "SMB02", SwitchName: "ConvergedSwitch", Purpose: VNICStorage, TeamMemberAdapter: "Ethernet2"},
		{Name: "Mgmt", SwitchName: "ConvergedSwitch", IPConfig: &IPConfig{Address: "192.168.1.11/24"}},
	}
	got := n.StorageAddresses()
	if len(got) != 1 || got[0] != "10.0.41.11/24" {
		t.Fatalf("only addressed storage vNICs can be bound to: %v", got)
	}
}

// The adapter list must not shuffle. sortedFold exists precisely because "an
// alarm whose wording shuffles between passes looks like a new alarm", and this
// message was built with the raw spec order — HVNEW02 reported its choices as
// "Ethernet1, Ethernet3, Ethernet0, Ethernet2" on 2026-08-24, which reads as
// noise and changes whenever the spec is re-authored.
func TestTheAdaptersOfferedAreInAStableOrder(t *testing.T) {
	n := HostNetworkingSpec{
		Switches: []VirtualSwitchSpec{{Name: "ConvergedSwitch", TeamMembers: []string{"Ethernet1", "Ethernet3", "Ethernet0", "Ethernet2"}}},
		ManagementVNICs: []ManagementVNICSpec{
			{Name: "Storage01", SwitchName: "ConvergedSwitch", Purpose: VNICStorage, IPConfig: &IPConfig{Address: "10.0.60.72/24"}},
		},
	}
	got := whys(ValidateStorageNetwork(n, false))
	if !strings.Contains(got, "Ethernet0, Ethernet1, Ethernet2, Ethernet3") {
		t.Fatalf("the adapters must be offered in a stable order: %s", got)
	}
}
