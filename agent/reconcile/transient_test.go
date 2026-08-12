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
		// Observed live 2026-08-06: enabling replication to a cluster's Replica
		// Broker failed with this while the target was still settling, and the same
		// VM was Replicating with health Normal after a later pass, with nothing
		// changed in between. The sentence names no cause, so treating it as a fault
		// on the first pass produces a confident wrong diagnosis — the failure mode
		// this whole file exists to avoid in the other direction.
		{`ensure vm replication "Linux": Enable-VMReplication : Hyper-V failed to enable replication.`, true},
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

/* A CSV that is not Online cannot be written to, and Windows reports that as
   "Access to the path ... is denied" — a permissions error for something that
   is not about permissions. On the rig this repeated every reconcile pass for
   hours while the storage pool rebuilt, telling the operator to inspect ACLs
   that were fine. */

func TestAVolumeComingBackOnlineSettlesBriefly(t *testing.T) {
	err := errors.New("ensure replica server: powershell: exit status 1: waiting for volume 'Cluster Virtual Disk (Datastore 1)' to come back online before configuring the replica server (the cluster reports it OnlinePending)")
	what := transientSignature(err)
	if what == "" {
		t.Fatal("a volume that is not online yet is normal recovery for a pass or two, not an immediate fault")
	}
	if !strings.Contains(what, "not online") {
		t.Fatalf("the explanation must name the actual cause, got %q", what)
	}
}

// The half that keeps it honest. transientWindow exists so nothing waits for
// ever in silence: a CSV still offline after two minutes is a real problem and
// must escalate rather than read calm indefinitely.
func TestAVolumeThatNeverComesBackEscalates(t *testing.T) {
	tr := newTransientTracker()
	base := time.Now()
	err := errors.New("waiting for volume 'Vol2' to come back online")

	if _, within := tr.observe("ReplicaServer", base); !within {
		t.Fatal("the first observation is inside the window")
	}
	if _, within := tr.observe("ReplicaServer", base.Add(transientWindow+time.Second)); within {
		t.Fatalf("past %s this must stop being treated as settling — a volume stuck offline is a fault", transientWindow)
	}
	// The signature itself keeps matching; it is the WINDOW that escalates, so
	// the message stays accurate while the severity changes.
	if transientSignature(err) == "" {
		t.Fatal("the signature should still recognise the cause after escalation")
	}
}

// A genuine permissions failure on an Online volume must NOT be softened — that
// is the fault this whole change could accidentally hide.
func TestARealPermissionFailureIsNotSoftened(t *testing.T) {
	err := errors.New("ensure replica server: powershell: exit status 1: New-Item : Access to the path 'Datastore 1' is denied.")
	if what := transientSignature(err); what != "" {
		t.Fatalf("an access-denied on an online volume is a real fault, got softened as %q", what)
	}
}

// Rebooting one node of a two-node cluster takes half of it away, and every
// cluster-level operation then fails until it returns. On bcluster2 the replica
// broker and a CSV both failed while HVNEW02 restarted, and both came back on
// their own when it did — so neither was a fault, and neither should have read
// as one for the minute it took.
func TestAClusterMemberRestartingIsTransient(t *testing.T) {
	for _, raw := range []string{
		"ensure replica broker \"bcluster2-Brk\": powershell: exit status 1: Add-ClusterServerRole : The cluster service is not running. Make sure that the service is running on all nodes in the cluster. There are no more endpoints available from the endpoint mapper",
		"ensure CSV \"ReplDS\": powershell: exit status 1: no S2D pool exists yet (0 poolable disk(s) visible). S2D was likely enabled while no disks were eligible; the pool is bootstrapped automatically once poolable disks appear - retries next pass.",
	} {
		got := transientSignature(errors.New(raw))
		if got == "" {
			t.Errorf("a cluster operation failing because a member is away is settling, not failed: %q", raw)
			continue
		}
		if strings.Contains(got, "powershell") || strings.Contains(got, "Add-Cluster") {
			t.Errorf("the explanation must replace the cmdlet error, not repeat it: %q", got)
		}
	}
}

// The gate is a member being away, not "anything mentioning a cluster". An
// unrecognised cluster error is still a failure.
func TestAnUnknownClusterErrorIsStillAFailure(t *testing.T) {
	if got := transientSignature(errors.New("ensure CSV: the volume name is already in use")); got != "" {
		t.Errorf("an unrecognised error must never be softened, got %q", got)
	}
}
