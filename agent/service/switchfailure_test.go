package main

import (
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

/* A switch that cannot be built is nearly always a switch whose team members
   have been renamed — a replaced NIC, a driver change, a different virtual
   adapter type. The console explains that by comparing the spec's adapter names
   against the reported INVENTORY, which refreshes every eighth cycle: minutes on
   a slow host. So the failure arrived alongside an adapter list old enough to
   still contain the missing names, the console had nothing to say, and the
   operator was left with the raw Set-VMSwitchTeam error until a refresh happened
   to come round. Seen on HVNEW03 2026-08-13 after a NIC swap.

   The failing step is what says the inventory is now worth re-reading. */

func TestAFailedSwitchStepIsWhatTriggersTheAdapterReRead(t *testing.T) {
	if !hasFailedSwitchStep([]types.Condition{
		{Type: "Switch/ConvergedSwitch", Reason: "ApplyFailed", Status: false},
	}) {
		t.Error("a switch that failed to apply must trigger the re-read that explains it")
	}
}

func TestAHealthyOrMerelyProgressingSwitchDoesNot(t *testing.T) {
	for _, c := range []types.Condition{
		{Type: "Switch/ConvergedSwitch", Status: true},
		{Type: "Switch/ConvergedSwitch", Reason: "NotApplied", Status: false},
	} {
		if hasFailedSwitchStep([]types.Condition{c}) {
			t.Errorf("re-reading the hardware costs a PowerShell call and must not fire for %+v", c)
		}
	}
}

// Another step failing says nothing about the adapters, and a re-read on every
// unrelated failure is a cost with no answer at the end of it.
func TestAnUnrelatedFailureDoesNotReReadTheHardware(t *testing.T) {
	if hasFailedSwitchStep([]types.Condition{
		{Type: "CSV/Volume01", Reason: "ApplyFailed", Status: false},
		{Type: "HostISCSI", Reason: "ApplyFailed", Status: false},
	}) {
		t.Error("only a switch failure is explained by the adapter list")
	}
}
