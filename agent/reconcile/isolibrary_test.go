package reconcile

import (
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

func hostWithLibrary(path string) types.Host {
	h := types.Host{Meta: types.ObjectMeta{Name: "hv04"}}
	if path != "" {
		h.Spec.ISOLibrary = &types.ISOLibrarySpec{Path: path}
	}
	return h
}

func memberOf(path string) *ClusterAssignment {
	a := &ClusterAssignment{IsMember: true, Cluster: types.Cluster{Meta: types.ObjectMeta{Name: "c1"}}}
	if path != "" {
		a.Cluster.Spec.ISOLibrary = &types.ISOLibrarySpec{Path: path}
	}
	return a
}

func TestAStandaloneHostUsesItsOwnLibrary(t *testing.T) {
	got := EffectiveISOLibrary(hostWithLibrary(`\\nas\isos`), nil)
	if got == nil || got.Path != `\\nas\isos` {
		t.Fatalf("a standalone host must use its own library, got %+v", got)
	}
	// And a host that declares none has none.
	if got := EffectiveISOLibrary(hostWithLibrary(""), nil); got != nil {
		t.Fatalf("no declaration means no library, got %+v", got)
	}
}

func TestAClusterMemberUsesTheClusterLibrary(t *testing.T) {
	got := EffectiveISOLibrary(hostWithLibrary(""), memberOf(`\\nas\cluster-isos`))
	if got == nil || got.Path != `\\nas\cluster-isos` {
		t.Fatalf("a member must use the cluster's library, got %+v", got)
	}
}

// The whole point of the rule: neither declaration leaks into the other's scope.
// A host-level library on a member is ignored, not merged and not preferred —
// two libraries on one host means two answers to "where does this VM's media
// come from", and the operator who set the cluster one could not see the other
// overriding it.
func TestAHostLibraryIsNotInheritedIntoACluster(t *testing.T) {
	got := EffectiveISOLibrary(hostWithLibrary(`\\nas\host-isos`), memberOf(`\\nas\cluster-isos`))
	if got == nil || got.Path != `\\nas\cluster-isos` {
		t.Fatalf("the cluster's library must win on a member, got %+v", got)
	}
}

// A member of a cluster that declares none has none. Falling back to the host's
// own would keep a standalone library alive under a cluster that never declared
// it — inheritance by the back door, and invisible from the cluster page.
func TestAMemberOfAClusterWithNoLibraryHasNone(t *testing.T) {
	if got := EffectiveISOLibrary(hostWithLibrary(`\\nas\host-isos`), memberOf("")); got != nil {
		t.Fatalf("a member must not fall back to its host-level library, got %+v", got)
	}
}

// An assignment that says "not a member" is a standalone host however it arrived,
// so the host's own library applies again. This is the eviction path: the
// membership goes, the cluster library goes with it, and nothing had to be
// cleaned up because nothing was ever copied.
func TestAnEvictedHostFallsBackToItsOwnLibrary(t *testing.T) {
	notMember := &ClusterAssignment{IsMember: false}
	got := EffectiveISOLibrary(hostWithLibrary(`\\nas\host-isos`), notMember)
	if got == nil || got.Path != `\\nas\host-isos` {
		t.Fatalf("a non-member uses its own library, got %+v", got)
	}
}
