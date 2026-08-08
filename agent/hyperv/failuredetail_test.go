package hyperv

import (
	"context"
	"strings"
	"testing"
	"time"
)

/* An empty error message is the worst possible outcome of a failure: it names
   neither a cause nor a remedy, and it does not even say whether anything went
   wrong. On the rig 2026-08-08 three RemoveVM jobs failed in a row with the
   entire message being:

       remove vm "Windows Standalone": powershell: exit status 1:

   Windows reports a killed process as exit status 1, and a killed process has
   written nothing — so a cancellation and a genuine failure were indistinguishable. */

func TestStderrIsUsedWhenThereIsAny(t *testing.T) {
	got := psFailureDetail(context.Background(), "", "  Remove-VM : access denied  ")
	if got != "Remove-VM : access denied" {
		t.Fatalf("the command's own diagnostic must win and be trimmed, got %q", got)
	}
}

func TestACancelledOperationSaysSoRatherThanNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := psFailureDetail(ctx, "", "")
	if got == "" {
		t.Fatal("an empty explanation is the defect being fixed")
	}
	if !strings.Contains(got, "cancelled") {
		t.Fatalf("a cancelled operation must be named as cancelled, not left blank: %q", got)
	}
}

// A deadline and a cancellation have different remedies — wait/raise the limit
// versus find out what stopped the agent — so they must not read the same.
func TestATimedOutOperationIsDistinguishedFromACancelledOne(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)

	timedOut := psFailureDetail(ctx, "", "")
	if !strings.Contains(timedOut, "time limit") {
		t.Fatalf("a deadline must be named as a deadline: %q", timedOut)
	}

	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if psFailureDetail(cctx, "", "") == timedOut {
		t.Fatal("a timeout and a cancellation have different remedies and must not read identically")
	}
}

func TestPartialOutputIsReportedWhenThereIsNoError(t *testing.T) {
	got := psFailureDetail(context.Background(), "step one ok\nstep two ok\n", "")
	if !strings.Contains(got, "step two ok") {
		t.Fatalf("how far it got is more use than nothing: %q", got)
	}
}

// The last resort still has to say something actionable. This is the case that
// produced the empty message, and it must never again be empty.
func TestTheLastResortIsNeverEmptyAndPointsSomewhere(t *testing.T) {
	got := psFailureDetail(context.Background(), "", "")
	if strings.TrimSpace(got) == "" {
		t.Fatal("no path through this function may return nothing")
	}
	if !strings.Contains(got, "event log") {
		t.Fatalf("with no output and no cancellation there is nowhere to look but the host's event logs; say so: %q", got)
	}
}
