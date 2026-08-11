package reconcile

import (
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
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
