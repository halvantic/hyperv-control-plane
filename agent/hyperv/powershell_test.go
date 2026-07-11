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
	// streamLines are emitted, in order, to onLine by the stream runner; streamErr
	// is returned after them. streamScript records the streamed script.
	streamLines  []string
	streamErr    error
	streamScript string
}

func (f *fakeRunner) runStream(_ context.Context, script string, onLine func(string)) error {
	f.streamScript = script
	for _, l := range f.streamLines {
		if onLine != nil {
			onLine(l)
		}
	}
	return f.streamErr
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
	return &PowerShell{run: f.run, runStream: f.runStream, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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

// Cluster cmdlets emit WARNING lines ahead of the JSON; decode must tolerate the
// preamble and read the final JSON line.
func TestDecodeJSONIgnoresWarningPreamble(t *testing.T) {
	out := []byte("WARNING: Node HV01: No disks found to be used for cache\nWARNING: Node HV02: ...\n{\"s2dEnabled\":true,\"volumes\":[\"Vol01\"]}")
	var obs storageObservation
	if err := decodeJSON(out, &obs); err != nil {
		t.Fatal(err)
	}
	if !obs.S2DEnabled || len(obs.Volumes) != 1 || obs.Volumes[0] != "Vol01" {
		t.Fatalf("decode wrong: %+v", obs)
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

func TestIPDiffers(t *testing.T) {
	desired := &types.IPConfig{Address: "10.0.0.21/24", Gateway: "10.0.0.1", DNSServers: []string{"10.0.0.1", "10.0.0.2"}}
	tests := []struct {
		name string
		obs  ipObservation
		want bool
	}{
		{"exact match", ipObservation{Address: "10.0.0.21/24", Gateway: "10.0.0.1", DNSServers: []string{"10.0.0.1", "10.0.0.2"}, Registers: true}, false},
		{"registration drift", ipObservation{Address: "10.0.0.21/24", Gateway: "10.0.0.1", DNSServers: []string{"10.0.0.1", "10.0.0.2"}, Registers: false}, true},
		{"dhcp / no static", ipObservation{Address: "", Gateway: "", DNSServers: nil}, true},
		{"address drift", ipObservation{Address: "10.0.0.99/24", Gateway: "10.0.0.1", DNSServers: []string{"10.0.0.1", "10.0.0.2"}, Registers: true}, true},
		{"gateway drift", ipObservation{Address: "10.0.0.21/24", Gateway: "", DNSServers: []string{"10.0.0.1", "10.0.0.2"}, Registers: true}, true},
		{"dns order matters", ipObservation{Address: "10.0.0.21/24", Gateway: "10.0.0.1", DNSServers: []string{"10.0.0.2", "10.0.0.1"}, Registers: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ipDiffers(desired, tt.obs); got != tt.want {
				t.Fatalf("ipDiffers = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseCIDR(t *testing.T) {
	ip, prefix, err := parseCIDR("10.0.0.21/24")
	if err != nil || ip != "10.0.0.21" || prefix != 24 {
		t.Fatalf("parseCIDR = %q/%d err=%v", ip, prefix, err)
	}
	if _, _, err := parseCIDR("not-a-cidr"); err == nil {
		t.Fatal("expected error on malformed CIDR")
	}
}

// The IP apply must free the desired address from any interface that already
// holds it (e.g. an AllowManagementOS auto-vNIC took it via DHCP) before adding
// it, or New-NetIPAddress fails "object already exists" (Windows error 5010)
// when the address moves between vNICs. It must still add on the target vNIC.
func TestApplyIPScriptFreesAddressOnOtherInterface(t *testing.T) {
	s := applyIPScript("ManagementHost", "192.168.1.159", 24, &types.IPConfig{Address: "192.168.1.159/24"})
	if !strings.Contains(s, "Get-NetIPAddress -IPAddress '192.168.1.159' -AddressFamily IPv4") {
		t.Fatalf("apply script does not free the address from other interfaces:\n%s", s)
	}
	if !strings.Contains(s, "New-NetIPAddress -InterfaceAlias $alias -IPAddress '192.168.1.159' -PrefixLength 24") {
		t.Fatalf("apply script add line wrong:\n%s", s)
	}
}

// A vNIC that exists and matches, but whose IP has drifted from DHCP to the
// desired static, reports Updated and runs the IP apply exactly once.
func TestEnsureMgmtVNICAppliesIPOnDrift(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{
		[]byte(`{"exists":true,"switchName":"ConvergedSwitch","vlanID":0}`), // queryVNIC: adapter matches
		[]byte(`{"address":"","gateway":"","dnsServers":[]}`),               // queryVNICIP: no static yet
	}}
	spec := types.ManagementVNICSpec{
		Name: "Management", SwitchName: "ConvergedSwitch", VLANID: 0,
		IPConfig: &types.IPConfig{Address: "10.0.0.21/24", DNSServers: []string{"10.0.0.1"}},
	}
	out, err := newTestPS(f).EnsureMgmtVNIC(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeUpdated {
		t.Fatalf("want Updated on IP drift, got %v", out)
	}
	// calls: queryVNIC, queryVNICIP, applyIP
	if len(f.calls) != 3 {
		t.Fatalf("want 3 calls (adapter query, ip query, ip apply), got %d", len(f.calls))
	}
	apply := f.calls[2]
	if !strings.Contains(apply, "New-NetIPAddress") || !strings.Contains(apply, "-IPAddress '10.0.0.21' -PrefixLength 24") {
		t.Fatalf("apply script wrong: %s", apply)
	}
	if strings.Contains(apply, "-DefaultGateway") {
		t.Fatalf("no gateway desired, but apply set one: %s", apply)
	}
}

// A fully converged vNIC (adapter and IP both match) makes no changes.
func TestEnsureMgmtVNICConvergedNoChange(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{
		[]byte(`{"exists":true,"switchName":"ConvergedSwitch","vlanID":0}`),
		[]byte(`{"address":"10.0.0.21/24","gateway":"","dnsServers":["10.0.0.1"],"registers":true}`),
	}}
	spec := types.ManagementVNICSpec{
		Name: "Management", SwitchName: "ConvergedSwitch", VLANID: 0,
		IPConfig: &types.IPConfig{Address: "10.0.0.21/24", DNSServers: []string{"10.0.0.1"}},
	}
	out, err := newTestPS(f).EnsureMgmtVNIC(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeUnchanged {
		t.Fatalf("want Unchanged on converged vNIC, got %v", out)
	}
	if len(f.calls) != 2 {
		t.Fatalf("converged vNIC must not run an apply; calls=%d", len(f.calls))
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
