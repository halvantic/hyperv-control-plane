package hyperv

import (
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

/* Moving onto a host that names its switches differently.

     The virtual machine 'HVNew01' is not compatible with physical computer
     'HVNEW06'. Could not find Ethernet switch 'ConvergedSwitch2'.

   Two hosts built separately do not agree on switch names, which is most of the
   reason an evacuation exists — it is how workloads reach a NEW environment. So
   this is not an edge case to guard against; it is the ordinary path. */

func TestAMappedSwitchIsReconnectedAtTheDestination(t *testing.T) {
	s := moveWithNetworkMap([]types.EvacuationNIC{
		{SourceSwitch: "ConvergedSwitch2", TargetSwitch: "Converged"},
	})

	if !strings.Contains(s, `$netMap = @{'ConvergedSwitch2' = 'Converged'}`) {
		t.Fatalf("the mapping is not rendered: %s", s)
	}
	if !strings.Contains(s, "Connect-VMNetworkAdapter -VMNetworkAdapter $ad -SwitchName $to") {
		t.Errorf("a mapped adapter is not reconnected: %s", s)
	}
	// Hyper-V's own mechanism: compare, fix the report, move against it.
	if !strings.Contains(s, "Compare-VM -Name $vm") || !strings.Contains(s, "Move-VM -CompatibilityReport $rep") {
		t.Errorf("the move does not go through a compatibility report: %s", s)
	}
}

/*
An UNMAPPED network arrives disconnected, deliberately.

	Attaching it to whichever switch happens to be there would put a VM on a
	network nobody chose — silently, and possibly one it can reach production
	from. A disconnected adapter is noticed in seconds; a wrong one is noticed
	when it matters.
*/
func TestAnUnmappedNetworkArrivesDisconnectedRatherThanGuessed(t *testing.T) {
	s := moveWithNetworkMap(nil)

	if !strings.Contains(s, "Disconnect-VMNetworkAdapter -VMNetworkAdapter $ad") {
		t.Fatalf("an unmapped adapter is not disconnected: %s", s)
	}
	if !strings.Contains(s, "disconnected (no mapping given)") {
		t.Errorf("a disconnected adapter is not reported: %s", s)
	}
	// And nothing invents a destination.
	if strings.Contains(s, "Get-VMSwitch") {
		t.Errorf("the script looks for a switch to attach to on its own: %s", s)
	}
}

// A VLAN retags the adapter on arrival; zero leaves the tag alone rather than
// clearing one somebody set.
func TestAVLANIsAppliedOnlyWhenGiven(t *testing.T) {
	with := moveWithNetworkMap([]types.EvacuationNIC{{SourceSwitch: "A", TargetSwitch: "B", VLANID: 40}})
	if !strings.Contains(with, `$vlanMap = @{'A' = 40}`) {
		t.Fatalf("the VLAN is not carried: %s", with)
	}
	if !strings.Contains(with, "if ($v -and [int]$v -gt 0)") {
		t.Errorf("a zero VLAN would be applied as a tag: %s", with)
	}
	without := moveWithNetworkMap([]types.EvacuationNIC{{SourceSwitch: "A", TargetSwitch: "B"}})
	if !strings.Contains(without, `$vlanMap = @{'A' = 0}`) {
		t.Errorf("an unset VLAN is not rendered as zero: %s", without)
	}
}

/* The report is carried forward, not judged.

   Incompatibilities do not clear as they are fixed — the list is a snapshot —
   and it always carries generic wrappers whose Source is the VM itself. A check
   that treated anything not an adapter as unhandled threw on those wrappers
   after the only real problem, a switch name, had just been remapped:

     the destination cannot take this VM: Virtual machine migration operation
     for 'HVNew01' failed at migration destination | The virtual machine
     'HVNew01' is not compatible with physical computer 'HVNEW06'

   Two sentences with no cause in either. Move-VM validates again for itself and
   refuses with the specific reason, which is the message worth having. */
func TestTheCompatibilityReportIsReportedRatherThanJudged(t *testing.T) {
	s := moveWithNetworkMap([]types.EvacuationNIC{{SourceSwitch: "A", TargetSwitch: "B"}})

	if strings.Contains(s, "the destination cannot take this VM") {
		t.Fatalf("the script still refuses on its own reading of the report: %s", s)
	}
	// The report still reaches the operator, so a Move-VM failure can be read
	// against what the comparison saw beforehand.
	if !strings.Contains(s, "PROGRESS the destination objected to: ") {
		t.Errorf("the report is discarded rather than carried forward: %s", s)
	}
	/* And it rides into the FAILURE, not only the progress notes. A progress
	   line is gone by the time somebody reads a failed job, and the whole
	   question when this fails is what was offered and what was done with it. */
	if !strings.Contains(s, "Ballast saw ") || !strings.Contains(s, "and applied: ") {
		t.Errorf("a failed move does not say what Ballast saw or did: %s", s)
	}
	if !strings.Contains(s, "no incompatibility carried an adapter") {
		t.Errorf("a move that matched nothing does not say so: %s", s)
	}
	if !strings.Contains(s, "Move-VM -CompatibilityReport $rep") {
		t.Errorf("the move does not go through the report: %s", s)
	}
}

/*
Adapters are found by their SHAPE, not by MessageId 33012.

	The id is right today and is a number in somebody else's product. An object
	carrying a SwitchName is a network adapter whatever the id happens to be.
*/
func TestAdaptersAreIdentifiedByShapeNotByMessageId(t *testing.T) {
	s := moveWithNetworkMap(nil)
	if !strings.Contains(s, "PSObject.Properties.Name -contains 'SwitchName'") {
		t.Fatalf("adapters are not identified by shape: %s", s)
	}
	if strings.Contains(s, "33012") && !strings.Contains(s, "# ") {
		t.Errorf("the script branches on a magic message id: %s", s)
	}
}

/*
Switch names come from an operator and go into a PowerShell literal. A name

	with an apostrophe in it must not end the string and start running.
*/
func TestASwitchNameWithAQuoteCannotBreakOutOfTheScript(t *testing.T) {
	s := psSwitchMap([]types.EvacuationNIC{{SourceSwitch: "it's odd", TargetSwitch: "also' odd"}})
	if !strings.Contains(s, `'it''s odd' = 'also'' odd'`) {
		t.Fatalf("quotes are not doubled: %s", s)
	}
}

// The rendered map is stable, so a change to the script is a change to the
// mapping rather than to Go's map ordering.
func TestTheRenderedMapIsStable(t *testing.T) {
	nics := []types.EvacuationNIC{
		{SourceSwitch: "zeta", TargetSwitch: "z"},
		{SourceSwitch: "alpha", TargetSwitch: "a"},
	}
	first := psSwitchMap(nics)
	for i := 0; i < 20; i++ {
		if psSwitchMap(nics) != first {
			t.Fatal("the rendered map changes between runs")
		}
	}
	if strings.Index(first, "alpha") > strings.Index(first, "zeta") {
		t.Errorf("the map is not sorted: %s", first)
	}
}
