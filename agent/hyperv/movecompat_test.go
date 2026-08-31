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

func TestTheRemapHappensOnTheRealVMNotTheReport(t *testing.T) {
	s := moveWithNetworkMap([]types.EvacuationNIC{
		{SourceSwitch: "ConvergedSwitch2", TargetSwitch: "Converged"},
	})

	if !strings.Contains(s, `$netMap = @{'ConvergedSwitch2' = 'Converged'}`) {
		t.Fatalf("the mapping is not rendered: %s", s)
	}
	/* Fixing adapters inside a compatibility report does not take. Measured
	   three times: connect by name, connect by switch object, and disconnect —
	   the documented example — each returning no error and each leaving Move-VM
	   refusing with "Could not find Ethernet switch ConvergedSwitch2". So the
	   work happens on the real VM with ordinary cmdlets. */
	if !strings.Contains(s, "Get-VMNetworkAdapter -VMName $vm") {
		t.Fatalf("the adapters are not read off the real VM: %s", s)
	}
	if strings.Contains(s, "Move-VM -CompatibilityReport") {
		t.Errorf("the move still goes through the report that does not take: %s", s)
	}
	if !strings.Contains(s, "Move-VM -Name $vm -DestinationHost $dest -IncludeStorage -DestinationStoragePath $path") {
		t.Errorf("the move is not a plain shared-nothing Move-VM: %s", s)
	}
	// The comparison is kept for what it IS good at: saying what else the
	// destination objects to, early and in its own words.
	if !strings.Contains(s, "Compare-VM -Name $vm") || !strings.Contains(s, "PROGRESS the destination objected to: ") {
		t.Errorf("the comparison is gone, so other objections surface only as a raw failure: %s", s)
	}
}

/*
Only the adapters being remapped are touched.

	One on a switch the destination also has stays connected and never notices
	this happened. There is no reason to interrupt it, and for the rest a moment
	disconnected is strictly better than a move that fails — the only other
	outcome on offer.
*/
func TestOnlyRemappedAdaptersAreDisconnected(t *testing.T) {
	s := moveWithNetworkMap([]types.EvacuationNIC{{SourceSwitch: "A", TargetSwitch: "B"}})
	if !strings.Contains(s, "if (-not $from -or -not $netMap.ContainsKey($from)) { continue }") {
		t.Fatalf("every adapter is disconnected, not only the remapped ones: %s", s)
	}
}

/*
A failed move puts the source VM's adapters back.

	Otherwise a move that did not happen becomes an outage that did: the VM stays
	where it was, off its network, with nothing to say why.
*/
func TestAFailedMoveRestoresTheSourceAdapters(t *testing.T) {
	s := moveWithNetworkMap([]types.EvacuationNIC{{SourceSwitch: "A", TargetSwitch: "B"}})
	if !strings.Contains(s, "Connect-VMNetworkAdapter -VMNetworkAdapter $back -SwitchName $p.From") {
		t.Fatalf("a failed move leaves the VM off its network: %s", s)
	}
	if !strings.Contains(s, "$p.OldVlan -gt 0") {
		t.Errorf("the original VLAN is not restored: %s", s)
	}
	if !strings.Contains(s, "put them back") {
		t.Errorf("the failure does not say the VM was restored: %s", s)
	}
}

// A VLAN retags the adapter on arrival; zero leaves the tag alone rather than
// clearing one somebody set.
func TestAVLANIsAppliedOnlyWhenGiven(t *testing.T) {
	with := moveWithNetworkMap([]types.EvacuationNIC{{SourceSwitch: "A", TargetSwitch: "B", VLANID: 40}})
	if !strings.Contains(with, `$vlanMap = @{'A' = 40}`) {
		t.Fatalf("the VLAN is not carried: %s", with)
	}
	if !strings.Contains(with, "if ($p.Vlan -gt 0)") {
		t.Errorf("a zero VLAN would be applied as a tag: %s", with)
	}
	without := moveWithNetworkMap([]types.EvacuationNIC{{SourceSwitch: "A", TargetSwitch: "B"}})
	if !strings.Contains(without, `$vlanMap = @{'A' = 0}`) {
		t.Errorf("an unset VLAN is not rendered as zero: %s", without)
	}
}

/*
The adapters come from the VM, so nothing depends on the report's shape.

	The old version picked incompatibilities apart looking for something carrying
	a SwitchName — a guess about somebody else's object model, and one of the
	three that turned out not to matter because the fix never took anyway. Asking
	the VM which switches its adapters are on has no such uncertainty.
*/
func TestTheAdaptersComeFromTheVMNotTheReport(t *testing.T) {
	s := moveWithNetworkMap([]types.EvacuationNIC{{SourceSwitch: "A", TargetSwitch: "B"}})

	if !strings.Contains(s, "foreach ($ad in @(Get-VMNetworkAdapter -VMName $vm") {
		t.Fatalf("the adapters are not enumerated from the VM: %s", s)
	}
	if strings.Contains(s, "PSObject.Properties.Name -contains 'SwitchName'") {
		t.Errorf("the script still picks the report apart by shape: %s", s)
	}
	if strings.Contains(s, "33012") {
		t.Errorf("the script branches on a magic message id: %s", s)
	}
}

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
