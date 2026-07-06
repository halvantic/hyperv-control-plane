package reconcile

import (
	"context"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

func vmDesired(power types.VMPowerState) types.VM {
	return types.VM{
		Meta: types.ObjectMeta{Name: "web01", Generation: 2},
		Spec: types.VMSpec{
			Placement:          types.VMPlacementSpec{HostName: "host01"},
			HyperVGeneration:   2,
			ProcessorCount:     2,
			MemoryStartupBytes: 2_147_483_648,
			NetworkAdapters: []types.VMNetworkAdapterSpec{
				{Name: "net0", SwitchName: "ConvergedSwitch"},
			},
			DesiredPowerState: power,
		},
	}
}

// stubWithSwitch returns a stub that already has the VM's switch, since the
// networking reconcile runs before the VM reconcile in the real loop.
func stubWithSwitch() *hyperv.Stub {
	s := &hyperv.Stub{}
	_, _ = s.EnsureSwitch(context.Background(), types.VirtualSwitchSpec{Name: "ConvergedSwitch"})
	return s
}

// A fresh VM converges (created, powered on, honoured) and a second identical
// pass is a no-op. This is the VM-side idempotency guarantee.
func TestReconcileVMFreshThenIdempotent(t *testing.T) {
	stub := stubWithSwitch()
	r := testReconciler(stub)
	desired := vmDesired(types.VMPowerRunning)

	res := r.ReconcileVM(context.Background(), desired)
	if !res.Honoured || res.Phase != types.PhaseReady || !res.Changed {
		t.Fatalf("first pass: want honoured/ready/changed, got %+v", res)
	}
	if !stub.HasVM("web01") {
		t.Fatal("VM should have been created")
	}
	if res.PowerState != types.VMPowerRunning {
		t.Fatalf("VM should be running, got %q", res.PowerState)
	}
	if reason, _ := reasonByType(res.Conditions, "VM/web01"); reason != "Created" {
		t.Fatalf("VM reason: want Created, got %q", reason)
	}

	res2 := r.ReconcileVM(context.Background(), desired)
	if !res2.Honoured || res2.Phase != types.PhaseReady || res2.Changed {
		t.Fatalf("second pass: want honoured/ready/unchanged, got %+v", res2)
	}
	if reason, _ := reasonByType(res2.Conditions, "VM/web01"); reason != "AlreadyConfigured" {
		t.Fatalf("second pass VM reason: want AlreadyConfigured, got %q", reason)
	}
}

// DesiredPowerState=Off leaves the freshly-created VM off (created VMs come up
// off), and — since power is imperative, only the initial Running state is
// driven at creation — no power action is taken at all.
func TestReconcileVMDesiredOff(t *testing.T) {
	stub := stubWithSwitch()
	res := testReconciler(stub).ReconcileVM(context.Background(), vmDesired(types.VMPowerOff))
	if !res.Honoured || res.PowerState != types.VMPowerOff {
		t.Fatalf("want honoured/off, got %+v", res)
	}
	if _, ok := reasonByType(res.Conditions, "VMPower/web01"); ok {
		t.Fatal("a created-Off VM should get no VMPower condition (power is imperative)")
	}
}

// Power is not re-enforced: a VM whose guest stopped (observed Off) while its
// spec still says Running is NOT restarted by the reconcile loop.
func TestReconcileVMPowerNotEnforced(t *testing.T) {
	stub := stubWithSwitch()
	r := testReconciler(stub)
	// Create + initial power-on.
	r.ReconcileVM(context.Background(), vmDesired(types.VMPowerRunning))
	// Simulate the guest/operator stopping it out-of-band.
	_, _ = stub.SetVMPowerState(context.Background(), "web01", types.VMPowerOff)
	// A steady-state reconcile must not start it again.
	res := r.ReconcileVM(context.Background(), vmDesired(types.VMPowerRunning))
	if res.PowerState != types.VMPowerOff {
		t.Fatalf("reconcile must not restart a manually-stopped VM; got %q", res.PowerState)
	}
	if _, ok := reasonByType(res.Conditions, "VMPower/web01"); ok {
		t.Fatal("steady-state reconcile should take no power action")
	}
}

// A failed EnsureVM is Degraded and not honoured, and power is not driven.
func TestReconcileVMEnsureFailureIsDegraded(t *testing.T) {
	stub := stubWithSwitch()
	stub.FailVM = "web01"
	res := testReconciler(stub).ReconcileVM(context.Background(), vmDesired(types.VMPowerRunning))
	if res.Honoured || res.Phase != types.PhaseDegraded {
		t.Fatalf("want degraded/not-honoured, got %+v", res)
	}
	if reason, _ := reasonByType(res.Conditions, "VM/web01"); reason != "ApplyFailed" {
		t.Fatalf("VM reason: want ApplyFailed, got %q", reason)
	}
}

// A processor/memory change requested while the VM is running cannot be applied
// (Hyper-V forbids it). The reconciler surfaces it as Progressing + a
// RequiresPowerOff condition and does NOT mark the VM honoured, so its
// ObservedGeneration holds back until the VM is stopped — rather than erroring.
func TestReconcileVMSizingChangeWhileRunningIsPending(t *testing.T) {
	stub := stubWithSwitch()
	r := testReconciler(stub)

	// Create at 2 vCPU and power on.
	r.ReconcileVM(context.Background(), vmDesired(types.VMPowerRunning))

	// Now request 4 vCPU while it is running.
	bigger := vmDesired(types.VMPowerRunning)
	bigger.Spec.ProcessorCount = 4
	res := r.ReconcileVM(context.Background(), bigger)

	if res.Honoured || res.Phase != types.PhaseProgressing {
		t.Fatalf("want progressing/not-honoured, got %+v", res)
	}
	if reason, _ := reasonByType(res.Conditions, "VMConfig/web01"); reason != "RequiresPowerOff" {
		t.Fatalf("want RequiresPowerOff condition, got %q", reason)
	}
}

// ReconcileVMs returns one result per VM, in order.
func TestReconcileVMsMultiple(t *testing.T) {
	stub := stubWithSwitch()
	a := vmDesired(types.VMPowerRunning)
	a.Meta.Name = "web01"
	b := vmDesired(types.VMPowerRunning)
	b.Meta.Name = "web02"

	results := testReconciler(stub).ReconcileVMs(context.Background(), []types.VM{a, b})
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if results[0].Name != "web01" || results[1].Name != "web02" {
		t.Fatalf("results out of order: %q, %q", results[0].Name, results[1].Name)
	}
	for _, res := range results {
		if !res.Honoured {
			t.Fatalf("%s not honoured: %+v", res.Name, res)
		}
	}
}
