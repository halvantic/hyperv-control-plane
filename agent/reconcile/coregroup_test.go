package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

/* DRCluster reported phase Ready for hours while its Cluster Group sat
   PartialOnline — Cluster Name and both Cluster IP Address resources offline.
   Everything downstream then failed on its own terms: the CSVs would not come
   online, the VM configuration resources went Failed, and each was diagnosed
   separately because the console said the cluster itself was fine.

   The state was observed the whole time and thrown away. */

func TestAFullyOnlineCoreGroupIsNoProblem(t *testing.T) {
	got := coreGroupProblem([]hyperv.ClusterGroup{
		{Name: "Cluster Group", State: "Online"},
		{Name: "Available Storage", State: "Offline"},
	})
	if got != nil {
		t.Fatalf("an online core group must raise nothing, got %+v", got)
	}
}

// PartialOnline reads like a half-success and is not one: the cluster name being
// offline is total, whatever else in the group is up.
func TestPartialOnlineIsAProblem(t *testing.T) {
	got := coreGroupProblem([]hyperv.ClusterGroup{{Name: "Cluster Group", State: "PartialOnline"}})
	if got == nil {
		t.Fatal("PartialOnline must not read as healthy")
	}
	if got.Status {
		t.Error("the condition must be unmet")
	}
	if !strings.Contains(got.Message, "PartialOnline") {
		t.Errorf("the message must name the state observed: %q", got.Message)
	}
}

// The point of naming it first: an operator who starts at the CSV works backwards
// through three unrelated-looking faults.
func TestTheMessageSaysTheRestIsDownstream(t *testing.T) {
	got := coreGroupProblem([]hyperv.ClusterGroup{{Name: "Cluster Group", State: "Offline"}})
	if got == nil {
		t.Fatal("an offline core group must be reported")
	}
	for _, want := range []string{"shared volumes will not come online", "downstream"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("the message must explain the consequence (%q): %q", want, got.Message)
		}
	}
}

// Absent is not online. A pass that could not read the core group must not report
// the cluster as healthy — the same absent-versus-unknown rule as everywhere else.
func TestAnUnreportedCoreGroupIsNotTreatedAsOnline(t *testing.T) {
	got := coreGroupProblem([]hyperv.ClusterGroup{{Name: "Available Storage", State: "Online"}})
	if got == nil {
		t.Fatal("a cluster that reported no core group must not read as healthy")
	}
	if got.Reason != "NotReported" {
		t.Errorf("reason %q: an unread group is not the same as one that is offline", got.Reason)
	}
}

func TestAnEmptyGroupListIsNotOnline(t *testing.T) {
	if coreGroupProblem(nil) == nil {
		t.Fatal("no groups at all must not read as healthy")
	}
}

// The group is matched case-insensitively: it is Windows' own name for it, and a
// case difference must not silently mean "not reported".
func TestTheCoreGroupIsMatchedCaseInsensitively(t *testing.T) {
	got := coreGroupProblem([]hyperv.ClusterGroup{{Name: "cluster group", State: "Online"}})
	if got != nil {
		t.Fatalf("case must not decide whether the core group was found: %+v", got)
	}
}

// End to end: the phase, not just the condition. Reporting the problem while
// still calling the cluster Ready is what happened for hours.
func TestAClusterWithAPartialCoreGroupIsNotReady(t *testing.T) {
	stub := &hyperv.Stub{
		ClusterExists: true, ClusteringInstalled: true, ClusterFirewallOpen: true,
		ClusterName: "bcluster", ClusterMembers: []string{"HV01", "HV02", "HV03"},
		ClusterGroups: []hyperv.ClusterGroup{{Name: "Cluster Group", State: "PartialOnline"}},
	}
	r := testReconciler(stub)

	res, _ := r.ReconcileCluster(context.Background(), clusterAssignment(true), nil)

	if res.Phase == types.PhaseReady {
		t.Fatal("a cluster whose name and IP addresses are offline must not report Ready")
	}
	if res.Honoured {
		t.Error("it must not report its desired state as honoured either")
	}
	var found bool
	for _, c := range res.Conditions {
		if c.Type == "ClusterCoreGroup" {
			found = true
		}
	}
	if !found {
		t.Error("the reason must be reported, not just the phase")
	}
}

