package hyperv

import (
	"strings"
	"testing"
)

// The observation must never claim "this node has never been in a cluster" on
// evidence that disappears when the cluster service stops.
//
// HKLM:\Cluster is the cluster database hive MOUNTED BY ClusSvc at startup. On a
// joined node whose service is stopped, crashed or still starting, the key is
// simply absent — and the reconciler reads exists=false/known=true as licence to
// run New-Cluster. On the rig (2026-08-06) ClusSvc sat in StartPending on two of
// three nodes and the designated former did exactly that against the live cluster
// it was already a member of.
//
// It failed only because New-Cluster does its own check. With the service down on
// every member nothing would have refused, and a second cluster forming over a
// live S2D pool loses data. So the guard must rest on evidence that survives the
// service being down.
func TestClusterStateScriptDoesNotTrustAServiceMountedHiveAlone(t *testing.T) {
	s := clusterStateScript

	if !strings.Contains(s, `Cluster\CLUSDB`) {
		t.Error("the on-disk cluster database is the evidence that survives ClusSvc being down; it must be checked")
	}
	if !strings.Contains(s, `Services\ClusSvc\Parameters`) {
		t.Error("ClusSvc\\Parameters\\ClusterName is written on join and persists; it must be checked")
	}
	if !strings.Contains(s, "HKLM:\\Cluster") {
		t.Error("the mounted hive is still a valid positive signal and should be kept as the cheap first check")
	}

	// Only one place may conclude "definitely not clustered", and it must sit
	// behind the combined evidence rather than beside it.
	const verdict = `[pscustomobject]@{ exists = $false; known = $true }`
	if n := strings.Count(s, verdict); n != 1 {
		t.Fatalf("exactly one path may report a certain not-clustered verdict, found %d", n)
	}
	joined := strings.Index(s, "if ($joined)")
	if joined == -1 {
		t.Fatal("the not-clustered verdict must be gated on the combined membership evidence")
	}
	if joined > strings.Index(s, verdict) {
		t.Error("the certain not-clustered verdict is reachable before the membership evidence is weighed")
	}
}
