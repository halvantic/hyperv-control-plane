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
	got := psFailureDetail(context.Background(), time.Now(), "", "  Remove-VM : access denied  ")
	if got != "Remove-VM : access denied" {
		t.Fatalf("the command's own diagnostic must win and be trimmed, got %q", got)
	}
}

func TestACancelledOperationSaysSoRatherThanNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := psFailureDetail(ctx, time.Now(), "", "")
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

	timedOut := psFailureDetail(ctx, time.Now(), "", "")
	if !strings.Contains(timedOut, "time limit") {
		t.Fatalf("a deadline must be named as a deadline: %q", timedOut)
	}

	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if psFailureDetail(cctx, time.Now(), "", "") == timedOut {
		t.Fatal("a timeout and a cancellation have different remedies and must not read identically")
	}
}

func TestPartialOutputIsReportedWhenThereIsNoError(t *testing.T) {
	got := psFailureDetail(context.Background(), time.Now(), "step one ok\nstep two ok\n", "")
	if !strings.Contains(got, "step two ok") {
		t.Fatalf("how far it got is more use than nothing: %q", got)
	}
}

// The last resort still has to say something, and what it must NOT say is "go
// and read the host's event log". The agent is on that host and reads those same
// logs elsewhere, so a fact it can establish must not be posted as homework —
// CLAUDE.md counts that as the defect, not the workaround. The last resort is
// reached only once the logs have been asked and had nothing.
func TestTheLastResortIsNeverEmptyAndDoesNotDelegateToTheOperator(t *testing.T) {
	got := psFailureDetail(context.Background(), time.Now(), "", "")
	if strings.TrimSpace(got) == "" {
		t.Fatal("no path through this function may return nothing")
	}
	if strings.Contains(got, "check the") {
		t.Fatalf("the agent reads these logs itself; it must not ask the operator to: %q", got)
	}
	// It must say the logs were consulted, or the reader cannot tell "nothing was
	// recorded" from "nobody looked".
	if !strings.Contains(got, "recorded nothing") {
		t.Fatalf("say that the host was asked and had nothing, so silence is a finding rather than an omission: %q", got)
	}
}

// The enrichment is best-effort by design: it exists to improve somebody else's
// error and must never replace one unhelpful message with a different one. On a
// machine with no Hyper-V logs it simply returns nothing.
func TestTheEventLogProbeNeverReportsItsOwnFailure(t *testing.T) {
	// Whatever this host is, the probe must return a string and not panic.
	_ = recentHostErrors(time.Now())
}
