package reconcile

import (
	"context"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

// countingStub records how often the per-VM cluster-role query was reached.
type countingStub struct {
	*hyperv.Stub
	perVMRoleCalls int
	batchCalls     int
	batchFails     bool
}

func (c *countingStub) EnsureClusterVMRole(ctx context.Context, name string) (hyperv.Outcome, error) {
	c.perVMRoleCalls++
	return c.Stub.EnsureClusterVMRole(ctx, name)
}

func (c *countingStub) ClusterVMRolesPresent(ctx context.Context, names []string) (map[string]bool, error) {
	c.batchCalls++
	if c.batchFails {
		return nil, context.DeadlineExceeded
	}
	return c.Stub.ClusterVMRolesPresent(ctx, names)
}

func clusteredVMs(n int) []types.VM {
	out := make([]types.VM, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, types.VM{
			Meta: types.ObjectMeta{Name: string(rune('A'+i)) + "vm", Generation: 1},
			Spec: types.VMSpec{Placement: types.VMPlacementSpec{ClusterName: "cl1", HostName: "host01"}},
		})
	}
	return out
}

// EnsureClusterVMRole enumerates every cluster group to answer a question about
// one VM — 2437ms on the rig — so asking per VM re-read the same cluster-wide
// list once per VM. On a thirty-VM host that is 73s of a single reconcile spent
// confirming what the previous VM had just confirmed.
func TestClusterRolesAreReadOncePerPassNotOncePerVM(t *testing.T) {
	stub := &countingStub{Stub: &hyperv.Stub{}}
	r := testReconciler(stub)

	res := r.ReconcileVMs(context.Background(), clusteredVMs(10), true)

	if len(res) != 10 {
		t.Fatalf("every VM must still be reconciled, got %d", len(res))
	}
	if stub.batchCalls != 1 {
		t.Fatalf("the cluster-wide role list must be read exactly once, got %d", stub.batchCalls)
	}
	if stub.perVMRoleCalls != 0 {
		t.Fatalf("no per-VM cluster query should remain when the batch answered: %d calls", stub.perVMRoleCalls)
	}
	// The condition still reports per VM: an operator reading status must not
	// have to know which internal path produced it.
	for _, vr := range res {
		if _, ok := reasonByType(vr.Conditions, "VMClusterRole/"+vr.Name); !ok {
			t.Fatalf("%s lost its VMClusterRole condition to the batch", vr.Name)
		}
	}
}

// The batch is an optimisation. If it fails, every VM must fall back to asking
// for itself — the old behaviour — rather than the reconcile failing.
func TestFailedBatchFallsBackToPerVMRoleQueries(t *testing.T) {
	stub := &countingStub{Stub: &hyperv.Stub{}, batchFails: true}
	r := testReconciler(stub)

	res := r.ReconcileVMs(context.Background(), clusteredVMs(3), true)

	if stub.perVMRoleCalls != 3 {
		t.Fatalf("a failed batch must fall back to one query per VM, got %d", stub.perVMRoleCalls)
	}
	for _, vr := range res {
		if vr.Phase == types.PhaseDegraded {
			t.Fatalf("%s must not degrade because an optimisation failed", vr.Name)
		}
	}
}

// A standalone VM has no cluster role, so the batch must not be consulted for
// one — and must not be issued at all on a host with no clustered VMs.
func TestStandaloneVMsDoNotTriggerAClusterQuery(t *testing.T) {
	stub := &countingStub{Stub: &hyperv.Stub{}}
	r := testReconciler(stub)

	vms := []types.VM{{
		Meta: types.ObjectMeta{Name: "solo", Generation: 1},
		Spec: types.VMSpec{Placement: types.VMPlacementSpec{HostName: "host01"}},
	}}
	res := r.ReconcileVMs(context.Background(), vms, true)

	if stub.batchCalls != 0 {
		t.Fatalf("a host with no clustered VMs must not query the cluster, got %d", stub.batchCalls)
	}
	if stub.perVMRoleCalls != 0 {
		t.Fatalf("a standalone VM has no cluster role to ensure, got %d", stub.perVMRoleCalls)
	}
	if _, ok := reasonByType(res[0].Conditions, "VMClusterRole/solo"); ok {
		t.Fatal("a standalone VM must not report a cluster-role condition")
	}
}

// ReconcileVM on its own — the single-VM entry point — must still work, and
// must still ask, since nobody has answered for it.
func TestSingleVMPathStillQueriesForItself(t *testing.T) {
	stub := &countingStub{Stub: &hyperv.Stub{}}
	r := testReconciler(stub)

	r.ReconcileVM(context.Background(), clusteredVMs(1)[0])

	if stub.batchCalls != 0 {
		t.Fatalf("the single-VM path should not issue a batch, got %d", stub.batchCalls)
	}
	if stub.perVMRoleCalls != 1 {
		t.Fatalf("the single-VM path must ensure its own role, got %d", stub.perVMRoleCalls)
	}
}
