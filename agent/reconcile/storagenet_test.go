package reconcile

import (
	"strings"
	"testing"
	"time"

	"github.com/joshua-fourie/ballast/api/types"
)

func fixedNow() time.Time { return time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC) }

func netWith(vnics ...types.ManagementVNICSpec) types.HostNetworkingSpec {
	return types.HostNetworkingSpec{
		Switches:        []types.VirtualSwitchSpec{{Name: "ConvergedSwitch", TeamMembers: []string{"Ethernet1", "Ethernet2"}}},
		ManagementVNICs: vnics,
	}
}

func storV(name, adapter, cidr string) types.ManagementVNICSpec {
	return types.ManagementVNICSpec{
		Name: name, SwitchName: "ConvergedSwitch", Purpose: types.VNICStorage,
		TeamMemberAdapter: adapter, IPConfig: &types.IPConfig{Address: cidr},
	}
}

// A host with local disks has nothing to say here, and a permanent "not
// applicable" row is noise on every host that will never have storage
// networking.
func TestNoStorageVNICsReportsNothing(t *testing.T) {
	n := netWith(types.ManagementVNICSpec{Name: "Mgmt", SwitchName: "ConvergedSwitch"})
	if got := storageNetworkCondition(n, true, fixedNow); len(got) != 0 {
		t.Fatalf("a host with no storage vNICs must not carry a condition: %+v", got)
	}
}

// The whole point: a correct arrangement says so, so an operator can tell the
// difference between checked-and-fine and never-looked-at.
func TestARealPairOfPathsIsReportedAsSuch(t *testing.T) {
	n := netWith(storV("SMB01", "Ethernet1", "10.0.41.11/24"), storV("SMB02", "Ethernet2", "10.0.42.11/24"))
	got := storageNetworkCondition(n, true, fixedNow)
	if len(got) != 1 || !got[0].Status {
		t.Fatalf("a correct layout must report healthy: %+v", got)
	}
	if got[0].Reason != "AlreadyConfigured" {
		t.Errorf("unexpected reason %q", got[0].Reason)
	}
	if !strings.Contains(got[0].Message, "2 storage vNICs across 2 physical adapters") {
		t.Errorf("it must say what it found: %q", got[0].Message)
	}
}

// Two paths, one cable. Everything else on the host reports success; this is the
// only thing that says the redundancy is decorative.
func TestSharedUplinkIsReportedWithoutFailingTheHost(t *testing.T) {
	n := netWith(storV("SMB01", "Ethernet1", "10.0.41.11/24"), storV("SMB02", "Ethernet1", "10.0.42.11/24"))
	got := storageNetworkCondition(n, true, fixedNow)
	if len(got) != 1 {
		t.Fatalf("expected one condition, got %+v", got)
	}
	if got[0].Reason != "NotRedundant" {
		t.Errorf("the reason must be machine-readable so the console can act on it: %q", got[0].Reason)
	}
	// Status stays true. A red host here would be an arrangement the operator may
	// have chosen, and it would hide the hosts that are actually broken.
	if !got[0].Status {
		t.Error("a working but non-redundant path must not fail the host")
	}
	if !strings.Contains(got[0].Message, "one path wearing two addresses") {
		t.Errorf("it must say what is actually wrong: %q", got[0].Message)
	}
}

// A cluster member always owes a second path — S2D over SMB Multichannel and a
// clustered array over MPIO both lose their shared storage with one link. A
// standalone host with one LUN over one link is an ordinary arrangement.
func TestWhoIsOwedASecondPath(t *testing.T) {
	member := types.Host{}
	member.Spec.ClusterMembership = &types.ClusterMembershipSpec{ClusterName: "S2DCluster"}
	if !wantsMultipathStorage(member) {
		t.Error("a cluster member is always owed a second path")
	}

	alone := types.Host{}
	if wantsMultipathStorage(alone) {
		t.Error("a host with no storage declared is owed nothing")
	}

	array := types.Host{}
	array.Spec.Storage.ISCSI = &types.ISCSIStorageSpec{Portals: []string{"10.0.41.10", "10.0.42.10"}}
	if want, _ := array.Spec.Storage.ISCSI.MPIORequired(); want != wantsMultipathStorage(array) {
		t.Error("a standalone host follows its own array's MPIO declaration, not a guess")
	}
}
