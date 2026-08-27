package vmware

import (
	"errors"
	"strings"
	"testing"
)

/* The failures a migration has to tell apart.

   Both of these are "changed block tracking went wrong" as far as vCenter is
   concerned, and they have opposite remedies: one needs the VM restarted, the
   other needs the disk read in full and nothing else. Getting them the wrong
   way round sends a migration to enable something already enabled and fail
   again identically, or restarts a guest that never needed it. */

func TestACBTResetIsRecognisedAndRecoverable(t *testing.T) {
	resets := []string{
		"ServerFaultCode: A specified parameter was not correct: changeId",
		"The change tracking has been reset for this disk",
		"InvalidArgument: invalid change id",
	}
	for _, msg := range resets {
		err := explainCBTError(errors.New(msg), "52 de c0 d3/7")
		if !errors.Is(err, ErrCBTReset) {
			t.Errorf("%q was not recognised as a reset: %v", msg, err)
		}
		// The remedy has to be in the words, not only in the sentinel: this text
		// is what an operator reads on the job.
		if !strings.Contains(err.Error(), "read in full again") {
			t.Errorf("%q does not say what happens next: %v", msg, err)
		}
		// And the original must survive, or a support call has nothing to go on.
		if !strings.Contains(err.Error(), msg) {
			t.Errorf("the underlying error was dropped from: %v", err)
		}
	}
}

func TestCBTNotEnabledIsNotMistakenForAReset(t *testing.T) {
	err := explainCBTError(errors.New("ServerFaultCode: Change tracking is not enabled for this virtual machine"), "*")
	if errors.Is(err, ErrCBTReset) {
		t.Fatal("a VM without CBT was treated as a reset, which would retry instead of restarting it")
	}
	if !strings.Contains(err.Error(), "power-on") {
		t.Errorf("the failure does not say why enabling it was not enough: %v", err)
	}
}

// Anything else is passed through unchanged rather than guessed at. A wrong
// explanation is worse than none: it sends the operator somewhere else.
func TestAnUnrelatedFailureIsNotDressedUp(t *testing.T) {
	orig := errors.New("dial tcp 10.0.0.9:443: connect: connection refused")
	if got := explainCBTError(orig, "*"); got != orig {
		t.Errorf("an unrelated error was rewritten as %v", got)
	}
}

/* Removing a snapshot that has already gone is the normal case on a cleanup
   path — the caller is tidying after a failure and does not know how far the
   previous attempt got. Every phrasing govmomi and vCenter use for it must read
   as success, or cleanup reports a failure that is not one. */
func TestASnapshotThatHasAlreadyGoneIsNotAFailure(t *testing.T) {
	gone := []string{
		`snapshot "snapshot-4021" not found`,
		"no snapshots for this VM",
		"ServerFaultCode: The object has already been deleted or has not been completely created",
		"ManagedObjectNotFound: snapshot-4021 could not be found",
	}
	for _, msg := range gone {
		if !isGone(errors.New(msg)) {
			t.Errorf("%q was treated as a failure to clean up", msg)
		}
	}
	if isGone(errors.New("Insufficient disk space on datastore ds1 to consolidate")) {
		t.Error("a real consolidation failure was swallowed as an already-deleted snapshot")
	}
}

/* Consolidation is bounded, and the timeout has to be long enough that a normal
   merge on busy storage is not cut short. Cutting one short is worse than
   waiting: it leaves the snapshot in place AND fails the pass. */
func TestTheConsolidationWaitIsNotImpatient(t *testing.T) {
	if consolidateWait.Minutes() < 5 {
		t.Errorf("waiting only %s for a consolidation would cut normal merges short", consolidateWait)
	}
}

/* A read is a range request, and HTTP byte ranges are inclusive at both ends.
   Off by one re-reads or skips a byte on every chunk of every disk, and the
   result still looks like a completed copy. */
func TestARangeCoversExactlyTheBytesAskedFor(t *testing.T) {
	tests := []struct {
		offset, length int64
		want           string
	}{
		{0, 4096, "bytes=0-4095"},
		{4096, 32 << 20, "bytes=4096-33558527"},
		{1 << 40, 512, "bytes=1099511627776-1099511628287"},
	}
	for _, tt := range tests {
		if got := rangeHeader(tt.offset, tt.length); got != tt.want {
			t.Errorf("rangeHeader(%d, %d) = %q, want %q", tt.offset, tt.length, got, tt.want)
		}
	}
}
