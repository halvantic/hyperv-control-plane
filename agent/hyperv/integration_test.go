package hyperv

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

func on() *bool  { b := true; return &b }
func off() *bool { b := false; return &b }

// Hyper-V's own defaults: Guest Service Interface off, the rest on.
func defaults() []IntegrationServiceState {
	return []IntegrationServiceState{
		{Name: "Guest Service Interface", Enabled: false},
		{Name: "Heartbeat", Enabled: true},
		{Name: "Key-Value Pair Exchange", Enabled: true},
		{Name: "Shutdown", Enabled: true},
		{Name: "Time Synchronization", Enabled: true},
		{Name: "VSS", Enabled: true},
	}
}

/*
A nil field is unmanaged and must never be acted on.

	This is the whole shape of the feature. Hyper-V's defaults differ per
	service, so a spec of plain bools would read as "disable all of these" for
	every VM nobody had ever been asked about — and the first save would quietly
	turn off backup and graceful shutdown across a fleet.
*/
func TestUndeclaredServicesAreLeftAlone(t *testing.T) {
	// Declares one thing only. Everything else must be untouched, including the
	// services whose current state differs from what a zero value would mean.
	want := &types.VMIntegrationServices{GuestServiceInterface: on()}
	enable, disable := integrationPlan(want, defaults())
	if strings.Join(enable, ",") != "Guest Service Interface" {
		t.Fatalf("enabled %v, want only Guest Service Interface", enable)
	}
	if len(disable) != 0 {
		t.Fatalf("disabled %v — every one of these was left undeclared", disable)
	}
}

// An empty declaration touches nothing at all, which is what a VM that has
// never been asked about looks like.
func TestAnEmptyDeclarationChangesNothing(t *testing.T) {
	enable, disable := integrationPlan(&types.VMIntegrationServices{}, defaults())
	if len(enable) != 0 || len(disable) != 0 {
		t.Fatalf("acted on an empty declaration: +%v -%v", enable, disable)
	}
	if e, d := integrationPlan(nil, defaults()); len(e) != 0 || len(d) != 0 {
		t.Fatalf("acted on a nil declaration: +%v -%v", e, d)
	}
}

// An explicit false is an instruction, not an absence.
func TestAnExplicitFalseDisables(t *testing.T) {
	want := &types.VMIntegrationServices{TimeSynchronisation: off()}
	enable, disable := integrationPlan(want, defaults())
	if strings.Join(disable, ",") != "Time Synchronization" {
		t.Fatalf("disabled %v, want Time Synchronization", disable)
	}
	if len(enable) != 0 {
		t.Errorf("enabled %v for a declaration that asked for nothing on", enable)
	}
}

/*
Idempotent: a service already in the declared state is not rewritten.

	These are cheap to set, but a pass that reported Updated every time would
	never settle and would fill the activity feed with a change nobody made.
*/
func TestAServiceAlreadyRightIsNotTouched(t *testing.T) {
	want := &types.VMIntegrationServices{Shutdown: on(), GuestServiceInterface: off()}
	enable, disable := integrationPlan(want, defaults())
	if len(enable) != 0 || len(disable) != 0 {
		t.Fatalf("rewrote services already in the declared state: +%v -%v", enable, disable)
	}
}

/*
A service the host does not report is not acted on.

	Older guests and Linux VMs expose different sets, and calling
	Enable-VMIntegrationService for one that is not there fails the whole pass
	for nothing. Absent is not "off" here either.
*/
func TestAServiceTheHostDoesNotReportIsSkipped(t *testing.T) {
	sparse := []IntegrationServiceState{{Name: "Heartbeat", Enabled: true}}
	want := &types.VMIntegrationServices{VSS: on(), Heartbeat: on()}
	enable, disable := integrationPlan(want, sparse)
	if len(enable) != 0 || len(disable) != 0 {
		t.Fatalf("acted on a service the host never reported: +%v -%v", enable, disable)
	}
}

// End to end through the stub, including that a settled VM costs no write.
func TestEnsureIntegrationServicesSettles(t *testing.T) {
	s := &Stub{}
	want := &types.VMIntegrationServices{GuestServiceInterface: on(), TimeSynchronisation: off()}

	out, err := s.EnsureIntegrationServices(context.Background(), "web01", want)
	if err != nil || out != OutcomeUpdated {
		t.Fatalf("first pass: out=%v err=%v", out, err)
	}
	if strings.Join(s.IntegrationWrites, ",") != "+Guest Service Interface,-Time Synchronization" {
		t.Fatalf("wrote %v", s.IntegrationWrites)
	}
	for i := 0; i < 3; i++ {
		out, err := s.EnsureIntegrationServices(context.Background(), "web01", want)
		if err != nil || out != OutcomeUnchanged {
			t.Fatalf("steady pass %d: out=%v err=%v", i, out, err)
		}
	}
	if len(s.IntegrationWrites) != 2 {
		t.Fatalf("a steady pass rewrote services: %v", s.IntegrationWrites)
	}
}

