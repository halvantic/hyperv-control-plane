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
		5*time.Minute, reconcile.SlowPassThreshold))

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
	r.notePassTiming(reconcile.SlowPassMessage(nil, time.Second, reconcile.SlowPassThreshold))

	st := r.buildStatus(types.HostInventory{}, types.HostMetrics{}, types.HostResources{},
		false, types.PhaseReady, nil, true, false, false)
	for _, c := range st.Conditions {
		if c.Type == "ReconcilePass" {
			t.Fatalf("a healthy cycle must clear the condition, got %q", c.Message)
		}
	}
}
