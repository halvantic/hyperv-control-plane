package hyperv

import (
	"context"
	"strings"
	"testing"
)

func clusterIPScriptFor(t *testing.T, ip string, resp string) string {
	t.Helper()
	f := &fakeRunner{responses: [][]byte{[]byte(resp)}}
	if _, _, err := newTestPS(f).EnsureClusterIP(context.Background(), ip); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("expected one script call, got %d", len(f.calls))
	}
	return f.calls[0]
}

// The properties that keep a re-address from breaking a working cluster.
func TestClusterIPScriptShape(t *testing.T) {
	script := clusterIPScriptFor(t, "192.168.1.230", "RESULT=NOOP")
	for _, want := range []struct{ needle, why string }{
		{"$want = '192.168.1.230'", "the target address must be quoted into the script"},
		{"'Cluster Group'", "only the CORE group's address resource may be touched"},
		{"'IP Address'", "only an IP Address resource may be re-addressed"},
		{"RESULT=NOOP", "an address that already matches must not take anything offline"},
		{"Get-ClusterNetwork", "the mask must come from the cluster network, not be invented"},
		{"no cluster network covers", "an address on no member's subnet must be refused with the reason"},
		{"Stop-ClusterResource", "the resource has to be offline to take a new address"},
		{"Set-ClusterParameter", "the address is set through the cluster's own API"},
		{"Start-ClusterGroup", "the core group must be brought back up afterwards"},
	} {
		if !strings.Contains(script, want.needle) {
			t.Errorf("script missing %q — %s\n---\n%s", want.needle, want.why, script)
		}
	}
}

// The no-op has to be decided BEFORE anything is stopped, or reconciling a
// correct cluster would take its name offline every pass.
func TestAMatchingAddressIsDecidedBeforeAnythingStops(t *testing.T) {
	script := clusterIPScriptFor(t, "192.168.1.230", "RESULT=NOOP")
	noop := strings.Index(script, "RESULT=NOOP")
	stop := strings.Index(script, "Stop-ClusterResource")
	if noop < 0 || stop < 0 {
		t.Fatal("both the no-op path and the stop must be present")
	}
	if noop > stop {
		t.Error("the already-correct check runs after the resource is stopped; a correct cluster would be disrupted on every pass")
	}
}

// An empty address is "not declared" and must not even reach PowerShell.
func TestAnEmptyAddressRunsNothing(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("")}}
	out, _, err := newTestPS(f).EnsureClusterIP(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeUnchanged {
		t.Fatalf("an undeclared address must be unchanged, got %v", out)
	}
	if len(f.calls) != 0 {
		t.Fatalf("an undeclared address must run no script, got %d calls", len(f.calls))
	}
}

func TestClusterIPResultParsing(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("RESULT=UPDATED set to 192.168.1.230 on Cluster Network 1; core group is Online")}}
	out, note, err := newTestPS(f).EnsureClusterIP(context.Background(), "192.168.1.230")
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeUpdated {
		t.Fatalf("a re-address must report Updated, got %v", out)
	}
	if !strings.Contains(note, "192.168.1.230") || !strings.Contains(note, "core group is Online") {
		t.Fatalf("the note must say what happened, got %q", note)
	}
}

// A script that ends without a marker did something unknown. Claiming success
// would report a re-address that may not have happened.
func TestAnUnmarkedResultIsNotSuccess(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("some noise")}}
	_, _, err := newTestPS(f).EnsureClusterIP(context.Background(), "192.168.1.230")
	if err == nil {
		t.Fatal("an unmarked result must not be reported as a successful re-address")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("the error must say the outcome is unknown, got %v", err)
	}
}