// The recovery has to be reachable from the centre. Needing Failover Cluster
// Manager to start a resource on a host Ballast runs an agent on is a defect by
// CLAUDE.md's own rule, not a runbook step.
func TestBringingTheCoreGroupOnlineIsAJob(t *testing.T) {
	stub := &hyperv.Stub{
		ClusterExists: true, ClusteringInstalled: true, ClusterFirewallOpen: true,
		ClusterName: "bcluster", ClusterMembers: []string{"HV01", "HV02", "HV03"},
		ClusterGroups: []hyperv.ClusterGroup{{Name: "Cluster Group", State: "PartialOnline"}},
	}
	r := testReconciler(stub)

	if _, err := r.ExecuteJob(context.Background(), types.Job{Kind: types.JobClusterStartCoreGroup}, nil); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if !stub.CoreGroupStarted {
		t.Fatal("the job did not reach the host")
	}

	// And the cluster must stop reporting the problem afterwards — asserting the
	// call was made would pass just as well against a job that did nothing.
	res, _ := r.ReconcileCluster(context.Background(), clusterAssignment(true), nil)
	if res.Phase != types.PhaseReady {
		t.Fatalf("the cluster is still %s after the core group came online", res.Phase)
	}
}

// A cluster that refuses must not report the recovery as done.
func TestARefusedStartIsReportedAsAFailure(t *testing.T) {
	stub := &hyperv.Stub{ClusterExists: true, FailCoreGroupStart: true}
	r := testReconciler(stub)

	if _, err := r.ExecuteJob(context.Background(), types.Job{Kind: types.JobClusterStartCoreGroup}, nil); err == nil {
		t.Fatal("a cluster that would not start its core group must surface as a failed job")
	}
}

// Running it against a healthy cluster is harmless, so the console can offer it
// without first having to be right about whether anything is wrong.
func TestStartingAnAlreadyOnlineCoreGroupIsANoOp(t *testing.T) {
	stub := &hyperv.Stub{
		ClusterExists: true,
		ClusterGroups: []hyperv.ClusterGroup{{Name: "Cluster Group", State: "Online"}},
	}
	r := testReconciler(stub)

	msg, err := r.ExecuteJob(context.Background(), types.Job{Kind: types.JobClusterStartCoreGroup}, nil)
	if err != nil {
		t.Fatalf("a healthy cluster must not error: %v", err)
	}
	if !strings.Contains(msg, "already online") {
		t.Errorf("it should say nothing needed doing, got %q", msg)
	}
}

/* Quarantine works by STOPPING the cluster service on the offending node, so the
   node cannot act for the cluster at all. bcluster2's designated former was the
   quarantined node, so every pass asked a stopped service what the cluster looked
   like and deferred — which is how one node's quarantine blinded Ballast to a
   whole cluster. The recovery therefore runs from a peer. */

func TestClearingAQuarantineIsAJob(t *testing.T) {
	stub := &hyperv.Stub{ClusterExists: true}
	r := testReconciler(stub)

	msg, err := r.ExecuteJob(context.Background(),
		types.Job{Kind: types.JobClusterClearQuarantine, Params: map[string]string{"node": "HVNEW02"}}, nil)
	if err != nil {
		t.Fatalf("run job: %v", err)
	}
	if stub.QuarantineCleared != "HVNEW02" {
		t.Fatalf("the job named node %q", stub.QuarantineCleared)
	}
	if !strings.Contains(msg, "HVNEW02") {
		t.Errorf("the outcome must name the node: %q", msg)
	}
}

// A cluster that will not readmit the node must not report the recovery as done —
// the node is still outside, and the reason it kept leaving has not gone away.
func TestARefusedReadmissionIsAFailure(t *testing.T) {
	stub := &hyperv.Stub{ClusterExists: true, FailClearQuarantine: true}
	r := testReconciler(stub)

	if _, err := r.ExecuteJob(context.Background(),
		types.Job{Kind: types.JobClusterClearQuarantine, Params: map[string]string{"node": "HVNEW02"}}, nil); err == nil {
		t.Fatal("a cluster refusing to readmit the node must surface as a failed job")
	}
}

// The script must refuse a node that is not actually quarantined: Start-ClusterNode
// on a node that is Down for a real reason papers over the reason, and the two
// states mean different things.
func TestTheScriptRefusesANodeThatIsNotQuarantined(t *testing.T) {
	if !strings.Contains(hyperv.QuarantineScriptForTest(), "not quarantined") {
		t.Error("a node in some other state must be refused rather than started")
	}
	if !strings.Contains(hyperv.QuarantineScriptForTest(), "cannot answer for the cluster") {
		t.Error("running it on the quarantined node itself must be explained, not just fail")
	}
}
