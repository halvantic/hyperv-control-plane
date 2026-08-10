package reconcile

import (
	"context"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

/* The S2D reconciler must run for exactly the clusters that want S2D, and the
   two eras of spec must be indistinguishable to it.

   A cluster authored before ClusterStorageSpec existed carries only EnableS2D;
   one authored after carries only Storage.Kind. Reading the flag directly would
   silently stop provisioning storage for every converted cluster — the reconcile
   would just return, with every condition it would have raised absent rather
   than failing, which is the shape of bug this codebase keeps finding. */

func formerOf(spec types.ClusterSpec) ClusterAssignment {
	return ClusterAssignment{
		IsMember: true, IsFormer: true,
		Cluster: types.Cluster{Meta: types.ObjectMeta{Name: "c1"}, Spec: spec},
	}
}

func TestS2DStillRunsForAClusterAuthoredBeforeTheStorageKind(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	_, conds, _, err := r.reconcileStorage(context.Background(), formerOf(types.ClusterSpec{EnableS2D: true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(conds) == 0 {
		t.Fatal("an S2D cluster on the deprecated flag must still be reconciled, not silently skipped")
	}
}

func TestS2DRunsForAnExplicitS2DKind(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	_, conds, _, err := r.reconcileStorage(context.Background(), formerOf(types.ClusterSpec{
		Storage: &types.ClusterStorageSpec{Kind: types.StorageKindS2D},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(conds) == 0 {
		t.Fatal("an explicitly S2D cluster must be reconciled")
	}
}

// The point of the discriminator. An iSCSI cluster has no pool, and enabling S2D
// on it would claim its local disks — turning a cluster backed by an array into
// one that has quietly built a second, unwanted pool underneath it.
func TestS2DNeverRunsForAnISCSICluster(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	s2d, conds, changed, err := r.reconcileStorage(context.Background(), formerOf(types.ClusterSpec{
		// Deliberately also carrying the stale flag: a converted cluster keeps it,
		// and honouring it here is precisely the failure to avoid.
		EnableS2D: true,
		Storage:   &types.ClusterStorageSpec{Kind: types.StorageKindISCSI},
	}))
	if err != nil || changed || s2d || len(conds) != 0 {
		t.Fatalf("S2D must not touch an iSCSI cluster: s2d=%v changed=%v conds=%d err=%v", s2d, changed, len(conds), err)
	}
	if stub.S2DEnabled {
		t.Fatal("S2D was enabled on a cluster backed by an array")
	}
}

// A cluster that declares no storage at all is left alone, as before.
func TestNoStorageDeclaredIsANoOp(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	_, conds, changed, err := r.reconcileStorage(context.Background(), formerOf(types.ClusterSpec{}))
	if err != nil || changed || len(conds) != 0 {
		t.Fatalf("no storage declared must be a no-op: changed=%v conds=%d err=%v", changed, len(conds), err)
	}
}

// Only the former provisions storage, whatever the kind — otherwise every member
// races to build the same thing.
func TestOnlyTheFormerReconcilesStorage(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	a := formerOf(types.ClusterSpec{Storage: &types.ClusterStorageSpec{Kind: types.StorageKindS2D}})
	a.IsFormer = false
	_, conds, changed, err := r.reconcileStorage(context.Background(), a)
	if err != nil || changed || len(conds) != 0 {
		t.Fatalf("a non-former must not provision: changed=%v conds=%d err=%v", changed, len(conds), err)
	}
}
