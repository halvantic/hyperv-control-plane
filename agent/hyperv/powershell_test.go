package hyperv

import (
	"context"
	"io"
	"strings"
	"testing"

	"log/slog"

	"github.com/joshua-fourie/ballast/api/types"
)

// fakeRunner returns queued outputs in order and records every script it was
// asked to run, standing in for powershell.exe.
type fakeRunner struct {
	responses [][]byte
	errs      []error
	calls     []string
	i         int
}

func (f *fakeRunner) run(_ context.Context, script string) ([]byte, error) {
	f.calls = append(f.calls, script)
	var out []byte
	var err error
	if f.i < len(f.responses) {
		out = f.responses[f.i]
	}
	if f.i < len(f.errs) {
		err = f.errs[f.i]
	}
	f.i++
	return out, err
}

func newTestPS(f *fakeRunner) *PowerShell {
	return &PowerShell{run: f.run, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestCollectInventoryParses(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte(`{
		"physicalAdapters":[{"name":"NIC1","mac":"00-15-5D-00-00-01","linkSpeedBps":25000000000,"up":true}],
		"physicalDisks":[{"deviceId":"0","sizeBytes":1920383410176,"mediaType":"SSD","canPool":true}],
		"totalMemoryBytes":137438953472,"logicalCPUs":32}`)}}
	inv, err := newTestPS(f).CollectInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.PhysicalAdapters) != 1 || inv.PhysicalAdapters[0].Name != "NIC1" ||
		inv.PhysicalAdapters[0].LinkSpeedBps != 25_000_000_000 || !inv.PhysicalAdapters[0].Up {
		t.Fatalf("adapter parse wrong: %+v", inv.PhysicalAdapters)
	}
	if len(inv.PhysicalDisks) != 1 || inv.PhysicalDisks[0].MediaType != "SSD" {
		t.Fatalf("disk parse wrong: %+v", inv.PhysicalDisks)
	}
	if inv.TotalMemoryBytes != 137_438_953_472 || inv.LogicalCPUs != 32 {
		t.Fatalf("memory/cpu parse wrong: %d %d", inv.TotalMemoryBytes, inv.LogicalCPUs)
	}
}

func TestDecodeJSONEmptyIsZero(t *testing.T) {
	var obs switchObservation
	if err := decodeJSON([]byte("  \n"), &obs); err != nil {
		t.Fatal(err)
	}
	if obs.Exists {
		t.Fatal("empty output should decode to zero value")
	}
}

func sampleSwitchSpec() types.VirtualSwitchSpec {
	return types.VirtualSwitchSpec{
		Name:              "ConvergedSwitch",
		TeamMembers:       []string{"NIC1", "NIC2"},
		LoadBalancing:     types.SETDynamic,
		AllowManagementOS: true,
	}
}

