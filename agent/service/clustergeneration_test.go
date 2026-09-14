package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/joshua-fourie/ballast/agent/store"
)

/* The cluster's last honoured generation is a fact that is KEPT, not a reading
   that is recomputed.

   Primary1 on the rig, 2026-09-14: the Desired state tab read "gen 0/8" — which
   the console renders as "the agent has not reported honouring generation 8
   yet", observedGeneration 0 meaning nothing has ever been honoured — for a
   cluster with three members reporting every single pass. Two declared iSCSI
   portals do not answer, so the reconcile never reports Honoured, and the agent
   sent ObservedGeneration 0 every time. The zero was not a transient state on
   the way to the truth; it WAS the steady state.

   Hosts and VMs already held this durably for exactly this reason. The cluster
   was the object left out of that work.

   These drive the real method, not a copy of its rules: a test that reimplements
   what it is checking can pass while the thing it checks fails. */

func TestTheClusterGenerationHoldsWhenAPassDoesNotHonour(t *testing.T) {
	r := newTestRunner(t)

	if got := r.clusterObservedGeneration("Primary1", 8, true); got != 8 {
		t.Fatalf("an honoured pass should record generation 8, got %d", got)
	}
	// The pass that follows cannot honour — the portals still do not answer. It
	// must report the fact that stands, not zero: zero is a different and false
	// claim, that nothing has ever been honoured.
	if got := r.clusterObservedGeneration("Primary1", 8, false); got != 8 {
		t.Fatalf("an unhonoured pass erased the last honoured generation: got %d, want 8", got)
	}
	// A new generation not yet honoured leaves the old fact standing. That is
	// "behind", which the console can say something useful about.
	if got := r.clusterObservedGeneration("Primary1", 9, false); got != 8 {
		t.Fatalf("a new unhonoured generation should leave 8 standing, got %d", got)
	}
	if got := r.clusterObservedGeneration("Primary1", 9, true); got != 9 {
		t.Fatalf("honouring 9 should advance to 9, got %d", got)
	}
}

// Never honoured really is zero. Holding the value is what lets zero keep its
// meaning — a cluster nothing has ever honoured — instead of being the answer
// for every cluster that is merely unsettled.
func TestAClusterNeverHonouredStillReportsZero(t *testing.T) {
	r := newTestRunner(t)
	if got := r.clusterObservedGeneration("Fresh", 3, false); got != 0 {
		t.Fatalf("a cluster that has never honoured anything should report 0, got %d", got)
	}
}

/*
Keyed by name, so a host that moves between clusters does not carry the old

	cluster's number across. A member leaving Primary1 for Secondary and reporting
	8 against Secondary would claim work that was never done on it.
*/
func TestOneClustersGenerationIsNotAnothers(t *testing.T) {
	r := newTestRunner(t)
	r.clusterObservedGeneration("Primary1", 8, true)
	if got := r.clusterObservedGeneration("Secondary", 2, false); got != 0 {
		t.Fatalf("Secondary should not inherit Primary1's honoured generation, got %d", got)
	}
	if got := r.clusterObservedGeneration("Primary1", 8, false); got != 8 {
		t.Fatalf("Primary1's own generation should be untouched by Secondary, got %d", got)
	}
}

/*
It survives a restart, which is the whole reason it lives in the store rather

	than on the runner. An agent that has just come back up has not re-established
	the fact, and a fact it cannot re-derive is one it must not discard.
*/
func TestTheHeldGenerationSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.db")

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	r := &runner{st: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r.clusterObservedGeneration("Primary1", 8, true)
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	st2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st2.Close()
	o, ok, err := st2.LoadObserved()
	if err != nil || !ok {
		t.Fatalf("load observed after restart: ok=%v err=%v", ok, err)
	}
	if o.Clusters["Primary1"] != 8 {
		t.Fatalf("the cluster generation did not survive the restart: %v", o.Clusters)
	}
}
