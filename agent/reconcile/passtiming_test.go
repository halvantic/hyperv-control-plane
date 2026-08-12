package reconcile

import (
	"context"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

/* Pass timings belong to the pass that made them.

   Reconcile has several early returns — the Hyper-V role not being active is one,
   and it is the one a host takes while it is still booting. The accumulator was
   drained only at the bottom, so a pass that returned early left its calls behind
   and the NEXT completed pass reported them as its own.

   Seen live on HVNEW02: a pass that took 4m27s named "GetHostRoleState 39.2s
   (2 calls), CollectInventory 36.9s (2 calls), CollectResources 24.6s (2 calls)"
   — one call from itself and one inherited from the boot pass that had timed out
   and returned early. The whole point of timing the pass is to name the call that
   went slow; a list that includes another pass's work cannot do that. */

// countingHV records how often the accumulator was drained.
type countingHV struct {
	*hyperv.Stub
	takes int
}

func (c *countingHV) TakeTimings() []hyperv.CallTiming {
	c.takes++
	return c.Stub.TakeTimings()
}

func TestEveryPassDrainsItsOwnTimings(t *testing.T) {
	// A host that wants the Hyper-V role, on a stub that reports it absent, takes
	// the early return — the boot-time path.
	stub := &hyperv.Stub{}
	hv := &countingHV{Stub: stub}
	r := New(hv, nil)

	desired := types.Host{
		Meta: types.ObjectMeta{Name: "HVNEW02"},
		Spec: types.HostSpec{EnableHyperVRole: true},
	}

	for i := 1; i <= 3; i++ {
		if _, err := r.Reconcile(context.Background(), desired, nil); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if hv.takes != i {
			t.Fatalf("after %d passes the accumulator was drained %d times: a pass that does not drain leaves its calls for the next one to report as its own", i, hv.takes)
		}
	}
}
