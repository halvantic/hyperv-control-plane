package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/agent/reconcile"
	"github.com/joshua-fourie/ballast/agent/store"
	"github.com/joshua-fourie/ballast/api/types"
)

/* A pass's cost and the list of calls that explain it must describe the same
   window.

   The accumulator is one bucket and a take claims everything since the last
   take, so whoever drains it has to be the thing timing it. That was not true:
   the timer ran around the host reconcile while the drain also collected the
   runner's own collections and the cluster reconcile that follows it. On the rig
   2026-08-13 a host reported "this pass took 1m14s — slowest: GetClusterState
   4m5s" — a four-minute call inside a one-minute pass, which cannot be read as
   anything and quietly moved a real four-minute cluster read onto the wrong
   cycle.

   The cycle is the only window that contains everything a pass does, so it owns
   both the timer and the drain. */

// drainCountingHV counts how often the timings accumulator is drained.
type drainCountingHV struct {
	*hyperv.Stub
	takes int
}

func (d *drainCountingHV) TakeTimings() []hyperv.CallTiming {
	d.takes++
	return d.Stub.TakeTimings()
}

func newDrainCountingRunner(t *testing.T) (*runner, *drainCountingHV) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hv := &drainCountingHV{Stub: &hyperv.Stub{}}
	return &runner{
		cfg:        runnerConfig{hostName: "host01"},
		log:        log,
		hv:         hv,
		st:         st,
		reconciler: reconcile.New(hv, log),
	}, hv
}

// One cycle, one drain — no more (which would split a pass's calls across two
// reports) and no fewer (which would leave them for the next cycle to claim).
func TestEachCycleDrainsTheTimingsExactlyOnce(t *testing.T) {
	r, hv := newDrainCountingRunner(t)
	fc := &fakeClient{errAll: true} // the centre being down must not change this

	for i := 1; i <= 3; i++ {
		r.cycle(context.Background(), fc)
		if hv.takes != i {
			t.Fatalf("after %d cycles the accumulator was drained %d times", i, hv.takes)
		}
	}
}

// A cycle can only be measured once it has ended, and status is reported partway
// through one — so the message necessarily travels on the following report. It
// must therefore not call itself "this pass", and it must reach the centre.
func TestTheFinishedPassIsReportedOnTheNextStatus(t *testing.T) {
	r, _ := newDrainCountingRunner(t)

	r.notePassTiming(reconcile.SlowPassMessage(
		[]hyperv.CallTiming{{Name: "GetClusterState", Took: 4*time.Minute + 5*time.Second, Calls: 1}},
		nil, 5*time.Minute, reconcile.SlowPassThreshold, false))

	st := r.buildStatus(types.HostInventory{}, types.HostMetrics{}, types.HostResources{},
		false, types.PhaseReady, nil, true, false, false)

	var msg string
	for _, c := range st.Conditions {
		if c.Type == "ReconcilePass" {
			msg = c.Message
		}
	}
	if msg == "" {
		t.Fatal("a slow cycle must reach the centre on the next report; no ReconcilePass condition was raised")
	}
	if !strings.Contains(msg, "the last completed pass") {
		t.Errorf("the message describes a pass that has ENDED and must not present itself as the one now running: %q", msg)
	}
	if !strings.Contains(msg, "GetClusterState 4m5s") {
		t.Errorf("the cluster read is part of a cycle and must be nameable in its cost: %q", msg)
	}
}

// And it clears itself: a host that recovers must stop carrying the condition,
// or the console shows a slow pass for ever after one bad cycle.
func TestARecoveredHostStopsReportingASlowPass(t *testing.T) {
	r, _ := newDrainCountingRunner(t)
	r.notePassTiming("the last completed pass took 5m0s.")
	r.notePassTiming(reconcile.SlowPassMessage(nil, nil, time.Second, reconcile.SlowPassThreshold, false))

	st := r.buildStatus(types.HostInventory{}, types.HostMetrics{}, types.HostResources{},
		false, types.PhaseReady, nil, true, false, false)
	for _, c := range st.Conditions {
		if c.Type == "ReconcilePass" {
			t.Fatalf("a healthy cycle must clear the condition, got %q", c.Message)
		}
	}
}

