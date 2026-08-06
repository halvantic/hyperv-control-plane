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

	// A Cluster Shared Volume that is not Online cannot be written to. It goes
	// OnlinePending or Detached while the storage pool rebuilds, and comes back
	// by itself — so a pass or two of this is normal recovery, not a fault.
	//
	// It is here rather than reported as an error because the raw symptom is
	// "Access to the path ... is denied", which sends the operator to check
	// permissions on something whose permissions are fine. Note the window still
	// applies: a volume that has not come back after transientWindow escalates,
	// because a CSV stuck offline is a real problem and the point of that window
	// is that nothing waits for ever in silence.
	case strings.Contains(m, "to come back online"):
		return "a cluster volume it needs is not online yet"

	// Enabling replication to a cluster involves a target that is itself still
	// settling: the Replica Broker's client access point has to be online, its
	// name resolvable, and the owning node ready to place the replica. Ask a pass
	// too early and Hyper-V answers "failed to enable replication" — a sentence
	// that describes no cause and applies to every one of those.
	//
	// Observed 2026-08-06: 'Linux' failed exactly this way against bcluster2-Brk
	// and was Replicating with health Normal after a later pass, with nothing
	// changed in between. So the first passes are settling, not a fault.
	//
	// This deliberately sits ahead of the storage diagnosis EnsureVMReplication
	// attaches to the same error. That diagnosis is right when the relationship
	// never establishes, and wrong — confidently, which is worse — when the
	// target was merely not ready. The window still applies: past it, the real
	// message escalates with the storage detail intact, because replication that
	// never starts is a genuine fault and nothing may wait for ever in silence.
	case strings.Contains(m, "failed to enable replication"):
		return "the replica target is not ready to accept the relationship yet"
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
