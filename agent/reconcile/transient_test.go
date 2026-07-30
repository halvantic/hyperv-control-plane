package reconcile

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/joshua-fourie/ballast/agent/hyperv"
)

// Only explicitly recognised in-flight signatures are transient. Anything else
// is a failure — softening an unknown error would manufacture the false-green
// defect this package exists to prevent.
func TestTransientSignatureRecognisesOnlyKnownCauses(t *testing.T) {
	for _, tc := range []struct {
		err  string
		want bool
	}{
		// Observed live: removing replication merges the reference-point .avhdx
		// into the base VHDX; until it finishes the attach names a vanishing file.
		{`Attachment 'C:\ClusterStorage\Datastore 1\Linux\Linux_5187.avhdx' not found. Error: 'The system cannot find the file specified.'`, true},
		// Observed live: the VHDX is held open by a running VM mid-migration.
		{"the process cannot access the file because it is being used by another process", true},
		// Not transient — these are real and must stay red.
		{"Access to the path 'Datastore 1' is denied", false},
		{"Hyper-V Replica Broker is Failed - not online in this group", false},
		{"New-VHD : Failed to create the virtual hard disk", false},
		{"no such host is known", false},
	} {
		got := transientSignature(errors.New(tc.err)) != ""
		if got != tc.want {
			t.Errorf("transient=%v, want %v for %q", got, tc.want, tc.err)
		}
	}
	if transientSignature(nil) != "" {
		t.Error("nil error is not transient")
	}
}

// A transient reports as settling and does NOT degrade the host — but only
// inside the window. Past it the original error must surface, or "settling"
// becomes a permanent lie.
func TestConditionEscalatesAPersistentTransient(t *testing.T) {
	r := New(&hyperv.Stub{}, nil)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	err := errors.New(`Attachment 'C:\x\Linux_51.avhdx' not found. Error: 'The system cannot find the file specified.'`)

	// First pass: settling, and not a failure.
	c := r.condition("VM/Linux", hyperv.OutcomeUnchanged, err)
	if !c.Status || c.Reason != "Settling" {
		t.Fatalf("first pass should settle, got status=%v reason=%q", c.Status, c.Reason)
	}
	if !strings.Contains(c.Message, "merging") {
		t.Errorf("message should explain what is in flight, got %q", c.Message)
	}

	// Still inside the window.
	now = now.Add(transientWindow - time.Second)
	if c := r.condition("VM/Linux", hyperv.OutcomeUnchanged, err); !c.Status {
		t.Fatalf("still inside the window; want settling, got %q: %s", c.Reason, c.Message)
	}

	// Past the window: the real error, and a failure.
	now = now.Add(2 * time.Second)
	c = r.condition("VM/Linux", hyperv.OutcomeUnchanged, err)
	if c.Status || c.Reason != "ApplyFailed" {
		t.Fatalf("past the window it must fail, got status=%v reason=%q", c.Status, c.Reason)
	}
	if !strings.Contains(c.Message, "no longer treated as transient") || !strings.Contains(c.Message, "avhdx") {
		t.Errorf("escalated message should carry the original error and say why, got %q", c.Message)
	}
}

// A success clears the history, so a later unrelated transient starts its own
// window rather than inheriting an old start time and escalating immediately.
func TestConditionResetsTransientHistoryOnSuccess(t *testing.T) {
	r := New(&hyperv.Stub{}, nil)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	err := errors.New(`Attachment 'C:\x\a.avhdx' not found. Error: 'The system cannot find the file specified.'`)

	r.condition("VM/Linux", hyperv.OutcomeUnchanged, err)
	now = now.Add(10 * time.Minute)
	if c := r.condition("VM/Linux", hyperv.OutcomeUnchanged, nil); !c.Status {
		t.Fatal("a successful pass must report success")
	}
	// A fresh transient long after should settle again, not inherit the old clock.
	if c := r.condition("VM/Linux", hyperv.OutcomeUnchanged, err); !c.Status || c.Reason != "Settling" {
		t.Fatalf("history should have been cleared; got status=%v reason=%q", c.Status, c.Reason)
	}
}

// A non-transient failure must never be softened, and must clear any transient
// history so it cannot mask a later genuine settle.
func TestConditionLeavesRealFailuresAlone(t *testing.T) {
	r := New(&hyperv.Stub{}, nil)
	c := r.condition("VM/Linux", hyperv.OutcomeUnchanged, errors.New("Access to the path 'Datastore 1' is denied"))
	if c.Status || c.Reason != "ApplyFailed" {
		t.Fatalf("a real failure must stay a failure, got status=%v reason=%q", c.Status, c.Reason)
	}
	if strings.Contains(c.Message, "retrying") {
		t.Errorf("a real failure must not be dressed as settling, got %q", c.Message)
	}
}
