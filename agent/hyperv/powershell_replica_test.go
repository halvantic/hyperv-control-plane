package hyperv

import (
	"context"
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

// TestEnsureVMReplicationScriptHandlesStuckStates guards the fix for the
// green-but-absent replica bug: the script must repair wedged relationship
// states (start initial replication, resume, resynchronise) and fail the
// condition on Critical health — never no-op green over a relationship that
// is not actually replicating.
func TestEnsureVMReplicationScriptHandlesStuckStates(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("RESULT=NOOP")}}
	ps := newTestPS(f)
	out, err := ps.EnsureVMReplication(context.Background(), "Website", types.VMReplicationSpec{
		Enabled:       true,
		ReplicaServer: "hv04.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != OutcomeUnchanged {
		t.Fatalf("noop run: want Unchanged, got %v", out)
	}
	if len(f.calls) != 1 {
		t.Fatalf("want 1 script, got %d", len(f.calls))
	}
	s := f.calls[0]
	for _, want := range []string{
		"Start-VMInitialReplication",
		"'Suspended'",
		"Resume-VMReplication -VMName $vm -ErrorAction Stop",
		"Resume-VMReplication -VMName $vm -Resynchronize",
		"'WaitingForStartResynchronize'",
		"'Error'",
		"$health -eq 'Critical'",
		"replication is configured but unhealthy",
		// The replica side is a no-op: Set-VMReplication/Resume there fail
		// "Replication is not enabled" during a failover role swap.
		"$r.Mode -eq 'Replica'",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("script missing %q\n---\n%s", want, s)
		}
	}
}

// TestReverseReplicationProbesTargetOnFailure guards that a failed reverse does
// not surface Hyper-V's bare "Could not reverse replication" — the script must
// identify the reverse target (the endpoint that is not this host), probe its
// reachability and Replica server role, and throw an actionable reason.
func TestReverseReplicationProbesTargetOnFailure(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("RESULT=OK")}}
	if _, err := newTestPS(f).ReverseReplication(context.Background(), "Website"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("want 1 script, got %d", len(f.calls))
	}
	s := f.calls[0]
	for _, want := range []string{
		"Set-VMReplication -VMName 'Website' -Reverse",
		"} catch {",
		"$r.PrimaryServer",
		"$r.ReplicaServer",
		"$_ -split '\\.'",  // target = the endpoint that is not this host
		"Test-NetConnection",
		"Get-VMReplicationServer -ComputerName $target",
		"not reachable on the replica port",
		"Replica server role is not enabled",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("reverse script missing %q\n---\n%s", want, s)
		}
	}
}

// TestStopTestFailoverRemovesOrphanedClone guards that tearing down a test
// failover does not rely solely on Stop-VMFailover (a no-op when the relationship
// no longer tracks the clone) — it must remove the "<vm> - Test" VM directly if it
// is still present, so the clone never lingers on the destination host.
func TestStopTestFailoverRemovesOrphanedClone(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("RESULT=OK")}}
	if err := newTestPS(f).StopTestFailover(context.Background(), "Website"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("want 1 script, got %d", len(f.calls))
	}
	s := f.calls[0]
	for _, want := range []string{
		"$testName = 'Website' + ' - Test'",
		"Stop-VMFailover -VMName 'Website'",
		"Get-VM -Name $testName",
		"Remove-VM -Name $testName -Force",
		"still exists after Remove-VM",
		// The trailing marker keeps a swallowed "no test failover" error from
		// making powershell.exe exit 1 with no stderr.
		"'RESULT=OK'",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("stop-test script missing %q\n---\n%s", want, s)
		}
	}
}

// TestResultOutcomeRejectsTruncatedOutput guards the marker protocol: a script
// that exits 0 without reaching its RESULT= line (the silent Select-Object
// -First pipeline-stop failure) must surface as an error, never as a green
// no-op.
func TestResultOutcomeRejectsTruncatedOutput(t *testing.T) {
	if out, err := resultOutcome([]byte("RESULT=UPDATED\n"), "op"); err != nil || out != OutcomeUpdated {
		t.Fatalf("updated: got %v, %v", out, err)
	}
	if out, err := resultOutcome([]byte("some output\nRESULT=NOOP"), "op"); err != nil || out != OutcomeUnchanged {
		t.Fatalf("noop: got %v, %v", out, err)
	}
	if _, err := resultOutcome([]byte("partial output, script died here"), "ensure vm replication Website"); err == nil {
		t.Fatal("truncated output must be an error, not a silent no-op")
	}
	if _, err := resultOutcome(nil, "op"); err == nil {
		t.Fatal("empty output must be an error")
	}
}
