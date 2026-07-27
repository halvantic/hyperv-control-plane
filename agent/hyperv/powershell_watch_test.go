package hyperv

import (
	"context"
	"strings"
	"testing"
)

// WatchVMState fires onEvent once per EVENT line and ignores anything else, and
// its script subscribes to VM power-state changes via a CIM indication.
func TestWatchVMStateFiresOnEventOnly(t *testing.T) {
	f := &fakeRunner{streamLines: []string{"EVENT", "", "some noise", "EVENT", "EVENT"}}
	n := 0
	if err := newTestPS(f).WatchVMState(context.Background(), func() { n++ }); err != nil {
		t.Fatalf("WatchVMState: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 events, got %d", n)
	}
	if !strings.Contains(f.streamScript, "Register-CimIndicationEvent") ||
		!strings.Contains(f.streamScript, "Msvm_ComputerSystem") ||
		!strings.Contains(f.streamScript, "EnabledState <> PreviousInstance.EnabledState") {
		t.Fatalf("watch script does not subscribe to VM power-state changes:\n%s", f.streamScript)
	}
}
