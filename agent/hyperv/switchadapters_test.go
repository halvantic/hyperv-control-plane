package hyperv

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

/* Swapping a NIC is ordinary maintenance, and Windows gives the replacement a new
   name — 'Ethernet0 2' where the spec still says 'Ethernet0'. Done on HVNEW03,
   the console said:

     Virtual switch — ConvergedSwitch
     update switch "ConvergedSwitch": powershell: exit status 1:
     Set-VMSwitchTeam : Physical network adapter 'Ethernet0 2' not found.

   That names the cmdlet that failed and nothing a person can do about it. Which
   adapters this host actually has, and which are free, is a fact the agent can
   establish — it is running ON the host — so leaving it as research for the
   operator is the defect CLAUDE.md describes, not a limitation. */

// switchUpdateFails queues the observation that forces an update, the failure
// from Set-VMSwitchTeam, and then the adapter enumeration the diagnosis makes.
func switchUpdateFails(adaptersJSON string) *fakeRunner {
	return &fakeRunner{
		responses: [][]byte{
			[]byte(`{"exists":true,"known":true,"teamMembers":["Ethernet0"],"loadBalancing":"Dynamic","allowManagementOS":true}`),
			nil,
			[]byte(adaptersJSON),
		},
		errs: []error{
			nil,
			errors.New("powershell: exit status 1: Set-VMSwitchTeam : Physical network adapter 'NIC1' not found."),
			nil,
		},
	}
}

func TestAMissingTeamMemberNamesTheNICsTheHostActuallyHas(t *testing.T) {
	f := switchUpdateFails(`[
	  {"name":"Ethernet0 2","mac":"00-15-5D-00-00-01","up":true,"inUseBy":"","ipv4":""},
	  {"name":"Ethernet1","mac":"00-15-5D-00-00-02","up":true,"inUseBy":"OtherSwitch","ipv4":"192.168.1.73"}
	]`)

	_, err := newTestPS(f).EnsureSwitch(context.Background(), sampleSwitchSpec())
	if err == nil {
		t.Fatal("the operation failed; it must still report a failure")
	}
	msg := err.Error()

	// The cause: which declared member is not there.
	if !strings.Contains(msg, `"NIC1"`) || !strings.Contains(msg, `"NIC2"`) {
		t.Errorf("the missing team members must be named: %q", msg)
	}
	// The facts needed to choose a replacement.
	if !strings.Contains(msg, "Ethernet0 2") || !strings.Contains(msg, "free") {
		t.Errorf("a free adapter is what the operator remaps onto and must be listed: %q", msg)
	}
	if !strings.Contains(msg, "already teamed in OtherSwitch") {
		t.Errorf("an adapter another switch already owns must say so, or it reads as available: %q", msg)
	}
	if !strings.Contains(msg, "carries 192.168.1.73") {
		t.Errorf("a NIC holding a host address must be flagged before anyone teams it: %q", msg)
	}
	// The action.
	if !strings.Contains(msg, "Edit the switch") {
		t.Errorf("a known failure with a known remedy must offer the action: %q", msg)
	}
	// And the cmdlet's own words survive for the log.
	if !strings.Contains(msg, "Set-VMSwitchTeam") {
		t.Errorf("the underlying error must not be thrown away: %q", msg)
	}
}

// The agent never invents intent. Which fabric a NIC belongs to is not something
// its name says, and teaming the wrong one takes the host off the network — so
// it must not quietly pick the free adapter and remap the switch itself.
func TestTheAgentDoesNotRemapTheSwitchItself(t *testing.T) {
	f := switchUpdateFails(`[{"name":"Ethernet0 2","mac":"00-15-5D-00-00-01","up":true,"inUseBy":"","ipv4":""}]`)

	_, err := newTestPS(f).EnsureSwitch(context.Background(), sampleSwitchSpec())
	if err == nil {
		t.Fatal("want the failure to stand")
	}
	for _, script := range f.calls {
		if strings.Contains(script, "Set-VMSwitchTeam -Name") && strings.Contains(script, "Ethernet0 2") {
			t.Fatalf("the agent remapped the team on its own; desired state is the operator's: %s", script)
		}
	}
	if !strings.Contains(err.Error(), "Ballast will not choose for you") {
		t.Errorf("it must say why it is not choosing, or it reads as an unhelpful refusal: %q", err.Error())
	}
}

// A failure that is NOT about a missing adapter must come through untouched.
// Dressing an unrelated error up as a NIC problem sends someone to remap
// hardware that was never the trouble.
func TestAnUnrelatedFailureIsNotBlamedOnTheNICs(t *testing.T) {
	f := switchUpdateFails(`[{"name":"NIC1","mac":"00-15-5D-00-00-01","up":true,"inUseBy":"","ipv4":""},
	  {"name":"NIC2","mac":"00-15-5D-00-00-02","up":true,"inUseBy":"","ipv4":""}]`)

	_, err := newTestPS(f).EnsureSwitch(context.Background(), sampleSwitchSpec())
	if err == nil {
		t.Fatal("want the failure to stand")
	}
	if strings.Contains(err.Error(), "no network adapter named") {
		t.Fatalf("every declared member is present; the cause is something else: %q", err.Error())
	}
}

// If the adapters cannot be read, the original error stands. A diagnosis that
// could not be made must not displace the evidence that something failed.
func TestAFailedAdapterReadLeavesTheOriginalError(t *testing.T) {
	f := &fakeRunner{
		responses: [][]byte{
			[]byte(`{"exists":true,"known":true,"teamMembers":["Ethernet0"],"loadBalancing":"Dynamic","allowManagementOS":true}`),
			nil,
			nil,
		},
		errs: []error{nil, errors.New("Set-VMSwitchTeam : Physical network adapter 'NIC1' not found."), errors.New("Get-NetAdapter failed")},
	}

	_, err := newTestPS(f).EnsureSwitch(context.Background(), sampleSwitchSpec())
	if err == nil || !strings.Contains(err.Error(), "Set-VMSwitchTeam") {
		t.Fatalf("the underlying failure must survive an undiagnosable case: %v", err)
	}
}

// One missing member reads as one, not as a list of one.
func TestNamesAreListedTheWayAPersonReadsThem(t *testing.T) {
	for _, tc := range []struct {
		names []string
		want  string
	}{
		{[]string{"NIC1"}, `"NIC1"`},
		{[]string{"NIC1", "NIC2"}, `"NIC1" or "NIC2"`},
		{[]string{"NIC1", "NIC2", "Ethernet0"}, `"NIC1", "NIC2" or "Ethernet0"`},
		{nil, ""},
	} {
		if got := quoteList(tc.names); got != tc.want {
			t.Errorf("quoteList(%v) = %q, want %q", tc.names, got, tc.want)
		}
	}
}

// A single missing member still produces the whole diagnosis.
func TestASingleMissingAdapterIsStillDiagnosed(t *testing.T) {
	spec := types.VirtualSwitchSpec{Name: "ConvergedSwitch", TeamMembers: []string{"NIC1"},
		LoadBalancing: types.SETDynamic, AllowManagementOS: true}
	f := switchUpdateFails(`[{"name":"Ethernet0 2","mac":"00-15-5D-00-00-01","up":true,"inUseBy":"","ipv4":""}]`)

	_, err := newTestPS(f).EnsureSwitch(context.Background(), spec)
	if err == nil {
		t.Fatal("want the failure to stand")
	}
	if !strings.Contains(err.Error(), `named "NIC1", which the switch declares`) {
		t.Errorf("one missing name must read as one: %q", err.Error())
	}
}