func TestPlanSwitch(t *testing.T) {
	spec := sampleSwitchSpec()
	tests := []struct {
		name string
		obs  switchObservation
		want switchPlan
	}{
		{"absent", switchObservation{Exists: false}, switchCreate},
		{"matches ignoring order", switchObservation{
			Exists: true, TeamMembers: []string{"NIC2", "NIC1"},
			LoadBalancing: "Dynamic", AllowManagementOS: true,
		}, switchNoop},
		{"member drift", switchObservation{
			Exists: true, TeamMembers: []string{"NIC1"},
			LoadBalancing: "Dynamic", AllowManagementOS: true,
		}, switchUpdate},
		{"lb drift", switchObservation{
			Exists: true, TeamMembers: []string{"NIC1", "NIC2"},
			LoadBalancing: "HyperVPort", AllowManagementOS: true,
		}, switchUpdate},
		{"mgmt-os drift", switchObservation{
			Exists: true, TeamMembers: []string{"NIC1", "NIC2"},
			LoadBalancing: "Dynamic", AllowManagementOS: false,
		}, switchUpdate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := planSwitch(spec, tt.obs); got != tt.want {
				t.Fatalf("planSwitch = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEnsureSwitchCreate(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte(`{"exists":false}`)}}
	out, err := newTestPS(f).EnsureSwitch(context.Background(), sampleSwitchSpec())
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeCreated {
		t.Fatalf("want Created, got %v", out)
	}
	if len(f.calls) != 2 {
		t.Fatalf("want query+act (2 calls), got %d", len(f.calls))
	}
	if !strings.Contains(f.calls[1], "New-VMSwitch") ||
		!strings.Contains(f.calls[1], "-EnableEmbeddedTeaming $true") {
		t.Fatalf("act script not a SET create: %s", f.calls[1])
	}
	// Weight mode must be set at creation or per-vNIC QoS weights fail (proven
	// on a real host). Regression guard.
	if !strings.Contains(f.calls[1], "-MinimumBandwidthMode Weight") {
		t.Fatalf("SET switch must be created in Weight bandwidth mode: %s", f.calls[1])
	}
}

func TestEnsureSwitchUnchangedDoesNotAct(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte(`{"exists":true,"teamMembers":["NIC1","NIC2"],"loadBalancing":"Dynamic","allowManagementOS":true}`)}}
	out, err := newTestPS(f).EnsureSwitch(context.Background(), sampleSwitchSpec())
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeUnchanged {
		t.Fatalf("want Unchanged, got %v", out)
	}
	if len(f.calls) != 1 {
		t.Fatalf("converged host must not run an act script; calls=%d", len(f.calls))
	}
}

func TestEnsureSwitchUpdate(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte(`{"exists":true,"teamMembers":["NIC1"],"loadBalancing":"Dynamic","allowManagementOS":true}`)}}
	out, err := newTestPS(f).EnsureSwitch(context.Background(), sampleSwitchSpec())
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeUpdated {
		t.Fatalf("want Updated, got %v", out)
	}
	if !strings.Contains(f.calls[1], "Set-VMSwitchTeam") {
		t.Fatalf("act script not a team update: %s", f.calls[1])
	}
}

func TestPlanVNIC(t *testing.T) {
	spec := types.ManagementVNICSpec{Name: "Management", SwitchName: "ConvergedSwitch", VLANID: 10}
	tests := []struct {
		name string
		obs  vnicObservation
		want vnicPlan
	}{
		{"absent", vnicObservation{Exists: false}, vnicCreate},
		{"matches", vnicObservation{Exists: true, SwitchName: "ConvergedSwitch", VlanID: 10}, vnicNoop},
		{"vlan drift", vnicObservation{Exists: true, SwitchName: "ConvergedSwitch", VlanID: 0}, vnicUpdate},
		{"switch drift", vnicObservation{Exists: true, SwitchName: "Other", VlanID: 10}, vnicUpdate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := planVNIC(spec, tt.obs); got != tt.want {
				t.Fatalf("planVNIC = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEnsureMgmtVNICCreate(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte(`{"exists":false}`)}}
	spec := types.ManagementVNICSpec{Name: "Management", SwitchName: "ConvergedSwitch", VLANID: 10, MinBandwidthWeight: 10}
	out, err := newTestPS(f).EnsureMgmtVNIC(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeCreated {
		t.Fatalf("want Created, got %v", out)
	}
	act := f.calls[1]
	if !strings.Contains(act, "Add-VMNetworkAdapter -ManagementOS -Name 'Management'") {
		t.Fatalf("act script missing adapter add: %s", act)
	}
	if !strings.Contains(act, "-Access -VlanId 10") {
		t.Fatalf("act script missing VLAN: %s", act)
	}
	if !strings.Contains(act, "-MinimumBandwidthWeight 10") {
		t.Fatalf("act script missing bandwidth weight: %s", act)
	}
}

func TestPSQuoteEscapesSingleQuotes(t *testing.T) {
	if got := psQuote("a'b"); got != "'a''b'" {
		t.Fatalf("psQuote escaping wrong: %s", got)
	}
}

func TestSameStringSet(t *testing.T) {
	if !sameStringSet([]string{"a", "b"}, []string{"b", "a"}) {
		t.Fatal("order should not matter")
	}
	if sameStringSet([]string{"a"}, []string{"a", "b"}) {
		t.Fatal("different sizes are not equal")
	}
	if !sameStringSet([]string{"a", "a", "b"}, []string{"a", "b"}) {
		t.Fatal("duplicates should not matter")
	}
}
