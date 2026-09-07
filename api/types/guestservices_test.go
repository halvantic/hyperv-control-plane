package types

import (
	"strings"
	"testing"
)

func runningFor(sec int64) VMStatus {
	return VMStatus{PowerState: VMPowerRunning, UptimeSeconds: sec, Heartbeat: "OkApplicationsHealthy"}
}

/*
The case this exists for: a Linux guest with the drivers and without the

	daemon.

	Heartbeat comes from the guest KERNEL — hv_utils, mainline, present on every
	Rocky 8/9 boot. Key-Value Pair Exchange needs a userspace daemon from the
	hyperv-daemons package. A guest answering the first and not the second has
	the drivers and is missing the package, which is exactly one remedy.

	Without this the console shows a running machine with no OS and no IP and
	nothing anywhere saying why — a diagnosis Ballast could make and was leaving
	as two blank fields.
*/
func TestAGuestAnsweringHeartbeatButNotKVP(t *testing.T) {
	why := GuestKVPProblem(runningFor(600), true)
	if why == "" {
		t.Fatal("a guest answering heartbeat with no OS and no IP was not reported")
	}
	for _, want := range []string{"hyperv-daemons", "hypervkvpd", "Hyper-V Data Exchange"} {
		if !strings.Contains(why, want) {
			t.Errorf("the message does not name the remedy %q: %s", want, why)
		}
	}
	// And it corrects the assumption the operator most likely arrives with.
	if !strings.Contains(why, "no Hyper-V drivers to install") {
		t.Errorf("does not say the drivers are already in the kernel: %s", why)
	}
}

/*
Every way this must stay silent. Each is a real state, and flagging any of

	them is the false-alarm pattern that has cost this codebase most.
*/
func TestGuestKVPProblemStaysSilent(t *testing.T) {
	ok := runningFor(600)
	tests := []struct {
		name       string
		st         VMStatus
		kvpEnabled bool
	}{
		{"a stopped VM reports nothing and is not at fault",
			VMStatus{PowerState: VMPowerOff, Heartbeat: "OkApplicationsHealthy", UptimeSeconds: 600}, true},
		{"KVP switched off is the setting working as asked", ok, false},
		{"a guest still booting has not had time to answer", runningFor(30), true},
		{"an OS name is already coming back, so the daemon answers",
			VMStatus{PowerState: VMPowerRunning, UptimeSeconds: 600, Heartbeat: "Ok", GuestOS: "Rocky Linux"}, true},
		{"an IP is already coming back",
			VMStatus{PowerState: VMPowerRunning, UptimeSeconds: 600, Heartbeat: "Ok", IPAddress: "192.168.1.144"}, true},
		/* No heartbeat either: this guest is not running the integration
		   services at all — an appliance, a live ISO, a kernel without the
		   modules — which is a different problem with a different answer.
		   Naming a package here sends somebody to install the wrong thing. */
		{"a guest answering nothing at all is a different problem",
			VMStatus{PowerState: VMPowerRunning, UptimeSeconds: 600, Heartbeat: "Lost"}, true},
		{"and so is one reporting no heartbeat value",
			VMStatus{PowerState: VMPowerRunning, UptimeSeconds: 600}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if why := GuestKVPProblem(tc.st, tc.kvpEnabled); why != "" {
				t.Errorf("flagged a guest that is fine: %s", why)
			}
		})
	}
}

// Hyper-V's heartbeat values, and which of them means a guest is answering.
func TestHeartbeatAnswering(t *testing.T) {
	for _, v := range []string{"Ok", "OkApplicationsHealthy", "OkApplicationsUnknown"} {
		if !heartbeatAnswering(v) {
			t.Errorf("%q should read as answering", v)
		}
	}
	// Empty is "nothing was said", which is not a fault and must not read as one.
	for _, v := range []string{"", "Lost", "Error", "Disabled", "Paused"} {
		if heartbeatAnswering(v) {
			t.Errorf("%q should not read as answering", v)
		}
	}
}

/*
An unreported service list is not a disabled one.

	A VM nobody has read must not be assumed to have KVP off, which would
	silence the check on exactly the machines nobody has looked at.
*/
func TestKVPEnabledIn(t *testing.T) {
	on := []VMIntegrationServiceState{{Name: "Key-Value Pair Exchange", Enabled: true}}
	if e, known := KVPEnabledIn(on); !e || !known {
		t.Errorf("enabled=%v known=%v, want both true", e, known)
	}
	off := []VMIntegrationServiceState{{Name: "Key-Value Pair Exchange", Enabled: false}}
	if e, known := KVPEnabledIn(off); e || !known {
		t.Errorf("enabled=%v known=%v, want false,true", e, known)
	}
	if _, known := KVPEnabledIn(nil); known {
		t.Error("an unread list reported as known; absent is not disabled")
	}
	if _, known := KVPEnabledIn([]VMIntegrationServiceState{{Name: "Heartbeat", Enabled: true}}); known {
		t.Error("a list without KVP reported as known")
	}
}
