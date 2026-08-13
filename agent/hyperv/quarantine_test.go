package hyperv

import (
	"strings"
	"testing"
)

/* Clearing a quarantine refused the only case it exists for.

   Failover Clustering reports a QUARANTINED node as State=Down and puts the word
   in StatusInformation. The guard read State alone, so on bcluster2 2026-08-13 —
   with Failover Cluster Manager showing HVNEW01 quarantined — Ballast answered:

     clear quarantine on "HVNEW01": HVNEW01 is Down, not quarantined. Clearing a
     quarantine is only meaningful for a node the cluster has ejected; a node
     that is Down needs whatever put it there looked at instead.

   Refusing the remedy is bad. Refusing it while flatly contradicting the cluster
   is worse: the sentence reads as a definite answer, so an operator stops
   looking for a quarantine and goes hunting for a dead machine that is running
   perfectly.

   The observation path was fixed first (StatusInformation carried through the
   proto and resolved in the console) and this one was missed — the same defect,
   in the place where it blocks the fix rather than merely hiding the cause. */

func TestClearingAQuarantineLooksAtStatusInformationNotJustState(t *testing.T) {
	s := quarantineScript
	if !strings.Contains(s, "StatusInformation") {
		t.Fatal("a quarantined node reports State=Down; without StatusInformation the guard cannot see a quarantine at all")
	}
	// The decision must not rest on State alone.
	if !strings.Contains(s, "$info -like 'Quarantine*'") {
		t.Error("quarantine must be recognised from StatusInformation, matched loosely — Windows has spelled it more than one way")
	}
}

// A refusal must show what it actually saw. The old message asserted a state and
// a conclusion with nothing to check it against, which is why it was believed.
func TestARefusalReportsBothFieldsItJudgedOn(t *testing.T) {
	s := quarantineScript
	for _, want := range []string{"'State=' + $state", "StatusInformation=' + $info"} {
		if !strings.Contains(s, want) {
			t.Errorf("the refusal must report %s so a wrong verdict can be seen to be wrong", want)
		}
	}
}

// Readmission restarts the cluster service and the node rejoins over the cluster
// networks. Judging that on the very next line failed the job for a rejoin that
// was working.
func TestAReadmittedNodeIsGivenTimeToRejoin(t *testing.T) {
	s := quarantineScript
	if !strings.Contains(s, "AddSeconds(90)") || !strings.Contains(s, "Start-Sleep") {
		t.Error("the node must be given time to come back before the job calls it a failure")
	}
	// And it still must not claim success for a node that never arrived.
	if !strings.Contains(s, "not Up") {
		t.Error("a node still outside the cluster must not be reported as readmitted")
	}
}

// The refusal itself must survive: a node that is genuinely Down for its own
// reasons is not fixed by Start-ClusterNode, and papering over that is how the
// real reason goes unlooked-at.
func TestANodeThatIsGenuinelyDownIsStillRefused(t *testing.T) {
	s := quarantineScript
	if !strings.Contains(s, "is not quarantined") {
		t.Fatal("a node in some other state must still be refused rather than started")
	}
	if !strings.Contains(s, "cannot answer for the cluster") {
		t.Error("running it on the quarantined node itself must be explained, not just fail")
	}
}
