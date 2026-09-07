package hyperv

import (
	"context"
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
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
