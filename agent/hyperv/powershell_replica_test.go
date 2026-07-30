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
		"$_ -split '\\.'", // target = the endpoint that is not this host
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

// TestEnsureReplicaBrokerChecksTheResourceNotTheGroup guards the fix for a
// broker that reported healthy while it was Failed. The role's group can be
// Online while the Virtual Machine Replication Broker resource under it is
// Failed, and it is the RESOURCE that EnsureVMReplication waits on — so
// checking only the group made this role report a green condition on every
// pass while every clustered VM sat blocked on "waiting for the Hyper-V
// Replica Broker to come online (currently Failed)", with nothing acting to
// clear it.
func TestEnsureReplicaBrokerChecksTheResourceNotTheGroup(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("RESULT=NOOP")}}
	ps := newTestPS(f)
	if _, err := ps.EnsureReplicaBroker(context.Background(), types.ReplicaBrokerSpec{
		Name: "ReplBroker", StaticIP: "192.168.1.210",
	}); err != nil {
		t.Fatal(err)
	}
	s := f.calls[0]
	for _, want := range []string{
		// The group and the resource are re-read AFTER the create/start work;
		// the objects captured before it are snapshots, and a just-added
		// resource is Offline. The re-read fetches the object (rather than
		// naming it) because the start calls below need something to pipe.
		"$grp = @(Get-ClusterGroup -ErrorAction SilentlyContinue | Where-Object { [string]$_.Name -eq $name })[0]",
		"$grp | Start-ClusterGroup",
		// Starting the group is not enough: a resource that has exhausted its
		// restart threshold stays Failed until it is started itself.
		"$res | Start-ClusterResource",
		// The broker's own state decides the outcome.
		"if ([string]$res.State -ne 'Online')",
		// A broker that will not come online names the resource holding it back
		// rather than leaving the operator with the VM-side wait alone.
		"not online in this group",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("replica broker script missing %q\n---\n%s", want, s)
		}
	}
	// The old script decided everything from $grp.State alone and could not see
	// a Failed broker under an Online group.
	if strings.Contains(s, "if ($grp.State -ne 'Online')") {
		t.Fatal("broker health must not be inferred from the group state alone")
	}
	assertNoClusterNameBinding(t, s)
}

// The Failover Clustering cmdlets type -Name as a StringCollection, so binding a
// plain string to it fails at RUNTIME with "Cannot convert '<name>' to the type
// ...". Nothing catches that at build time and the script reads perfectly well —
// the first Remove-ClusterGroup shipped this way and died on a real cluster.
// Passing the object binds -InputObject, which needs no conversion.
func assertNoClusterNameBinding(t *testing.T, script string) {
	t.Helper()
	for _, bad := range []string{
		"Start-ClusterGroup -Name",
		"Stop-ClusterGroup -Name",
		"Remove-ClusterGroup -Name",
		"Start-ClusterResource -Name",
		"Remove-ClusterResource -Name",
	} {
		if strings.Contains(script, bad) {
			t.Errorf("%q binds a string to a StringCollection parameter; pipe the object instead", bad)
		}
	}
}

