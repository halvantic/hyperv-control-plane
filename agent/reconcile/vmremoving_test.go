package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

// ensureCountingStub records whether the reconciler tried to create a VM.
type ensureCountingStub struct {
	*hyperv.Stub
	ensureCalls int
}

func (c *ensureCountingStub) EnsureVM(ctx context.Context, vm types.VM) (hyperv.VMEnsureResult, error) {
	c.ensureCalls++
	return c.Stub.EnsureVM(ctx, vm)
}

// A VM a RemoveVM job has just deleted must not be recreated by a pass that is
// still walking the desired set it loaded before the delete. This is the whole
// failure: the job removed the cluster role and the VM, the pass put both back,
// and the VM an operator deleted was in the cluster again minutes later.
func TestReconcileDoesNotRecreateAVMBeingDeleted(t *testing.T) {
	stub := &ensureCountingStub{Stub: &hyperv.Stub{}}
	r := testReconciler(stub)
	r.SetVMBusy(func(name string) (string, bool) {
		return types.JobRemoveVM, strings.EqualFold(name, "Tes")
	})

	res := r.ReconcileVMs(context.Background(), []types.VM{
		{Meta: types.ObjectMeta{Name: "Tes", Generation: 1},
			Spec: types.VMSpec{Placement: types.VMPlacementSpec{ClusterName: "cl1", HostName: "host01"}}},
	}, true)

	if len(res) != 1 {
		t.Fatalf("want one result, got %d", len(res))
	}
	if stub.ensureCalls != 0 {
		t.Fatalf("a VM being deleted must not be created: %d EnsureVM calls", stub.ensureCalls)
	}
	// Progressing, not Degraded: nothing is wrong, the removal is simply not
	// settled yet.
	if res[0].Phase != types.PhaseProgressing {
		t.Errorf("want Progressing while the removal settles, got %q", res[0].Phase)
	}
	if len(res[0].Conditions) != 1 {
		t.Fatalf("want one condition explaining the stand-off, got %d", len(res[0].Conditions))
	}
	c := res[0].Conditions[0]
	// The reason an operator reads must be the delete, not a generic "a job is
	// running" — the two call for different reactions.
	if c.Reason != "Removing" {
		t.Errorf("want reason Removing, got %q", c.Reason)
	}
	if !strings.Contains(c.Message, "being deleted") {
		t.Errorf("the message must say the VM is being deleted, got %q", c.Message)
	}
}