/* A wedged agent could not report what wedged it.

   A status is only BUILT partway through a cycle, so a cycle that runs out of
   its budget before reaching that point never builds one, and the keepalive goes
   on replaying the last good status for as long as the wedge lasts. HVNEW02,
   2026-08-14: readings eleven minutes old, conditions frozen at "HyperVRole not
   attempted — something earlier in the pass is slow", and the phase breakdown
   naming the slow phase sitting on the host where nobody could see it. The one
   diagnosis that answers "what is slow" was stuck behind the fault it
   diagnoses. */

func TestAReplayedStatusCarriesTheNewestPassMessage(t *testing.T) {
	old := types.HostStatus{Phase: types.PhaseReady, Conditions: []types.Condition{
		{Type: "HyperVRole", Status: false, Reason: "NotAttempted"},
		{Type: "ReconcilePass", Status: false, Message: "the last completed pass took 44s."},
	}}

	replay := withPassMessage(old, "the last pass was CUT OFF after 5m0s — it hit the cycle limit")

	var pass []types.Condition
	for _, c := range replay.Conditions {
		if c.Type == "ReconcilePass" {
			pass = append(pass, c)
		}
	}
	if len(pass) != 1 {
		t.Fatalf("a replay must carry exactly one pass condition, got %d", len(pass))
	}
	if !strings.Contains(pass[0].Message, "CUT OFF") {
		t.Errorf("the replay must carry the NEWEST message, not the one frozen in the old status: %q", pass[0].Message)
	}
	// Everything else about the replay is untouched — it is still the last real
	// reading and must not start looking like a fresh one.
	if len(replay.Conditions) != 2 {
		t.Errorf("the replay must not gain or lose other conditions: %+v", replay.Conditions)
	}
}

// With nothing new to say the replay is left exactly as it was.
func TestAReplayWithNoNewTimingIsUnchanged(t *testing.T) {
	old := types.HostStatus{Conditions: []types.Condition{{Type: "HyperVRole", Status: true}}}
	if got := withPassMessage(old, ""); len(got.Conditions) != 1 || got.Conditions[0].Type != "HyperVRole" {
		t.Fatalf("an empty message must change nothing: %+v", got.Conditions)
	}
}

/* And a cycle killed at the cap must not call itself completed. HVNEW03
   reported "took 5m0s" three passes running, and only the suspiciously round
   number gave away that all three had been killed by cycleTimeout rather than
   finishing slowly. They mean opposite things: one did all its work, the other
   did not, and everything after the stall never ran. */

func TestACutOffPassSaysSoRatherThanClaimingItFinished(t *testing.T) {
	msg := reconcile.SlowPassMessage(
		[]hyperv.CallTiming{{Name: "CollectInventory", Took: 4 * time.Minute, Calls: 1}},
		[]reconcile.PhaseTiming{{Name: "inventory", Took: 4 * time.Minute}},
		5*time.Minute, reconcile.SlowPassThreshold, true)

	if !strings.Contains(msg, "CUT OFF") || !strings.Contains(msg, "did not finish") {
		t.Fatalf("a killed cycle must not read as a completed one: %q", msg)
	}
	if strings.Contains(msg, "last completed pass") {
		t.Errorf("it did not complete: %q", msg)
	}
}

// A cut-off pass is worth reporting however short it ran — it did not finish,
// which the duration alone cannot say.
func TestAShortCutOffPassIsStillReported(t *testing.T) {
	if reconcile.SlowPassMessage(nil, nil, 3*time.Second, reconcile.SlowPassThreshold, true) == "" {
		t.Fatal("a cancelled cycle must report regardless of how long it ran")
	}
	// And an ordinary quick pass still says nothing at all.
	if reconcile.SlowPassMessage(nil, nil, 3*time.Second, reconcile.SlowPassThreshold, false) != "" {
		t.Error("a healthy quick pass must stay silent")
	}
}
