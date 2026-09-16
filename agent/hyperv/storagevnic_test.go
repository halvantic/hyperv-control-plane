package hyperv

import (
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* Pinning a vNIC to one uplink, and turning RDMA on.

   Both storage models multiplex across INTERFACES — SMB Multichannel for S2D,
   MPIO for iSCSI — and neither can manufacture an interface that is not there.
   Without a pin, SET places each vNIC on whichever team member it likes, so two
   storage vNICs on two subnets can share a single physical port. Everything
   above goes on reporting redundancy. Observed on the rig 2026-08-24: three
   hosts, three declared portals each, one path each, MPIO "in effect" with
   nothing to fail over to. */

func storageSpec(name, adapter string) types.ManagementVNICSpec {
	return types.ManagementVNICSpec{
		Name: name, SwitchName: "ConvergedSwitch",
		Purpose: types.VNICStorage, TeamMemberAdapter: adapter,
	}
}

func TestCreatingAStorageVNICPinsIt(t *testing.T) {
	got := createVNICScript(storageSpec("SMB01", "Ethernet1"))
	if !strings.Contains(got, "Set-VMNetworkAdapterTeamMapping -ManagementOS -VMNetworkAdapterName 'SMB01' -PhysicalNetAdapterName 'Ethernet1'") {
		t.Fatalf("a declared pin must be applied at create: %s", got)
	}
	// Set- cannot move a mapping that already exists, and errors instead. So the
	// old one goes first, every time, or the second pass fails.
	rm := strings.Index(got, "Remove-VMNetworkAdapterTeamMapping")
	set := strings.Index(got, "Set-VMNetworkAdapterTeamMapping")
	if rm < 0 || rm > set {
		t.Errorf("the existing mapping must be removed before the new one is set: %s", got)
	}
	if !strings.Contains(got, "Remove-VMNetworkAdapterTeamMapping -ManagementOS -VMNetworkAdapterName 'SMB01' -ErrorAction SilentlyContinue") {
		t.Errorf("removing a mapping that is not there must not fail the script: %s", got)
	}
}

// The same commands on update, or a pin only ever holds for vNICs Ballast
// happened to create.
func TestUpdatingAVNICAppliesThePin(t *testing.T) {
	if !strings.Contains(updateVNICScript(storageSpec("SMB01", "Ethernet2"), vnicObservation{Exists: true, Known: true, SwitchName: "ConvergedSwitch", TeamMember: "Ethernet9"}), "-PhysicalNetAdapterName 'Ethernet2'") {
		t.Fatal("update must apply the pin too")
	}
}

// A pin deliberately removed from the spec must actually come off the host.
// Leaving it would let the spec and the host disagree for ever, with the
// console showing the spec.
func TestClearingThePinUnpinsTheVNIC(t *testing.T) {
	got := createVNICScript(types.ManagementVNICSpec{Name: "Mgmt", SwitchName: "ConvergedSwitch"})
	if !strings.Contains(got, "Remove-VMNetworkAdapterTeamMapping -ManagementOS -VMNetworkAdapterName 'Mgmt'") {
		t.Fatalf("an undeclared pin must be cleared, not left: %s", got)
	}
	if strings.Contains(got, "Set-VMNetworkAdapterTeamMapping") {
		t.Errorf("nothing to pin to, so nothing must be set: %s", got)
	}
}

func TestRDMAIsOnlyTouchedWhenTheSpecHasAnOpinion(t *testing.T) {
	// Nil is not "off". Enabling RDMA on adapters that cannot do it — or, for
	// RoCE, without DCB on switches Ballast does not administer — produces a
	// fabric that works until it is loaded. So it is declared, never inferred.
	if got := createVNICScript(storageSpec("SMB01", "Ethernet1")); strings.Contains(got, "NetAdapterRdma") {
		t.Fatalf("an undeclared RDMA setting must leave the adapter alone: %s", got)
	}
	on, off := true, false
	spec := storageSpec("SMB01", "Ethernet1")

	spec.RDMA = &on
	if got := createVNICScript(spec); !strings.Contains(got, "Enable-NetAdapterRdma -Name 'vEthernet (SMB01)'") {
		t.Errorf("RDMA on must be applied to the projected interface: %s", got)
	}
	spec.RDMA = &off
	if got := createVNICScript(spec); !strings.Contains(got, "Disable-NetAdapterRdma -Name 'vEthernet (SMB01)'") {
		t.Errorf("RDMA off must be applied: %s", got)
	}
}

// Drift in the pin is drift. Nothing else can see it: the vNIC is up, addressed
// and carrying traffic either way, and only a failed cable tells you which.
func TestAnUnpinnedVNICIsDrift(t *testing.T) {
	spec := storageSpec("SMB01", "Ethernet1")
	settled := vnicObservation{Exists: true, Known: true, SwitchName: "ConvergedSwitch", TeamMember: "Ethernet1"}

	if planVNIC(spec, settled) != vnicNoop {
		t.Fatal("a vNIC already pinned where it was told must be left alone")
	}
	if planVNIC(spec, vnicObservation{Exists: true, Known: true, SwitchName: "ConvergedSwitch"}) != vnicUpdate {
		t.Error("a pin that was never applied must be reconciled")
	}
	wrong := settled
	wrong.TeamMember = "Ethernet2"
	if planVNIC(spec, wrong) != vnicUpdate {
		t.Error("a pin on the wrong adapter must be reconciled")
	}
	// Windows returns adapter names in whatever case it stored them; case alone
	// must not make a settled vNIC report drift on every pass for ever.
	cased := settled
	cased.TeamMember = "ethernet1"
	if planVNIC(spec, cased) != vnicNoop {
		t.Error("adapter names differing only in case are the same adapter")
	}
}

// The reconcile has to settle. An adapter with no RDMA capability reports
// nothing, and reading that as "off" would make every pass try to enable it,
// report Updated, and change nothing.
func TestRDMAOnAnIncapableAdapterDoesNotLoop(t *testing.T) {
	on := true
	spec := storageSpec("SMB01", "Ethernet1")
	spec.RDMA = &on
	obs := vnicObservation{Exists: true, Known: true, SwitchName: "ConvergedSwitch", TeamMember: "Ethernet1"}

	if planVNIC(spec, obs) != vnicNoop {
		t.Fatal("an adapter that cannot answer about RDMA must not be reconciled for ever")
	}
	// But it must not pass silently either: RDMA would read as declared and
	// settled while SMB Direct never engaged — storage that works, slowly, for
	// reasons nothing on the host explains.
	err := rdmaRefused(spec, obs)
	if err == nil {
		t.Fatal("a spec asking for RDMA an interface cannot do must be reported")
	}
	if !strings.Contains(err.Error(), "no RDMA capability") || !strings.Contains(err.Error(), "SMB Direct will not engage") {
		t.Errorf("it must say what is wrong and what it costs: %v", err)
	}
	// And where the adapter CAN answer, the observation decides, not the guess.
	obs.RDMAKnown = true
	if rdmaRefused(spec, obs) != nil {
		t.Error("an adapter that reports RDMA off is a difference to reconcile, not a refusal")
	}
	if planVNIC(spec, obs) != vnicUpdate {
		t.Error("RDMA declared on and observed off must be reconciled")
	}
	obs.RDMA = true
	if planVNIC(spec, obs) != vnicNoop {
		t.Error("RDMA already on must settle")
	}
}

// Observed, or the drift above is decided on a field nobody fills in.
func TestTheVNICObservationReadsThePinAndRDMA(t *testing.T) {
	script := vnicsBatchScript([]string{"SMB01"})
	if !strings.Contains(script, "Get-VMNetworkAdapterTeamMapping") {
		t.Error("the batched observation must read the pin")
	}
	if !strings.Contains(script, "Get-NetAdapterRdma") {
		t.Error("the batched observation must read RDMA")
	}
	// rdmaKnown separates "reports off" from "cannot report", which is the whole
	// reason the reconcile settles.
	if !strings.Contains(script, "rdmaKnown  = [bool]$rdma") {
		t.Error("the observation must distinguish an adapter that cannot answer from one that answers no")
	}
}
