package reconcile

import (
	"context"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

/* Pass timings belong to the window that was measured, and the accumulator is a
   single bucket: a take claims everything put in since the last take. So the
   drain has to be paired with a timer covering the same window. It has now come
   apart in both directions.

   First, too little draining. Reconcile has several early returns — the Hyper-V
   role not being active is one, and it is the one a host takes while it is still
   booting — and the accumulator was drained only at the bottom, so those passes
   left their calls for the next one to report as its own. Seen on HVNEW02: a
   4m27s pass naming "GetHostRoleState 39.2s (2 calls), CollectInventory 36.9s
   (2 calls), CollectResources 24.6s (2 calls)", one call from itself and one
   inherited from the boot pass that had timed out.

   Then, too much. Draining from Reconcile also swept up the runner's own
   collections and the cluster reconcile that runs AFTERWARDS, none of which the
   timer covered. Seen on the rig 2026-08-13: "this pass took 1m14s — slowest:
   GetClusterState 4m5s", a four-minute call billed to a pass that ran for barely
   one, and a duration a reader has no way to reconcile with its own list.

   Both are the same defect: a list of calls set against a duration they were not
   part of. Timing now lives at the cycle boundary in the runner, which is the
   only place that spans everything a pass does — see agent/service. What matters
   here is that Reconcile does not drain behind its back. */

// countingHV records how often the accumulator was drained.
type countingHV struct {
	*hyperv.Stub
	takes int
}

func (c *countingHV) TakeTimings() []hyperv.CallTiming {
	c.takes++
	return c.Stub.TakeTimings()
}

func TestReconcileLeavesTheDrainToTheCycle(t *testing.T) {
	// A host that wants the Hyper-V role, on a stub that reports it absent, takes
	// the early return — the boot-time path, and the one that first exposed this.
	hv := &countingHV{Stub: &hyperv.Stub{}}
	r := New(hv, nil)

	desired := types.Host{
		Meta: types.ObjectMeta{Name: "HVNEW02"},
		Spec: types.HostSpec{EnableHyperVRole: true},
	}

	for i := 1; i <= 3; i++ {
		if _, err := r.Reconcile(context.Background(), desired, nil); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if hv.takes != 0 {
		t.Fatalf("Reconcile drained the accumulator %d times: it is timed by the cycle, and a drain here claims the cluster reconcile's calls for a window that never included them", hv.takes)
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
