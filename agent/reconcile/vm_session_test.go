package reconcile

import (
	"context"
	"testing"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

// sessionStub records the session a pass opens and which context each VM's
// EnsureVM ran under.
type sessionStub struct {
	*hyperv.Stub
	opened, closed int
	inSession      []bool
	order          []string
}

type testSessionKey struct{}

func (s *sessionStub) Session(ctx context.Context) (context.Context, func()) {
	s.opened++
	return context.WithValue(ctx, testSessionKey{}, true), func() { s.closed++ }
}

func (s *sessionStub) EnsureVM(ctx context.Context, vm types.VM) (hyperv.VMEnsureResult, error) {
	s.inSession = append(s.inSession, ctx.Value(testSessionKey{}) == true)
	s.order = append(s.order, vm.Meta.Name)
	return s.Stub.EnsureVM(ctx, vm)
}

func namedVMs(names ...string) []types.VM {
	out := make([]types.VM, 0, len(names))
	for _, n := range names {
		out = append(out, types.VM{
			Meta: types.ObjectMeta{Name: n, Generation: 1},
			Spec: types.VMSpec{Placement: types.VMPlacementSpec{HostName: "host01"}},
		})
	}
	return out
}

// One process for the pass, closed when the pass ends, and every VM's EnsureVM
// sent through it: the warm-up is paid once, not once per VM.
func TestAPassSharesOneSessionAndClosesIt(t *testing.T) {
	stub := &sessionStub{Stub: &hyperv.Stub{}}
	r := testReconciler(stub)

	r.ReconcileVMs(context.Background(), namedVMs("A", "B", "C"), false)

	if stub.opened != 1 || stub.closed != 1 {
		t.Fatalf("a pass must open one session and close it: opened=%d closed=%d", stub.opened, stub.closed)
	}
	for i, in := range stub.inSession {
		if !in {
			t.Fatalf("VM %d's EnsureVM did not run under the pass's session", i)
		}
	}
}

// The session changes where scripts run, not the order the pass takes: each VM
// is still reconciled in turn, after its own checks.
func TestASessionKeepsThePerVMOrder(t *testing.T) {
	stub := &sessionStub{Stub: &hyperv.Stub{}}
	r := testReconciler(stub)

	r.ReconcileVMs(context.Background(), namedVMs("C", "A", "B"), false)

	if got := len(stub.order); got != 3 || stub.order[0] != "C" || stub.order[1] != "A" || stub.order[2] != "B" {
		t.Fatalf("VMs must be ensured in their own order, one at a time: %v", stub.order)
	}
}

// A VM a job holds is still stood off with a session open: it is checked when
// its turn comes, and never sent.
func TestASessionStillStandsOffABusyVM(t *testing.T) {
	stub := &sessionStub{Stub: &hyperv.Stub{}}
	r := testReconciler(stub)
	r.vmBusy = func(name string) (string, bool) { return types.JobRemoveVM, name == "B" }

	res := r.ReconcileVMs(context.Background(), namedVMs("A", "B", "C"), false)

	for _, n := range stub.order {
		if n == "B" {
			t.Fatal("a VM being removed must not be ensured, session or not")
		}
	}
	if res[1].Phase != types.PhaseProgressing {
		t.Fatalf("the held VM must report Progressing, got %s", res[1].Phase)
	}
}
