package hyperv

import (
	"context"
	"strings"
	"testing"
)

/* A destructive job must never claim an outcome it did not produce.

   From the rig, 2026-08-23: DestroyS2D ran on HVNEW01 and reported "destroyed
   the S2D pool and its volumes, and disabled Storage Spaces Direct on the
   cluster" — twice — while the cluster went on reporting the pool. A clustered
   pool is only enumerable from a node that currently sees it, so on the wrong
   member every step found nothing, every error was swallowed into a log nobody
   read, and the final check ("are there any pools left?") passed trivially
   because there had been none to begin with.

   A confident success message for a no-op, on the one action with no undo. */

func destroyStub(t *testing.T, result string) *PowerShell {
	t.Helper()
	var p PowerShell
	p.run = func(_ context.Context, _ string) ([]byte, error) { return []byte(result), nil }
	return &p
}

func TestDestroyS2DRefusesToClaimSuccessWhenThereWasNoPool(t *testing.T) {
	p := destroyStub(t, `RESULT={"poolsAtStart":0,"poolsLeft":0,"wiped":0,"log":[]}`)
	note, err := p.DestroyS2D(context.Background(), true)
	if err == nil {
		t.Fatalf("finding no pool must not report success, got %q", note)
	}
	if !strings.Contains(err.Error(), "nothing was destroyed") {
		t.Errorf("it must say plainly that nothing happened: %v", err)
	}
	// And name the way to actually do it, rather than leaving a flat refusal.
	if !strings.Contains(err.Error(), "run this on the pool's owner node") {
		t.Errorf("it must name where to run it instead: %v", err)
	}
}

func TestDestroyS2DFailsWhenThePoolSurvives(t *testing.T) {
	p := destroyStub(t, `RESULT={"poolsAtStart":1,"poolsLeft":1,"wiped":0,"log":["pool-err-access denied"]}`)
	_, err := p.DestroyS2D(context.Background(), false)
	if err == nil {
		t.Fatal("a surviving pool must fail the job")
	}
	if !strings.Contains(err.Error(), "STILL PRESENT") || !strings.Contains(err.Error(), "access denied") {
		t.Errorf("it must say so and carry the reason: %v", err)
	}
	// Half-torn-down is a state worth naming, because the next action depends on it.
	if !strings.Contains(err.Error(), "half torn down") {
		t.Errorf("it must warn that the cluster may be half torn down: %v", err)
	}
}

// Tolerated step errors are still errors. The pool going does not mean every CSV
// and cluster resource went with it.
func TestDestroyS2DSurfacesToleratedStepFailures(t *testing.T) {
	p := destroyStub(t, `RESULT={"poolsAtStart":1,"poolsLeft":0,"wiped":0,"log":["csv-Vol01","res-err-the resource is in use"]}`)
	_, err := p.DestroyS2D(context.Background(), false)
	if err == nil {
		t.Fatal("a step that failed must not be swallowed just because the pool went")
	}
	if !strings.Contains(err.Error(), "the resource is in use") {
		t.Errorf("the step's own reason must reach the operator: %v", err)
	}
}

func TestDestroyS2DReportsARealTeardown(t *testing.T) {
	p := destroyStub(t, `RESULT={"poolsAtStart":1,"poolsLeft":0,"wiped":4,"log":["csv-Vol01","vdisk-Vol01","pool-S2D on c1","disabled-s2d"]}`)
	note, err := p.DestroyS2D(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "4 local disks returned to raw") {
		t.Errorf("the wipe count must be reported: %q", note)
	}
	// What it actually did, so a success is auditable rather than asserted.
	if !strings.Contains(note, "Steps: ") || !strings.Contains(note, "disabled-s2d") {
		t.Errorf("the steps must be carried on a success too: %q", note)
	}
}

// A wipe that wiped nothing is not a wipe, and saying "0 disks returned to raw"
// as though it were an achievement is how the rig's job read.
func TestDestroyS2DSaysWhenTheWipeTouchedNothing(t *testing.T) {
	p := destroyStub(t, `RESULT={"poolsAtStart":1,"poolsLeft":0,"wiped":0,"log":["pool-S2D on c1"]}`)
	note, err := p.DestroyS2D(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "NO disks were returned to raw") {
		t.Errorf("a wipe that touched nothing must say so: %q", note)
	}
	if !strings.Contains(note, "run the wipe on the members that hold them") {
		t.Errorf("it must name what to do about it: %q", note)
	}
}
