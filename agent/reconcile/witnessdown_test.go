package reconcile

import (
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
)

/*
A failed witness is not an outage, and Ballast said it was.

	Secondary on ballasttest, 2026-09-11. The witness share server was switched
	off and the core group went Failed, so the condition announced that "the
	cluster name and its IP addresses are not fully online … shared volumes will
	not come online and roles will fail" — and then, in the same message, named
	the File Share Witness as the resource holding it down. Both halves at once,
	contradicting each other.

	The operator's own words: "it's only the share that is failed, problem looks
	bigger than it is". Nothing was down. The cluster kept its identity, its CSV
	stayed online and both nodes kept running; what it lost was a quorum VOTE.

	An alarm that overstates is one people learn to discount, which costs the
	alarm that is real.
*/

func coreGroupFailed() []hyperv.ClusterGroup {
	return []hyperv.ClusterGroup{{Name: "Cluster Group", State: "Failed"}}
}

func TestAWitnessDownIsNotAnOutage(t *testing.T) {
	got := coreGroupProblem(coreGroupFailed(), []hyperv.ClusterCoreResource{
		{Name: "Cluster Name", Type: "Network Name", State: "Online"},
		{Name: "Cluster IP Address", Type: "IP Address", State: "Online", Address: "10.0.65.10"},
		{Name: "File Share Witness", Type: "File Share Witness", State: "Failed"},
	}, 3)
	if got == nil {
		t.Fatal("a failed core group must still be reported")
	}
	// It must NOT claim the things that are demonstrably online are down.
	for _, forbidden := range []string{
		"shared volumes will not come online",
		"roles will fail",
		"has no identity on the network",
	} {
		if strings.Contains(got.Message, forbidden) {
			t.Errorf("the message still claims an outage the witness did not cause: %q", forbidden)
		}
	}
	// And it must say what was actually lost, which is the vote.
	for _, want := range []string{"witness is down", "quorum", "is not an outage"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("missing %q in: %s", want, got.Message)
		}
	}
	// The resource is still named, as it was before.
	if !strings.Contains(got.Message, "File Share Witness") {
		t.Errorf("the failing resource is no longer named: %s", got.Message)
	}
}

/*
The identity case keeps every word of its warning.

	This is the one that earns "fix this first": with the cluster name or an IP
	down the cluster has no identity, and every CSV and role then fails on its
	own terms, looking like three unrelated faults.
*/
func TestTheNameBeingDownStillReadsAsAnOutage(t *testing.T) {
	got := coreGroupProblem(coreGroupFailed(), []hyperv.ClusterCoreResource{
		{Name: "Cluster Name", Type: "Network Name", State: "Failed"},
		{Name: "File Share Witness", Type: "File Share Witness", State: "Failed"},
	}, 3)
	for _, want := range []string{"shared volumes will not come online", "downstream", "Fix this first"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("missing %q in: %s", want, got.Message)
		}
	}
}

// An IP address down is the same class of fault as the name.
func TestAnIPAddressDownReadsAsAnOutage(t *testing.T) {
	got := coreGroupProblem(coreGroupFailed(), []hyperv.ClusterCoreResource{
		{Name: "Cluster IP Address", Type: "IP Address", State: "Failed", Address: "10.0.65.10"},
	}, 3)
	if !strings.Contains(got.Message, "shared volumes will not come online") {
		t.Errorf("an IP being down no longer reads as an outage: %s", got.Message)
	}
}

// Classification is by Windows' own type names, with the resource name as a
// fallback for a witness that reports a type this does not recognise.
func TestAWitnessIsRecognisedByTypeOrName(t *testing.T) {
	byType := hyperv.ClusterCoreResource{Name: "Quorum thing", Type: "File Share Witness", State: "Failed"}
	byName := hyperv.ClusterCoreResource{Name: "File Share Witness", Type: "", State: "Failed"}
	for _, r := range []hyperv.ClusterCoreResource{byType, byName} {
		if k := coreResourceKind(r); k != "witness" {
			t.Errorf("%+v classified as %q, want witness", r, k)
		}
	}
	if k := coreResourceKind(hyperv.ClusterCoreResource{Type: "Network Name"}); k != "identity" {
		t.Errorf("the cluster name classified as %q, want identity", k)
	}
}

/* What a lost witness COSTS depends on the node count, and the first version of
   this message did not ask.

   "Worth fixing and is not an outage" is true of three nodes: they hold a
   majority between themselves and the witness is a margin. On two it is the
   opposite — the witness is what makes a majority possible at all, so with it
   down losing EITHER node ends the cluster. Secondary on the rig is two nodes
   and was shown the three-node sentence, which invites an operator to leave it
   alone. */
func TestATwoNodeClusterIsToldTheWitnessIsTheMargin(t *testing.T) {
	res := []hyperv.ClusterCoreResource{
		{Name: "Cluster Name", Type: "Network Name", State: "Online"},
		{Name: "File Share Witness", Type: "File Share Witness", State: "Failed"},
	}
	two := coreGroupMessage("Failed", res, 2)
	if !strings.Contains(two, "losing either node ends the cluster") {
		t.Fatalf("a two-node cluster must be told it has no margin left:\n%s", two)
	}
	if strings.Contains(two, "is not an outage") {
		t.Fatalf("'not an outage' understates a two-node cluster with no witness:\n%s", two)
	}

	three := coreGroupMessage("Failed", res, 3)
	if !strings.Contains(three, "is not an outage") {
		t.Fatalf("a three-node cluster still holds a majority and should say so:\n%s", three)
	}
	if strings.Contains(three, "ends the cluster") {
		t.Fatalf("a three-node cluster does not end when the witness goes:\n%s", three)
	}

	// Both still have to say the identity is intact — that was the point of
	// splitting this message out in the first place.
	for _, m := range []string{two, three} {
		if !strings.Contains(m, "shared volumes stay online") {
			t.Fatalf("the message must still say the cluster keeps its identity:\n%s", m)
		}
	}
}

// An unknown node count must not invent a number, and must still say something
// useful rather than nothing.
func TestAnUnknownNodeCountFallsBackToTheGeneralWording(t *testing.T) {
	m := coreGroupMessage("Failed", []hyperv.ClusterCoreResource{
		{Name: "File Share Witness", Type: "File Share Witness", State: "Failed"},
	}, 0)
	if strings.Contains(m, " 0 nodes") {
		t.Fatalf("an unknown node count must not be printed as zero:\n%s", m)
	}
	if !strings.Contains(m, "quorum VOTE") {
		t.Fatalf("it should still say what is lost:\n%s", m)
	}
}
