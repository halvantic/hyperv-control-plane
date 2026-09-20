package hyperv

import (
	"context"
	"errors"
	"strings"
	"testing"
)

/* RemoveSwitch and iSCSI: a converged switch can carry the management-OS vNIC
   that an iSCSI session is bound to. JobRemoveSwitch and JobResetISCSIInitiator
   are dispatched as independent, concurrently-run jobs for the same host (see
   fanOutDecommissionCleanup and runJobs), so Remove-VMSwitch could be asked to
   tear a switch down while a session still held that vNIC's address open.

   The fix is narrower than ResetISCSIInitiator on purpose: it disconnects only
   a session bound to an address belonging to the switch actually being
   removed, leaving iSCSI traffic on every other switch or NIC untouched. */

func TestRemoveSwitchFastPathIsUnchanged(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte(`RESULT={"disconnected":0,"disconnectErrors":[]}`)}}
	msg, err := newTestPS(f).RemoveSwitch(context.Background(), "ConvergedSwitch")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("expected one script call, got %d", len(f.calls))
	}
	script := f.calls[0]
	// The overwhelmingly common case: no iSCSI mention needed in the reported
	// outcome when nothing was disconnected.
	if strings.Contains(msg, "disconnected") {
		t.Errorf("a no-op disconnect must not be reported as one: %q", msg)
	}
	if !strings.Contains(msg, `removed switch "ConvergedSwitch"`) {
		t.Errorf("the switch removal must still be reported: %q", msg)
	}
	// The removal itself is exactly as before: idempotent, -Force, guarded by
	// Get-VMSwitch.
	if !strings.Contains(script, "if (Get-VMSwitch -Name $switch -ErrorAction SilentlyContinue)") {
		t.Error("the switch removal must stay idempotent")
	}
	if !strings.Contains(script, "Remove-VMSwitch -Name $switch -Force") {
		t.Error("the removal must still use -Force")
	}
}

// Order matters: a matched session must be disconnected BEFORE the switch is
// removed, or the removal is what triggers the exact failure this exists to
// avoid.
func TestRemoveSwitchDisconnectsBeforeRemoving(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte(`RESULT={"disconnected":1,"disconnectErrors":[]}`)}}
	msg, err := newTestPS(f).RemoveSwitch(context.Background(), "ConvergedSwitch")
	if err != nil {
		t.Fatal(err)
	}
	script := f.calls[0]

	disconnect := strings.Index(script, "$tgt | Disconnect-IscsiTarget -Confirm:$false -ErrorAction Stop")
	remove := strings.Index(script, "Remove-VMSwitch -Name $switch -Force")
	if disconnect < 0 {
		t.Fatal("no disconnect call found in the script")
	}
	if remove < 0 {
		t.Fatal("no switch removal found in the script")
	}
	if disconnect > remove {
		t.Error("a matched iSCSI session must be disconnected before the switch is removed")
	}

	if !strings.Contains(msg, "disconnected 1 iSCSI session(s)") {
		t.Errorf("a disconnected session must be reported: %q", msg)
	}

	// Correlation is by piping the session into Get-IscsiConnection, not by
	// rebuilding an identifier — the same lesson ResetISCSIInitiator already
	// learned about Disconnect-IscsiTarget.
	if !strings.Contains(script, "$s | Get-IscsiConnection -ErrorAction SilentlyContinue") {
		t.Error("a connection must be correlated to its session by piping the session object")
	}
	// The disconnect itself must go through Get-IscsiTarget, not the session
	// object, for the same case-sensitivity reason as ResetISCSIInitiator.
	if strings.Contains(script, "Disconnect-IscsiTarget -NodeAddress") {
		t.Error("naming the target reintroduces the RFC 3720 case mismatch — pipe the connected target instead")
	}
	if !strings.Contains(script, "Get-IscsiTarget -ErrorAction Stop | Where-Object { $_.IsConnected }") {
		t.Error("the disconnect step must list connected targets via Get-IscsiTarget")
	}
	// A management-OS vNIC surfaces to Windows networking as "vEthernet (<name>)".
	if !strings.Contains(script, "'vEthernet (' + [string]$v.Name + ')'") {
		t.Error("the vNIC's host-side adapter must be found by its vEthernet alias")
	}
	if !strings.Contains(script, "Get-VMNetworkAdapter -ManagementOS -SwitchName $switch") {
		t.Error("only the management-OS vNICs on THIS switch may be considered")
	}
}

// A failed disconnect must not block the switch removal — Remove-VMSwitch's
// own error is the more useful one to the operator, and a disconnect failure
// here may not even be why the removal fails.
func TestRemoveSwitchStillRemovesWhenDisconnectFails(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{
		[]byte(`RESULT={"disconnected":0,"disconnectErrors":["iqn.syn:t1: The session is in use by a clustered disk"]}`),
	}}
	msg, err := newTestPS(f).RemoveSwitch(context.Background(), "ConvergedSwitch")
	if err != nil {
		t.Fatal(err)
	}
	script := f.calls[0]
	if !strings.Contains(script, "Remove-VMSwitch -Name $switch -Force") {
		t.Fatal("the switch removal must still be attempted after a disconnect failure")
	}
	if !strings.Contains(msg, "could not disconnect") || !strings.Contains(msg, "in use by a clustered disk") {
		t.Errorf("a disconnect failure must be named in the reported message: %q", msg)
	}
	if !strings.Contains(msg, "the switch removal was attempted regardless") {
		t.Errorf("it must say the removal went ahead anyway: %q", msg)
	}
}

// A genuine PowerShell failure (the switch itself would not remove) still
// surfaces as an error, same as before this change.
func TestRemoveSwitchSurfacesARealFailure(t *testing.T) {
	f := &fakeRunner{
		responses: [][]byte{[]byte("")},
		errs:      []error{errors.New("powershell: exit status 1: Remove-VMSwitch : some session is holding this switch open")},
	}
	_, err := newTestPS(f).RemoveSwitch(context.Background(), "ConvergedSwitch")
	if err == nil {
		t.Fatal("a real PowerShell failure must surface as an error")
	}
	if !strings.Contains(err.Error(), "some session is holding this switch open") {
		t.Errorf("the underlying reason must reach the caller: %v", err)
	}
}