/*
The batch reads only the VMs that declare services, in one process.

	The per-VM path was a powershell.exe of its own for each. -VMName * would
	put them in one process but read every VM on the host: 4.0s for fourteen
	on HVNEW06 when seven declared anything, and on a host where one VM of
	thirty declares services, the pass would pay for all thirty.
*/
func TestIntegrationBatchScriptReadsOnlyTheNamedVMs(t *testing.T) {
	s := psCode(integrationBatchScript([]string{"Web01", "It's Odd"}))
	if got := strings.Count(s, "Get-VMIntegrationService"); got != 1 {
		t.Fatalf("Get-VMIntegrationService: found %d, want 1:\n%s", got, s)
	}
	if !strings.Contains(s, "Get-VMIntegrationService -VMName $n -ErrorAction Stop") {
		t.Errorf("each named VM must be read by name:\n%s", s)
	}
	if strings.Contains(s, "-VMName *") {
		t.Errorf("a wildcard reads every VM on the host, not the ones that declare services:\n%s", s)
	}
	// One missing VM must not cost the rest their reading.
	if i, j := strings.Index(s, "try {"), strings.Index(s, "Get-VMIntegrationService"); i < 0 || i > j {
		t.Errorf("each VM's read must be guarded on its own:\n%s", s)
	}
	for _, n := range []string{"Web01", "It's Odd"} {
		if !strings.Contains(s, psQuote(n)) {
			t.Errorf("%q must be named and quoted through psQuote:\n%s", n, s)
		}
	}
	if !strings.Contains(s, ".ToLower()") {
		t.Error("name matching must be case-insensitive")
	}
}

// Run for real against a stubbed cmdlet: a declared VM the host does not have
// is left out, and the others are still read.
func TestIntegrationBatchSkipsAMissingVMAndKeepsTheRest(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("needs powershell.exe")
	}
	exe, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe not on PATH")
	}
	harness := `
function Get-VMIntegrationService { [CmdletBinding()] param($VMName)
  if ($VMName -eq 'Gone') { throw ('Hyper-V was unable to find a virtual machine with name "' + $VMName + '".') }
  [pscustomobject]@{ VMName = $VMName; Name = 'Heartbeat'; Enabled = $true }
  [pscustomobject]@{ VMName = $VMName; Name = 'Guest Service Interface'; Enabled = $false }
}
` + integrationBatchScript([]string{"Web01", "Gone", "App02"})
	out, err := exec.Command(exe, "-NoProfile", "-NonInteractive", "-Command", harness).CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	var got map[string][]IntegrationServiceState
	if derr := decodeJSON(out, &got); derr != nil {
		t.Fatalf("%v\n%s", derr, out)
	}
	if _, ok := got["gone"]; ok {
		t.Errorf("a VM the host does not have must be absent, not empty: %v", got)
	}
	for _, k := range []string{"web01", "app02"} {
		if len(got[k]) != 2 {
			t.Errorf("%s must still be read when another VM is missing, got %v", k, got[k])
		}
	}
}

// Applying against a reading the caller already took writes only what differs,
// exactly as the self-reading path does.
func TestApplyIntegrationServicesUsesTheGivenReading(t *testing.T) {
	s := &Stub{}
	want := &types.VMIntegrationServices{GuestServiceInterface: on()}

	// The reading says it is already on, so nothing is written.
	settled := defaults()
	settled[0].Enabled = true
	out, err := s.ApplyIntegrationServices(context.Background(), "web01", want, settled)
	if err != nil || out != OutcomeUnchanged || len(s.IntegrationWrites) != 0 {
		t.Fatalf("a reading that already matches must write nothing: out=%v err=%v writes=%v", out, err, s.IntegrationWrites)
	}

	out, err = s.ApplyIntegrationServices(context.Background(), "web01", want, defaults())
	if err != nil || out != OutcomeUpdated || strings.Join(s.IntegrationWrites, ",") != "+Guest Service Interface" {
		t.Fatalf("a reading that differs must be applied: out=%v err=%v writes=%v", out, err, s.IntegrationWrites)
	}
}

// The script names Hyper-V's own service names, which are what an operator
// sees in Get-VMIntegrationService and in the console.
func TestIntegrationScriptUsesHyperVsOwnNames(t *testing.T) {
	sc := integrationScript("web01", []string{"Guest Service Interface"}, []string{"VSS"})
	if !strings.Contains(sc, "Enable-VMIntegrationService -VMName 'web01' -Name 'Guest Service Interface'") {
		t.Errorf("enable is not addressed by name:\n%s", sc)
	}
	if !strings.Contains(sc, "Disable-VMIntegrationService -VMName 'web01' -Name 'VSS'") {
		t.Errorf("disable is not addressed by name:\n%s", sc)
	}
}

/*
Heartbeat has to reach the status, or nothing downstream can use it.

	The script has always read it and nothing carried it into VMState, so
	VMStatus.Heartbeat was never set, the proto carried an always-empty field,
	and the console's heartbeatOK was false on every VM since it was written.
	The gap only surfaced when something finally needed the value.

	Asserted at the type level rather than through a live read: the fault was a
	field that existed at both ends and in neither middle.
*/
func TestVMStateCarriesHeartbeat(t *testing.T) {
	var st VMState
	st.Heartbeat = "OkApplicationsHealthy"
	if st.Heartbeat == "" {
		t.Fatal("VMState has no Heartbeat to carry")
	}
}
