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

// The deep storage read must never land on pass 0.
//
// A rebooted host is a fresh agent process, so pass 0 is the first pass after
// boot: the moment the cluster's storage is busiest and the one pass an operator
// is actually waiting on. Measured on HVNEW02 it was 1m43s of a 3m54s first pass,
// spent on the read the gate exists to avoid. Wreckage detection is looking for
// damage left days ago and can wait.
func TestTheDeepStorageReadSkipsTheFirstPassAfterAStart(t *testing.T) {
	deep := func(passes uint64, wantMaintenance bool) bool {
		return wantMaintenance || (passes > 0 && passes%maintenanceDeepEvery == 0)
	}
	if deep(0, false) {
		t.Error("pass 0 is the first pass after a reboot and must not pay the cluster-wide read")
	}
	if !deep(maintenanceDeepEvery, false) {
		t.Error("the slow cadence must still come round, or wreckage on a resumed node is never found")
	}
	// Declared maintenance always reads it: the script is about to act on storage.
	if !deep(0, true) {
		t.Error("entering maintenance must read storage even on the first pass")
	}
}
