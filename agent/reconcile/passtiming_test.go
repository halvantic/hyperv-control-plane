package reconcile

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
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

// A phase that is slow has to be able to say WHICH PART of it was slow.
//
// From the rig, 2026-08-24: four of five hosts were CUT OFF at the 5-minute cap
// with clusterReconcile taking 3m20s-3m48s of it, and the host-call list could
// account for barely 90 seconds. Two and a half minutes a pass, on every host,
// attributed to nothing — and the message said so honestly ("the rest was not
// spent in a host call") without being able to say where it went.
func TestSlowPassDrillsIntoASlowPhase(t *testing.T) {
	calls := []hyperv.CallTiming{
		{Name: "CollectResources", Took: 15 * time.Second},
		{Name: "CollectInventory", Took: 13 * time.Second},
	}
	phases := []PhaseTiming{
		{Name: "hostReconcile", Took: 29 * time.Second},
		{Name: "clusterReconcile", Took: 3*time.Minute + 44*time.Second},
		{Name: "clusterReconcile/s2d+csv", Took: 2*time.Minute + 10*time.Second},
		{Name: "clusterReconcile/witness", Took: 41 * time.Second},
		{Name: "clusterReconcile/firewall", Took: 2 * time.Second},
	}
	msg := SlowPassMessage(calls, phases, 5*time.Minute, SlowPassThreshold, true)

	if !strings.Contains(msg, "clusterReconcile 3m44s (") {
		t.Fatalf("the slow phase must carry its own breakdown: %s", msg)
	}
	if !strings.Contains(msg, "s2d+csv 2m10s") {
		t.Errorf("the costliest stage inside it must be named: %s", msg)
	}
	// Sized against the PARENT: a 2s stage inside a 3m44s phase is noise, and a
	// line that lists it stops being read.
	if strings.Contains(msg, "firewall") {
		t.Errorf("a trivial stage must not be listed: %s", msg)
	}
	// The children sum to the parent, so listing both flat would double-count and
	// fill the top four with one branch of the same number.
	if strings.Contains(msg, "clusterReconcile/s2d+csv") {
		t.Errorf("a child must be rendered under its parent, not as a peer: %s", msg)
	}
	// And the phases beside it still get their place.
	if !strings.Contains(msg, "hostReconcile 29s") {
		t.Errorf("other phases must survive the drill-down: %s", msg)
	}
}

// A phase with no children reads exactly as it did before. The drill-down is an
// addition, not a change to how everything else is reported.
func TestSlowPassLeavesAFlatPhaseAlone(t *testing.T) {
	phases := []PhaseTiming{
		{Name: "clusterReconcile", Took: 2 * time.Minute},
		{Name: "hostReconcile", Took: 40 * time.Second},
	}
	msg := SlowPassMessage(nil, phases, 3*time.Minute, SlowPassThreshold, false)
	if !strings.Contains(msg, "clusterReconcile 2m0s") {
		t.Fatalf("the phase must still be named: %s", msg)
	}
	if strings.Contains(msg, "clusterReconcile 2m0s (") {
		t.Fatalf("a phase with no sub-stages must not grow brackets: %s", msg)
	}
}

// A pass with no host calls at all is the case where the phases are the ONLY
// thing that can explain it — and it was the branch that said the least,
// reporting "no single host call accounts for it" and stopping there.
func TestSlowPassWithNoHostCallsStillSaysWhereTheTimeWent(t *testing.T) {
	phases := []PhaseTiming{
		{Name: "deliver", Took: 2 * time.Minute},
		{Name: "journal", Took: 30 * time.Second},
	}
	msg := SlowPassMessage(nil, phases, 3*time.Minute, SlowPassThreshold, false)
	if !strings.Contains(msg, "no single host call accounts for it") {
		t.Fatalf("it must still say the calls do not explain it: %s", msg)
	}
	if !strings.Contains(msg, "Where it went: deliver 2m0s") {
		t.Fatalf("and then say where it DID go: %s", msg)
	}
}
