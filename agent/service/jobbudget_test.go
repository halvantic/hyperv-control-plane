package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* Two budgets that disagreed, in two files that each looked reasonable alone.

   The capture script waits sixty minutes for a generalised guest to shut itself
   down. This layer capped every job kind it did not recognise at ten. A
   generalising capture therefore ran sysprep, the guest shut down exactly as
   promised, and the job was cancelled and reported Failed part-way through —
   no template, and nothing saying why. From the console it looked as though the
   server sysprepped and stopped and then the capture simply never happened.

   These tests exist to keep the two numbers tied together. A number a second
   file has to guess is a number that will eventually be wrong. */

func capture(generalise bool) types.Job {
	return types.Job{
		Kind:   types.JobVMCaptureTemplate,
		Params: map[string]string{"generalise": strconv.FormatBool(generalise)},
	}
}

func TestAGeneralisingCaptureOutlastsItsOwnSysprepWait(t *testing.T) {
	got := jobTimeoutFor(capture(true))
	if got <= hyperv.SysprepDeadline {
		t.Fatalf("budget %v does not exceed the script's own %v sysprep wait, so the job is cancelled while the script is still legitimately waiting",
			got, hyperv.SysprepDeadline)
	}
	// And it must leave room for the copy that FOLLOWS the shutdown — a template
	// disk is tens of gigabytes, usually onto an SMB share.
	if got < hyperv.SysprepDeadline+hyperv.CaptureCopyAllowance {
		t.Fatalf("budget %v leaves no room to copy the disk after sysprep finishes", got)
	}
}

/* The specific regression: the default. Ten minutes for an operation whose
   first step alone may take sixty. */
func TestACaptureNeverFallsBackToTheDefaultBudget(t *testing.T) {
	for _, g := range []bool{true, false} {
		if got := jobTimeoutFor(capture(g)); got == jobTimeout {
			t.Errorf("generalise=%v fell through to the %v default", g, jobTimeout)
		}
	}
}

/* One kind is not one duration, which is why this takes the job rather than the
   kind. A plain capture is a disk copy; a generalising one is a guest shutdown
   on somebody else's schedule and then a disk copy. */
func TestAPlainCaptureIsNotGivenTheSysprepAllowance(t *testing.T) {
	plain, gen := jobTimeoutFor(capture(false)), jobTimeoutFor(capture(true))
	if plain >= gen {
		t.Fatalf("a plain capture (%v) is budgeted as generously as a generalising one (%v)", plain, gen)
	}
	if plain < hyperv.CaptureCopyAllowance {
		t.Fatalf("a plain capture (%v) is not given time to copy the disk", plain)
	}
}

/* An absent or malformed parameter must not silently buy the short budget: the
   expensive case is the one that fails badly, so anything other than an
   explicit "false" is treated as the long one by construction. This asserts the
   safe direction rather than the parsing. */
func TestAnUnreadableGeneraliseFlagDoesNotShortenTheBudget(t *testing.T) {
	no := types.Job{Kind: types.JobVMCaptureTemplate}
	if got := jobTimeoutFor(no); got == jobTimeout {
		t.Fatalf("a capture with no generalise parameter got the %v default", jobTimeout)
	}
}

func TestOtherLongJobsKeepTheirBudget(t *testing.T) {
	// MigrateVM is deliberately NOT here any more: it copies a VM's whole
	// storage across a link, so it belongs with CopyVM on the data-copy budget.
	// See TestAWholeVMCopyIsNotCutShortByADefaultBudget.
	for _, k := range []string{types.JobRebuildPool, types.JobClusterMoveVM, types.JobVMExport} {
		if got := jobTimeoutFor(types.Job{Kind: k}); got != 30*time.Minute {
			t.Errorf("%s = %v, want 30m", k, got)
		}
	}
	if got := jobTimeoutFor(types.Job{Kind: types.JobRemoveVM}); got != jobTimeout {
		t.Errorf("an ordinary job = %v, want the %v default", got, jobTimeout)
	}
}