// TestRemoveReplicaBrokerScript guards the cleanup path. Removing the broker
// resource on its own would strand the client access point holding the broker's
// name and IP, so the whole group goes; and a removal that quietly left the
// broker behind must fail rather than report success and send the operator to
// recreate onto a conflict.
func TestRemoveReplicaBrokerScript(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("RESULT=REMOVED bcluster2-Broker")}}
	ps := newTestPS(f)
	msg, err := ps.RemoveReplicaBroker(context.Background(), "bcluster2-Broker")
	if err != nil {
		t.Fatal(err)
	}
	if msg != "REMOVED bcluster2-Broker" {
		t.Fatalf("want the RESULT= marker stripped, got %q", msg)
	}
	s := f.calls[0]
	for _, want := range []string{
		// Found by TYPE, so a renamed group or a broker left behind by a cleared
		// spec is still removed.
		"$_.ResourceType -eq 'Virtual Machine Replication Broker'",
		// The group goes, not just the resource — that is what clears the CAP.
		"$grpObj | Remove-ClusterGroup -RemoveResources -Force",
		// -RemoveResources will not take a group whose resources are still online.
		"$grpObj | Stop-ClusterGroup",
		// A single stuck resource must not block the whole removal.
		"$r | Remove-ClusterResource -Force",
		// The group is fetched as an object, never bound by name.
		"$grpObj = @(Get-ClusterGroup",
		// Verified, not assumed.
		"the broker is still present after removal",
		// The operator needs the CAP name to clear its AD object.
		"clear its AD computer object before reusing the name",
		// The named group is removed even with no broker resource in it: a broker
		// resource deleted on its own strands the client access point, which still
		// holds the name and IP and then blocks recreating under that name.
		"$want = 'bcluster2-Broker'",
		"$groups += $want",
		// Both the resource AND the group are verified gone.
		"the broker group is still present after removal",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("remove broker script missing %q\n---\n%s", want, s)
		}
	}
	assertNoClusterNameBinding(t, s)
}

// An empty cluster is a no-op, not an error: the job is also the way to clean up
// after a spec that no longer declares a broker, and running it twice must be safe.
func TestRemoveReplicaBrokerNoBrokerIsNoop(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("RESULT=NOOP no Hyper-V Replica Broker in this cluster")}}
	msg, err := newTestPS(f).RemoveReplicaBroker(context.Background(), "")
	if err != nil {
		t.Fatalf("an absent broker must not be an error: %v", err)
	}
	if !strings.Contains(msg, "no Hyper-V Replica Broker") {
		t.Fatalf("want the no-op reported, got %q", msg)
	}
}

// The client access point's address is desired state like anything else.
// -StaticAddress only applies when Add-ClusterServerRole CREATES the role, so on
// an existing broker a changed StaticIP was ignored and the pass still reported
// converged — desired state naming one address while the broker answered on
// another.
func TestEnsureReplicaBrokerReconcilesAChangedStaticIP(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("RESULT=NOOP")}}
	if _, err := newTestPS(f).EnsureReplicaBroker(context.Background(), types.ReplicaBrokerSpec{
		Name: "ReplBroker", StaticIP: "192.168.1.211",
	}); err != nil {
		t.Fatal(err)
	}
	s := f.calls[0]
	for _, want := range []string{
		`$wantIP = '192.168.1.211'`,
		// Compared against what the resource actually holds, not assumed from
		// whatever created it.
		"Get-ClusterParameter -Name Address",
		"$curIP -ne $wantIP",
		// The address only takes while the resource is offline.
		"$ipr | Stop-ClusterResource",
		"Set-ClusterParameter -Name Address -Value $wantIP",
		// IPv4 only: the CAP's IPv6 address is the cluster's own business.
		"$_.ResourceType -eq 'IP Address'",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("broker script missing %q\n---\n%s", want, s)
		}
	}
	assertNoClusterNameBinding(t, s)
}

// No declared address means DHCP — the CAP's address is then not ours to touch.
func TestEnsureReplicaBrokerLeavesAddressAloneWhenUndeclared(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("RESULT=NOOP")}}
	if _, err := newTestPS(f).EnsureReplicaBroker(context.Background(), types.ReplicaBrokerSpec{
		Name: "ReplBroker",
	}); err != nil {
		t.Fatal(err)
	}
	if s := f.calls[0]; !strings.Contains(s, `$wantIP = ''`) {
		t.Fatalf("an undeclared address must leave the CAP alone\n---\n%s", s)
	}
}

