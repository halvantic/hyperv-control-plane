package main

import (
	"testing"
	"time"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

// The delete race, in one test.
//
// A reconcile pass loads the cached desired set once and can reach a given VM a
// minute or more later. If the RemoveVM job completes in between, releasing the
// hold the instant the job returns puts the pass back where it started: holding
// a name whose VM no longer exists, which EnsureVM then creates again — and, for
// a clustered VM, re-registers as a cluster role. Observed on 'Tes', 2026-08-20.
func TestRemovedVMStaysHeldAfterTheJobFinishes(t *testing.T) {
	r := &runner{}
	r.noteVMRemoved("Tes")

	// The job is over — nothing is in jobsVMs — and the VM must still be held.
	kind, busy := r.vmBusy("Tes")
	if !busy || kind != types.JobRemoveVM {
		t.Fatalf("a just-deleted VM must be held: kind=%q busy=%v", kind, busy)
	}
	// Case-insensitive, like every other name in the product.
	if _, busy := r.vmBusy("TES"); !busy {
		t.Fatal("the hold must be case-insensitive")
	}
	if _, busy := r.vmBusy("Windows"); busy {
		t.Fatal("an unrelated VM must not be held")
	}
}

// The hold is a window, not a tombstone. It exists to outlast a pass already in
// flight; keeping it for ever would mean an agent that could never be told to
// create a VM of that name again without a restart.
func TestRemovedVMHoldExpires(t *testing.T) {
	r := &runner{vmRemoved: map[string]time.Time{
		"tes": time.Now().Add(-vmRemovedHold - time.Second),
	}}
	if _, busy := r.vmBusy("Tes"); busy {
		t.Fatal("the hold must lapse once a pass could no longer be holding the old set")
	}
	if _, ok := r.vmRemoved["tes"]; ok {
		t.Error("a lapsed hold should be dropped, not left to accumulate")
	}
}

// Intent outranks the stand-off. A VM back in the delivered set is the centre
// asking for it — an operator who re-created it under the same name — and the
// agent honours what it was given rather than a hold it set itself.
func TestPulledVMClearsTheRemovalHold(t *testing.T) {
	r := &runner{}
	r.noteVMRemoved("Tes")
	r.noteVMRemoved("Linux")

	r.clearVMRemoved([]types.VM{{Meta: types.ObjectMeta{Name: "TES"}}})

	if _, busy := r.vmBusy("Tes"); busy {
		t.Fatal("a VM the centre has delivered again must be reconciled, not held")
	}
	if _, busy := r.vmBusy("Linux"); !busy {
		t.Fatal("a VM the centre did not deliver must stay held")
	}
}
