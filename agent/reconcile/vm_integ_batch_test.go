package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

// integStub counts which integration-services path each VM took.
type integStub struct {
	*hyperv.Stub
	batchCalls  int
	batchFails  bool
	batchOmits  string // a VM the batch leaves out, as if it had not been read
	ensureCalls int
	applyCalls  int
}

func (s *integStub) GetIntegrationServicesBatch(ctx context.Context, names []string) (map[string][]hyperv.IntegrationServiceState, error) {
	s.batchCalls++
	if s.batchFails {
		return nil, context.DeadlineExceeded
	}
	got, err := s.Stub.GetIntegrationServicesBatch(ctx, names)
	delete(got, strings.ToLower(s.batchOmits))
	return got, err
}

func (s *integStub) EnsureIntegrationServices(ctx context.Context, vm string, want *types.VMIntegrationServices) (hyperv.Outcome, error) {
	s.ensureCalls++
	return s.Stub.EnsureIntegrationServices(ctx, vm, want)
}

func (s *integStub) ApplyIntegrationServices(ctx context.Context, vm string, want *types.VMIntegrationServices, have []hyperv.IntegrationServiceState) (hyperv.Outcome, error) {
	s.applyCalls++
	return s.Stub.ApplyIntegrationServices(ctx, vm, want, have)
}

func integVMs(n int) []types.VM {
	on := true
	out := make([]types.VM, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, types.VM{
			Meta: types.ObjectMeta{Name: string(rune('A'+i)) + "vm", Generation: 1},
			Spec: types.VMSpec{
				Placement:           types.VMPlacementSpec{HostName: "host01"},
				IntegrationServices: &types.VMIntegrationServices{GuestServiceInterface: &on},
			},
		})
	}
	return out
}

// Each per-VM read was a PowerShell process of its own for one cmdlet: 7s of a
// pass on HVNEW06 for seven VMs. One read for the set, however many there are.
func TestIntegrationServicesAreReadOncePerPassNotOncePerVM(t *testing.T) {
	stub := &integStub{Stub: &hyperv.Stub{}}
	r := testReconciler(stub)

	res := r.ReconcileVMs(context.Background(), integVMs(7), false)

	if stub.batchCalls != 1 {
		t.Fatalf("the services must be read once for the set, got %d batch reads", stub.batchCalls)
	}
	if stub.ensureCalls != 0 || stub.applyCalls != 7 {
		t.Fatalf("every VM must apply against the batch reading: ensure=%d apply=%d", stub.ensureCalls, stub.applyCalls)
	}
	// Still applied, and still reported per VM: the batch changes how the state
	// is read, not what is done with it.
	if len(stub.IntegrationWrites) != 7 {
		t.Fatalf("each VM's declared service must still be applied, got writes %v", stub.IntegrationWrites)
	}
	for _, vr := range res {
		if _, ok := reasonByType(vr.Conditions, "IntegrationServices/"+vr.Name); !ok {
			t.Fatalf("%s lost its IntegrationServices condition to the batch", vr.Name)
		}
	}
}

// The batch is an optimisation. If it fails, every VM reads for itself, the
// behaviour before it existed, and nothing degrades.
func TestAFailedIntegrationBatchFallsBackToPerVM(t *testing.T) {
	stub := &integStub{Stub: &hyperv.Stub{}, batchFails: true}
	r := testReconciler(stub)

	res := r.ReconcileVMs(context.Background(), integVMs(3), false)

	if stub.ensureCalls != 3 || stub.applyCalls != 0 {
		t.Fatalf("a failed batch must fall back to each VM reading itself: ensure=%d apply=%d", stub.ensureCalls, stub.applyCalls)
	}
	for _, vr := range res {
		if vr.Phase == types.PhaseDegraded {
			t.Fatalf("%s must not degrade because an optimisation failed", vr.Name)
		}
	}
}

// A VM the batch did not read (one EnsureVM is about to create, say) reads for
// itself. Absent from the reading is not "has no services".
func TestAVMMissingFromTheBatchReadsForItself(t *testing.T) {
	stub := &integStub{Stub: &hyperv.Stub{}, batchOmits: "Bvm"}
	r := testReconciler(stub)

	r.ReconcileVMs(context.Background(), integVMs(3), false)

	if stub.ensureCalls != 1 || stub.applyCalls != 2 {
		t.Fatalf("only the VM the batch missed should read for itself: ensure=%d apply=%d", stub.ensureCalls, stub.applyCalls)
	}
}

// Nothing declared, nothing read: the batch is not issued at all.
func TestNoDeclaredServicesIssueNoBatch(t *testing.T) {
	stub := &integStub{Stub: &hyperv.Stub{}}
	r := testReconciler(stub)

	vms := integVMs(2)
	for i := range vms {
		vms[i].Spec.IntegrationServices = nil
	}
	r.ReconcileVMs(context.Background(), vms, false)

	if stub.batchCalls != 0 || stub.ensureCalls != 0 || stub.applyCalls != 0 {
		t.Fatalf("no VM declares services, so none may be read: batch=%d ensure=%d apply=%d",
			stub.batchCalls, stub.ensureCalls, stub.applyCalls)
	}
}
