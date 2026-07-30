package reconcile

import (
	"strings"
	"sync"
	"time"
)

// transientWindow is how long an operation may keep failing with a known
// in-flight signature before it stops being reported as settling and becomes a
// real failure.
//
// The window is the load-bearing part of this whole mechanism. Classifying an
// error as transient without one would manufacture the defect this codebase has
// spent its history removing: a condition that reads calm while nothing works.
// A disk that cannot attach for one pass is settling; the same disk failing for
// two minutes is broken, and the operator must be told.
const transientWindow = 2 * time.Minute

// transientSignature recognises errors that a LATER PASS is expected to clear
// with no operator action, and returns a plain explanation of what is in flight.
// It returns "" for anything not explicitly recognised — an unknown error is
// always a failure, never quietly softened.
//
// Every entry here must name a specific observed cause. The list is deliberately
// short: these are the two seen on real hardware, not a guess at what might be
// transient.
func transientSignature(err error) string {
	if err == nil {
		return ""
	}
	m := strings.ToLower(err.Error())
	switch {
	// Removing a Hyper-V Replica relationship merges the reference-point .avhdx
	// back into the base VHDX. Until that finishes the VM's configuration still
	// names a file that is being merged away, so attaching fails with "cannot
	// find the file specified". Observed live on a clustered VM immediately
	// after replication was removed; it cleared on the next pass by itself.
	case strings.Contains(m, ".avhdx") &&
		(strings.Contains(m, "cannot find the file") || strings.Contains(m, "not found")):
		return "a checkpoint or replication disk is still merging into the base VHDX"

	// During a live migration the VHDX is held open by the running VM, so a node
	// reconciling mid-migration sees the disk as unattached and cannot re-attach
	// it. Settles when the migration completes.
	case strings.Contains(m, "another process") || strings.Contains(m, "being used"):
		return "the disk is held open by a migration or a running VM"
	}
	return ""
}

// transientTracker remembers when each condition type first reported a transient
// error, so a persistent one can be escalated rather than reported as settling
// for ever. Keyed by condition type (e.g. "VM/Linux"), which is what an operator
// sees, so the elapsed time in the message matches the row they are looking at.
type transientTracker struct {
	mu    sync.Mutex
	first map[string]time.Time
}

func newTransientTracker() *transientTracker {
	return &transientTracker{first: map[string]time.Time{}}
}

// observe records a transient failure for condType and reports how long it has
// been failing, and whether that is still within the window. A condType that
// succeeds (or fails for a different reason) must be cleared, or a later,
// unrelated transient would inherit the old start time.
func (t *transientTracker) observe(condType string, now time.Time) (elapsed time.Duration, withinWindow bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	start, ok := t.first[condType]
	if !ok {
		t.first[condType] = now
		return 0, true
	}
	elapsed = now.Sub(start)
	return elapsed, elapsed < transientWindow
}

func (t *transientTracker) clear(condType string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.first, condType)
}
