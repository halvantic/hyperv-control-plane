package reconcile

import "github.com/joshua-fourie/ballast/api/types"

// EffectiveISOLibrary decides which ISO library share, if any, this host should
// be using — the single place the "declared for a cluster OR for a standalone
// host, never inherited across" rule lives.
//
// A cluster member takes the cluster's library, read from the assignment the
// agent already receives. Nothing is fanned into member HostSpecs, so there is
// no copy to outlive the membership: evict the host and the assignment goes with
// it, and the library goes too. That matters because a fanned copy is a second
// source of truth, which is exactly how a deleted CSV left storage paths naming
// it on three hosts at once.
//
// A host-level library on a cluster member is deliberately IGNORED rather than
// merged or preferred. Two libraries on one host means two answers to "where
// does this VM's boot media come from", and the operator who set the cluster one
// would have no way to see the host one overriding it. The console only offers
// the host-level field on a standalone host; this function is what makes that
// promise true even if the field is set by other means.
func EffectiveISOLibrary(host types.Host, a *ClusterAssignment) *types.ISOLibrarySpec {
	if a != nil && a.IsMember {
		// A member with no cluster library has none — it does NOT fall back to a
		// host-level one, or joining a cluster would silently keep the standalone
		// library alive under a cluster that never declared it.
		return a.Cluster.Spec.ISOLibrary
	}
	return host.Spec.ISOLibrary
}
