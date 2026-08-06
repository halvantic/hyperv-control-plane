package hyperv

import (
	"strings"
	"testing"
)

/* Observing management vNICs one at a time cost two PowerShell invocations each
   — queryVNIC then queryVNICIP — before the reconciler could decide nothing had
   changed. Each paid a fresh module load, and the IP query alone touches
   Hyper-V, NetTCPIP, NetRoute, DnsClient and FailoverClusters. Measured on the
   rig: ~7s per vNIC, so a three-vNIC converged host spent ~21s of a 40s host
   reconcile confirming it had nothing to do.

   Cold-versus-warm on that same rig, in one process: Get-ClusterNode 1976ms then
   31ms, Get-NetIPAddress 654ms then 24ms. The saving is real but it only exists
   if the work stays in ONE invocation, which is what these pin. */

func TestVNICBatchObservesEveryNameInOneScript(t *testing.T) {
	s := vnicsBatchScript([]string{"Management", "LiveMigration", "Storage"})

	for _, n := range []string{"Management", "LiveMigration", "Storage"} {
		if !strings.Contains(s, "'"+n+"'") {
			t.Fatalf("every vNIC must be observed by the one script; %q missing", n)
		}
	}
	// One loop over the names, not three pasted blocks — the point is a single
	// invocation, and a per-name unrolled script would grow without bound.
	//
	// The adapters are now enumerated ONCE ABOVE the loop and matched by name
	// inside it, rather than queried per name. That is both cheaper (one call for
	// the whole set instead of one each) and the only way to tell "this vNIC is
	// absent" from "Hyper-V did not answer": an enumeration returns an empty list
	// for the former and throws for the latter, whereas -Name errors for both.
	if n := strings.Count(s, "Get-VMNetworkAdapter -ManagementOS"); n != 1 {
		t.Fatalf("the adapter query must appear exactly once, found %d", n)
	}
	if strings.Index(s, "Get-VMNetworkAdapter -ManagementOS") > strings.Index(s, "foreach ($n in $names)") {
		t.Fatal("the adapter enumeration must be hoisted above the per-vNIC loop")
	}
	if !strings.Contains(s, "known = $false") {
		t.Fatal("a Hyper-V that does not answer must be reported as unknown, never as an absent vNIC")
	}
	if strings.Count(s, "Get-NetIPAddress") != 1 {
		t.Fatal("the IP query must appear once, inside the loop over names")
	}
}

// The cluster IP-resource enumeration is a property of the CLUSTER, not of a
// vNIC, and the per-vNIC version re-ran it for every one. Hoisting it is a
// large part of the saving: Get-ClusterResource was 1976ms cold on the rig.
func TestVNICBatchReadsClusterIPsOnce(t *testing.T) {
	s := vnicsBatchScript([]string{"Management", "LiveMigration", "Storage"})

	if n := strings.Count(s, "Get-ClusterResource"); n != 1 {
		t.Fatalf("cluster IP resources are per-cluster and must be read once, found %d", n)
	}
	// And it must be read BEFORE the loop, or hoisting it achieved nothing.
	if strings.Index(s, "Get-ClusterResource") > strings.Index(s, "foreach ($n in $names)") {
		t.Fatal("the cluster query must be hoisted above the per-vNIC loop")
	}
}

// Cluster-owned IPs are excluded by ADDRESS. PrefixOrigin is not a safe guard —
// it is build-dependent — and mistaking a cluster IP for the declared one would
// have the reconciler take it off a live cluster interface.
func TestVNICBatchStillExcludesClusterOwnedAddresses(t *testing.T) {
	s := vnicsBatchScript([]string{"Management"})
	if !strings.Contains(s, "$clusterIps -notcontains $_.IPAddress") {
		t.Fatal("cluster-owned IPs must be excluded by address, not left to be reconciled away")
	}
	if !strings.Contains(s, "169.254.") {
		t.Fatal("APIPA addresses must still be ignored")
	}
}

// Names come from desired state, which an operator edits. They are quoted the
// same way every other script in this package quotes them.
func TestVNICBatchQuotesNames(t *testing.T) {
	s := vnicsBatchScript([]string{"It's Odd"})
	if !strings.Contains(s, psQuote("It's Odd")) {
		t.Fatalf("names must be quoted through psQuote, got: %s", s)
	}
}

// A host with no declared vNICs must not produce a script that queries the whole
// machine — an empty name list means nothing to observe.
func TestVNICBatchWithNoNamesIteratesNothing(t *testing.T) {
	s := vnicsBatchScript(nil)
	if !strings.Contains(s, "$names = @()") {
		t.Fatalf("an empty set must produce an empty name list, got: %s", s)
	}
}
