package reconcile

import (
	"errors"
	"fmt"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

/* A VM that cannot start because its saved state was captured on another host
   is the worked example behind the "every recovery reachable from the centre"
   rule in CLAUDE.md. The console previously showed the operator:

     set vm power "Linux": powershell: exit status 1:
     Start-VM : 'Linux' failed to restore.

   which names the cmdlet that failed and nothing else. These tests pin the two
   properties that let the console do better: the failure is identifiable, and
   it is never mistaken for something that will fix itself. */

// The reason is a contract with the UI — it decides which remedy is offered —
// so it must survive being wrapped on the way up through the agent.
func TestAnIncompatibleSavedStateGetsAnActionableReason(t *testing.T) {
	r := testReconciler(&hyperv.Stub{})
	err := fmt.Errorf("set vm power %q: %w", "Linux", hyperv.ErrSavedStateIncompatible)

	c := r.condition("VMPower/Linux", hyperv.OutcomeUnchanged, err)

	if c.Reason != types.ReasonSavedStateIncompatible {
		t.Fatalf("want reason %q so the console can offer the remedy, got %q", types.ReasonSavedStateIncompatible, c.Reason)
	}
	if c.Status {
		t.Fatal("a VM that will not start is not a met condition")
	}
	if c.Message == "" {
		t.Fatal("the condition must carry the explanation, not just a code")
	}
}

// The important negative. transientSignature/Settling exists so brief churn is
// not reported as failure, but an incompatible saved state NEVER resolves on its
// own — it needs a destructive operator decision. Reporting it as "Settling"
// would promise a recovery that cannot happen and hide the one thing the
// operator must act on, indefinitely.
func TestAnIncompatibleSavedStateIsNeverReportedAsSettling(t *testing.T) {
	r := testReconciler(&hyperv.Stub{})
	err := fmt.Errorf("set vm power %q: %w", "Linux", hyperv.ErrSavedStateIncompatible)

	// Repeatedly, because Settling is time-based: if the classification were
	// wrong it would read calm for the whole transient window.
	for i := 0; i < 5; i++ {
		c := r.condition("VMPower/Linux", hyperv.OutcomeUnchanged, err)
		if c.Reason == "Settling" || c.Status {
			t.Fatalf("pass %d: reported as settling; this never settles and needs an operator", i)
		}
		if c.Reason != types.ReasonSavedStateIncompatible {
			t.Fatalf("pass %d: reason drifted to %q", i, c.Reason)
		}
	}
}

// Ordinary failures must be unaffected — the new branch sits ahead of the
// transient check, so it is well placed to swallow everything if written wrong.
func TestOrdinaryFailuresStillClassifyAsBefore(t *testing.T) {
	r := testReconciler(&hyperv.Stub{})

	plain := r.condition("VMPower/Web01", hyperv.OutcomeUnchanged, errors.New("something else went wrong"))
	if plain.Reason != "ApplyFailed" {
		t.Fatalf("an unrecognised failure should still be ApplyFailed, got %q", plain.Reason)
	}

	// A known-transient signature still gets its settling treatment.
	settling := r.condition("VM/Web01", hyperv.OutcomeUnchanged,
		errors.New("attach failed: cannot find the file specified: disk.avhdx"))
	if settling.Reason != "Settling" {
		t.Fatalf("a merging .avhdx should still settle, got %q", settling.Reason)
	}

	ok := r.condition("VMPower/Web01", hyperv.OutcomeUnchanged, nil)
	if !ok.Status {
		t.Fatal("a successful operation must still report a met condition")
	}
}

// The sentinel has to survive wrapping, since it is produced deep in the
// PowerShell adapter and read at the top of the reconciler.
func TestTheSentinelSurvivesWrapping(t *testing.T) {
	deep := fmt.Errorf("set vm power %q: %w", "Linux", hyperv.ErrSavedStateIncompatible)
	wrapped := fmt.Errorf("reconcile vm %q: %w", "Linux", deep)

	if !errors.Is(wrapped, hyperv.ErrSavedStateIncompatible) {
		t.Fatal("the sentinel must survive wrapping or the console silently loses the remedy")
	}
}
