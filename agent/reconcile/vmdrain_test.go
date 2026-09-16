package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* Draining a node moves its VMs off it. A pass that catches one mid-move asks
   Hyper-V for a VM that has already gone and gets "the object was not found. The
   object might have been deleted" — so the source host marked TestServer2025
   Degraded for migrating successfully, ninety seconds before HVNEW04's reboot had
   even begun.

   The VM is fine. It is somewhere else, which is what was asked for. */

type failingVM struct {
	*hyperv.Stub
}

func (f *failingVM) EnsureVM(_ context.Context, _ types.VM) (hyperv.VMEnsureResult, error) {
	return hyperv.VMEnsureResult{}, errContext("cannot enumerate VMs on this host (Hyper-V is not answering)")
}

type errContext string

func (e errContext) Error() string { return string(e) }

func vmFor(name string) types.VM {
	return types.VM{
		Meta: types.ObjectMeta{Name: name},
		Spec: types.VMSpec{Placement: types.VMPlacementSpec{ClusterName: "DRCluster"}},
	}
}

func TestAVMLeavingADrainingHostIsNotAFailure(t *testing.T) {
	r := New(&failingVM{Stub: &hyperv.Stub{}}, nil)
	r.hostDraining = true

	res := r.ReconcileVM(context.Background(), vmFor("TestServer2025"))

	if res.Phase == types.PhaseDegraded {
		t.Fatal("a VM migrating off a draining node must not be reported Degraded: it is doing what the drain asked")
	}
	var found bool
	for _, c := range res.Conditions {
		if c.Type == "VM/TestServer2025" {
			found = true
			if c.Reason != "Draining" {
				t.Errorf("want reason Draining, got %q: %q", c.Reason, c.Message)
			}
			if !strings.Contains(c.Message, "moving to another node") {
				t.Errorf("the condition must say where the VM went, not that a cmdlet failed: %q", c.Message)
			}
			if strings.Contains(c.Message, "powershell") {
				t.Errorf("a raw cmdlet error must not reach the operator here: %q", c.Message)
			}
		}
	}
	if !found {
		t.Fatal("the VM must still report a condition: going silent about it is how an operator loses track")
	}
}

// The gate is the drain, and only the drain. On a node that is not draining the
// same error is a real failure and must still read as one — otherwise this would
// hide every VM fault on every host.
func TestTheSameErrorOnAHealthyHostStillFails(t *testing.T) {
	r := New(&failingVM{Stub: &hyperv.Stub{}}, nil)
	r.hostDraining = false

	res := r.ReconcileVM(context.Background(), vmFor("TestServer2025"))

	if res.Phase != types.PhaseDegraded {
		t.Fatal("a VM that cannot be ensured on a node nobody is draining is a fault and must be reported as one")
	}
}