// The replica-server step waits for the broker to EXIST, never for it to be
// Online. Waiting for Online deadlocked the two against each other: the broker
// resource cannot come online until the members accept replica traffic, and this
// step refused to configure them until the broker was online. Observed live —
// three cluster members stuck on "waiting for the Hyper-V Replica Broker to come
// online (currently Failed)" while the broker sat Failed for want of exactly the
// configuration being withheld, and the standalone host in the same fabric
// configured itself without trouble.
func TestEnsureReplicaServerDoesNotWaitForTheBrokerToBeOnline(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("RESULT=NOOP")}}
	if _, err := newTestPS(f).EnsureReplicaServer(context.Background(), types.ReplicaServerSpec{
		Enabled: true, AuthenticationType: "Kerberos", DefaultStorageLocation: `C:\ClusterStorage\Vol01\Replica`,
	}); err != nil {
		t.Fatal(err)
	}
	s := f.calls[0]
	// An absent broker is still a legitimate wait: without one the query itself
	// fails with a misleading ObjectNotFound.
	if !strings.Contains(s, "broker to be provisioned") {
		t.Fatalf("an absent broker should still be waited on explicitly\n---\n%s", s)
	}
	// But its STATE must not gate the configuration. Asserted against the code
	// rather than the message, so the comment explaining why can keep saying it.
	if strings.Contains(s, "$broker.State -ne 'Online'") {
		t.Error("gating on the broker being Online deadlocks it against the configuration it needs")
	}
}

// The same guard in EnsureVMReplication is NOT a deadlock and must stay: a VM
// genuinely cannot replicate through a broker that is down, and enabling a VM's
// replication does nothing to bring the broker up. Only the host-level
// replica-server configuration is part of the cycle.
func TestEnsureVMReplicationStillWaitsForAnOnlineBroker(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("RESULT=NOOP")}}
	if _, err := newTestPS(f).EnsureVMReplication(context.Background(), "Website", types.VMReplicationSpec{
		Enabled: true, ReplicaServer: "hv04.example.test",
	}); err != nil {
		t.Fatal(err)
	}
	if s := f.calls[0]; !strings.Contains(s, "$broker.State -ne 'Online'") {
		t.Fatalf("a VM must not be told to replicate through a broker that is down\n---\n%s", s)
	}
}

// A broker that will not come online must (a) try the one targeted remediation
// for the cause seen live, and (b) report what Windows already knows.
//
// The broker binds a network listener to its own client access point name, so a
// node that cannot resolve that name fails with 0x80072AF9 "No such host is
// known" — with the Network Name resource sitting Online, because a name can come
// online without its DNS record landing where the node looks. This rig sat Failed
// for hours while that exact sentence was in the VMMS log and the console said
// only "is Failed".
func TestEnsureReplicaBrokerSelfHealsAndReportsTheReason(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("RESULT=NOOP")}}
	if _, err := newTestPS(f).EnsureReplicaBroker(context.Background(), types.ReplicaBrokerSpec{
		Name: "ReplBroker", StaticIP: "192.168.1.235",
	}); err != nil {
		t.Fatal(err)
	}
	s := f.calls[0]
	for _, want := range []string{
		// Self-heal: force the CAP to re-register in DNS, then retry.
		"Update-ClusterNetworkNameResource",
		// Only the group's own Network Name, and only while it is failing.
		"$_.ResourceType -eq 'Network Name'",
		// Then report the reason Windows recorded, not just the state.
		"Microsoft-Windows-Hyper-V-VMMS-Admin",
		"Microsoft-Windows-FailoverClustering/Operational",
		"Windows reports: ",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("broker self-heal/diagnosis missing %q\n---\n%s", want, s)
		}
	}
	// The remediation must not run on a healthy broker — it sits behind the
	// not-Online check, so a converged pass stays a pure no-op read.
	// Matched on the piped invocation, not the bare cmdlet name: the comment
	// above it explains the remediation and would otherwise match first.
	heal := strings.Index(s, "$nn | Update-ClusterNetworkNameResource")
	gate := strings.Index(s, "if ([string]$res.State -ne 'Online') {")
	if gate < 0 || heal < gate {
		t.Error("the DNS re-registration must be gated behind the broker actually failing")
	}
}
