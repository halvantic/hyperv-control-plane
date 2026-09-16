package main

import (
	"time"

	"github.com/halvantic/hyperv-control-plane/agent/reconcile"
)

// phaseTimer accumulates how long each part of a cycle took, so "the cycle is
// slow" resolves to which part of it.
//
// Only phases that actually ran are reported. A cycle that skipped the inventory
// refresh should not log "inventory=0ms" beside one that collected it — the two
// mean opposite things, and a zero that means "did not run" is how a throttled
// observation gets mistaken for a fast one.
type phaseTimer struct {
	names []string
	took  map[string]time.Duration
}

// mark returns a function that records the elapsed time under name when called.
// Intended as `defer t.mark("inventory")()` at the top of a phase, or
// `done := t.mark("x"); ...; done()` where a defer would be too coarse.
func (t *phaseTimer) mark(name string) func() {
	start := time.Now()
	return func() {
		if t.took == nil {
			t.took = map[string]time.Duration{}
		}
		if _, seen := t.took[name]; !seen {
			t.names = append(t.names, name)
		}
		// Accumulate: a phase that runs per VM or per vNIC is reported as its
		// total, which is the figure that decides whether batching it is worth it.
		t.took[name] += time.Since(start)
	}
}

// fields renders the recorded phases as alternating key/value log arguments, in
// the order they first ran.
func (t *phaseTimer) fields() []any {
	out := make([]any, 0, len(t.names)*2)
	for _, n := range t.names {
		out = append(out, n, roundMS(t.took[n]))
	}
	return out
}

// roundMS trims a duration to whole milliseconds. Sub-millisecond precision on a
// phase that spawns powershell.exe is noise, and it makes the line harder to
// scan for the number that matters.
func roundMS(d time.Duration) time.Duration {
	return d.Round(time.Millisecond)
}

// timings renders the recorded phases for the status report, in the order they
// first ran; SlowPassMessage sorts and trims. Unlike fields() this is for the
// centre rather than the log — a slow pass has to be explainable without anyone
// logging on to the host that had it.
func (t *phaseTimer) timings() []reconcile.PhaseTiming {
	out := make([]reconcile.PhaseTiming, 0, len(t.names))
	for _, n := range t.names {
		out = append(out, reconcile.PhaseTiming{Name: n, Took: t.took[n]})
	}
	return out
}