/* The second, worse half of the same bug.

   A job that hits its budget has, by definition, a cancelled context — and the
   failure was reported THROUGH that context. The call could not succeed, so the
   centre never learned the job had ended: it stayed Running for ever, and a
   capture behind it stayed Capturing, which the template delete guard then
   refuses to clear. The record could not even be removed from the console.

   The one case where reporting matters most was the one case it could not
   happen. */
func TestATimedOutJobStillHasALiveContextToReportOn(t *testing.T) {
	// The job's own context, already expired, as it is at the moment of failure.
	jctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if jctx.Err() == nil {
		t.Fatal("test setup: the job context should already be done")
	}

	// The report context is built from the PARENT with WithoutCancel, so a cycle
	// ending underneath the job cannot take the report with it either.
	parent, pcancel := context.WithCancel(context.Background())
	pcancel()
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(parent), jobReportTimeout)
	defer rcancel()
	if rctx.Err() != nil {
		t.Fatalf("the report context is already dead (%v), so a timed-out job could not report its failure", rctx.Err())
	}
}

/* "context deadline exceeded" names neither the budget nor the operation and
   reads as an internal error. For a capture it is actively misleading: sysprep
   ran, the guest shut down as asked, and the operator is shown a phrase
   suggesting nothing happened at all. */
func TestATimeoutSaysItRanTooLongRatherThanShowingAContextError(t *testing.T) {
	jctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	got := describeJobFailure(capture(true), jctx, context.DeadlineExceeded)
	if !strings.Contains(got, "ran longer than") {
		t.Errorf("the message does not say the job was cut short:\n %s", got)
	}
	// The budget is quoted, so an operator can tell a job that needs longer from
	// one that is genuinely stuck.
	if !strings.Contains(got, jobTimeoutFor(capture(true)).String()) {
		t.Errorf("the message does not name the budget:\n %s", got)
	}
	// And it says the host may already have changed — a capture that timed out
	// after sysprep has left the guest generalised and shut down.
	if !strings.Contains(got, "check the host") {
		t.Errorf("the message does not warn that work may already have happened:\n %s", got)
	}
}

/* A job that failed on its own merits must report ITS error, not be relabelled
   as a timeout. */
func TestAnOrdinaryFailureIsReportedAsItself(t *testing.T) {
	real := errors.New("the guest credential was rejected")
	got := describeJobFailure(capture(true), context.Background(), real)
	if got != real.Error() {
		t.Fatalf("an ordinary failure was rewritten as %q", got)
	}
}

/* A whole-VM copy is bounded by disk size and link speed, not by this host.

   CopyVM was not in jobTimeoutFor at all, so it took the ten-minute default and
   was killed part-way through copying HVNew01 to HVNEW06 on 2026-09-01 — the
   same shape as the capture bug the function's own comment describes. MigrateVM
   was listed but at thirty minutes, which is the same mistake one step less
   obvious: half a terabyte over a gigabit link is an hour and a quarter at the
   theoretical rate, and an evacuation is exactly when the link is busiest. */
func TestAWholeVMCopyIsNotCutShortByADefaultBudget(t *testing.T) {
	for _, kind := range []string{types.JobCopyVM, types.JobMigrateVM} {
		got := jobTimeoutFor(types.Job{Kind: kind})
		if got == jobTimeout {
			t.Errorf("%s takes the default %v budget, so a copy of any real size is killed part-way through", kind, got)
		}
		// An hour is not the bar; a slow terabyte is the case that matters, and
		// cutting one short wastes every byte already copied.
		if got < 4*time.Hour {
			t.Errorf("%s gets %v, which a large VM on a busy link will exceed", kind, got)
		}
	}
}

// The budget lives beside the operation so the two cannot drift apart, which is
// exactly how the capture budget once went wrong.
func TestTheCopyBudgetComesFromTheHypervPackage(t *testing.T) {
	if jobTimeoutFor(types.Job{Kind: types.JobCopyVM}) != hyperv.CopyBudget {
		t.Error("the copy budget is duplicated rather than taken from hyperv.CopyBudget")
	}
}
