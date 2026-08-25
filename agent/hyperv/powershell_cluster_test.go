package hyperv

import (
	"os"
	"path/filepath"
	"regexp"
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

// A group state names the group and not the fault. "SDDC Group is PartialOnline"
// says some of it came up and some did not, and which is the whole question —
// and that group is one Failover Cluster Manager does not display, so there is
// nowhere else for an operator to read the answer.
//
// Seen on S2DCluster 2026-08-16: the console reported PartialOnline with no
// detail and the operator went looking in a GUI that never shows the group. One
// Get-ClusterResource named it — of three resources only SDDC Management was
// Offline, a management resource in neither the storage nor the VM data path.
func TestClusterStateScriptCollectsGroupResources(t *testing.T) {
	s := clusterStateScript

	// Read once. Both the core-group diagnosis and the per-group detail need the
	// resources, and this runs on every pass on every member.
	if n := strings.Count(s, "Get-ClusterResource -ErrorAction SilentlyContinue"); n != 1 {
		t.Fatalf("cluster resources must be enumerated exactly once per pass, found %d calls", n)
	}
	if !strings.Contains(s, "$allRes = @(Get-ClusterResource") {
		t.Error("the single enumeration should be bound once and reused")
	}
	if !strings.Contains(s, "$resByGroup") {
		t.Error("resources must be indexed by owning group so each group can carry its own")
	}

	// Gated on state. A group resting Online or Offline has nothing to explain,
	// and Available Storage rests Offline holding every spare disk — collecting
	// there would send a healthy cluster's resting state over the wire each pass.
	if !strings.Contains(s, `$gstate -ne 'Online' -and $gstate -ne 'Offline'`) {
		t.Error("resources must be attached only to groups that are neither Online nor Offline")
	}
}

// The wire shape has to survive decoding, or the agent collects the answer and
// drops it before anything can report it.
func TestClusterObservationCarriesGroupResources(t *testing.T) {
	const payload = `{"exists":true,"known":true,"groups":[
	  {"name":"Cluster Group","owner":"n1","state":"Online","groupType":"Cluster"},
	  {"name":"SDDC Group","owner":"n2","state":"PartialOnline","groupType":"CoreSddc","resources":[
	    {"name":"Health","type":"Health Service","state":"Online"},
	    {"name":"SDDC Management","type":"SDDC Management","state":"Offline"}]}]}`

	var obs clusterObservation
	if err := decodeJSON([]byte(payload), &obs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(obs.Groups) != 2 {
		t.Fatalf("want 2 groups, got %d", len(obs.Groups))
	}
	if len(obs.Groups[0].Resources) != 0 {
		t.Errorf("an Online group carries no resources, got %d", len(obs.Groups[0].Resources))
	}
	sddc := obs.Groups[1]
	if len(sddc.Resources) != 2 {
		t.Fatalf("want 2 resources on the partial group, got %d", len(sddc.Resources))
	}
	// The type is what separates a resource that carries data from one that only
	// manages, so it must survive as well as the name.
	if sddc.Resources[1].Name != "SDDC Management" || sddc.Resources[1].Type != "SDDC Management" || sddc.Resources[1].State != "Offline" {
		t.Errorf("the down resource lost detail in transit: %+v", sddc.Resources[1])
	}
}

// Remove-ClusterGroup and Stop-ClusterGroup type -Name as a StringCollection.
// Binding a plain string to it fails with "Cannot convert ... to the type", so
// the group is never removed — and under -ErrorAction SilentlyContinue that
// happens silently, on every teardown.
//
// RemoveReplicaBroker learned this against a real cluster and documents it in its
// own comment. The destroy script was still doing it, and only the failure
// report exposed it: on the rig 2026-08-24 Remove-Cluster refused with
// "S2DCluster-Brk[Unknown]=Online" and the broker's whole client access point
// still standing, after the step that was supposed to have removed it.
func TestDestroyClusterPipesTheGroupRatherThanNamingIt(t *testing.T) {
	s := destroyClusterScript

	if strings.Contains(s, "Remove-ClusterGroup -Name") || strings.Contains(s, "Stop-ClusterGroup -Name") {
		t.Fatal("-Name binds to a StringCollection and silently fails; the group object must be piped")
	}
	if !strings.Contains(s, "$g | Remove-ClusterGroup -RemoveResources -Force -ErrorAction Stop") {
		t.Error("the group must be piped, and the failure must not be swallowed")
	}
	// One resource refusing to go offline blocks the whole group — the fallback
	// that makes a Replica Broker come out at all.
	if !strings.Contains(s, "$r | Remove-ClusterResource -Force -ErrorAction SilentlyContinue") {
		t.Error("a group that refuses must have its resources dropped individually, then be retried")
	}
	// A disk resource named by string has the same hazard.
	if strings.Contains(s, "Remove-ClusterResource -Name $r.Name") {
		t.Error("the disk resource must be piped too")
	}
}

// Reporting what survived describes the symptom. WHY each removal refused is the
// part that was left on the host for somebody to go and find.
func TestDestroyClusterReportsWhyARemovalFailed(t *testing.T) {
	s := destroyClusterScript
	if !strings.Contains(s, "$groupErrs = @()") {
		t.Fatal("per-step failures must be collected, not discarded")
	}
	if !strings.Contains(s, `removals that failed: `) {
		t.Error("the reasons must reach the operator alongside the leftovers")
	}
	// And the existing report must survive: what is left is still worth saying.
	for _, want := range []string{"groups remaining", "resources still online", "nodes: "} {
		if !strings.Contains(s, want) {
			t.Errorf("the existing state report must be kept; missing %q", want)
		}
	}
}

/*
A cluster object is never looked up by -Name from a PROPERTY or an INDEX.

	Get-ClusterResource and Get-ClusterGroup declare -Name as a StringCollection.
	A quoted literal binds fine, which is why most call sites here are safe. What
	fails is a value arriving as a PSObject — $x.Name, $arr[0] — with "Cannot
	convert 'Cluster Disk 1' to the type
	System.Collections.Specialized.StringCollection".

	Three times in this repo: Remove-ClusterGroup in RemoveReplicaBroker,
	Ensure-ResourceOnline, and the LUN adoption on Primary1 2026-08-25 — where the
	rename SUCCEEDED and the lookup after it threw, so a finished adoption was
	reported to the operator as a failure.

	Deliberately narrow: flagging the quoted-literal call sites too would cry wolf
	on eight safe ones, and a test nobody trusts gets deleted.
*/
func TestNoClusterObjectIsLookedUpByNameFromAnObject(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	bad := regexp.MustCompile(`Get-Cluster(Resource|Group) -Name \$[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z]|\[)`)
	comment := regexp.MustCompile(`^\s*(#|//)`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatal(rerr)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if comment.MatchString(line) || !bad.MatchString(line) {
				continue
			}
			t.Errorf("%s:%d: -Name binds to a StringCollection and throws on a PSObject — enumerate and filter, or cast to [string]:\n\t%s",
				f, i+1, strings.TrimSpace(line))
		}
	}
}
