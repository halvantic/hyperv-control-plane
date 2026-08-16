package reconcile

import (
	"context"
	"strings"
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

// onlineCore is a cluster whose name and IP addresses are up, which is the
// precondition for any storage work at all.
func onlineCore() []hyperv.ClusterGroup {
	return []hyperv.ClusterGroup{{Name: "Cluster Group", State: "Online"}}
}

func s2dCluster() ClusterAssignment {
	return formerOf(types.ClusterSpec{Storage: &types.ClusterStorageSpec{Kind: types.StorageKindS2D}})
}

/* Storage must not be attempted while the cluster has no identity.

   Enable-ClusterStorageSpacesDirect cannot succeed with the cluster name and IP
   offline, and failing is expensive: on the rig (S2DCluster, 2026-08-16)
   EnsureS2DPoolDisks took 3m57s a go because it cycles S2D when it finds no
   pool, and the pass was cut off at the five-minute cycle limit before it
   reached the cluster state read. Every pass burned four minutes on something
   that could not work, and the centre received no cluster status at all — so the
   ClusterCoreGroup condition naming the real fault never arrived and the console
   showed an empty cluster page.

   The core-group condition already told operators to fix that first. These
   tests are what make the reconciler agree with it. */
func TestStorageIsNotAttemptedWhileTheCoreGroupIsDown(t *testing.T) {
	for _, state := range []string{"Pending", "Offline", "PartialOnline", "Failed"} {
		t.Run(state, func(t *testing.T) {
			stub := &hyperv.Stub{}
			r := testReconciler(stub)
			groups := []hyperv.ClusterGroup{{Name: "Cluster Group", State: state}}

			s2d, conds, changed, err := r.reconcileStorage(context.Background(), s2dCluster(), groups)

			if err != nil || changed || s2d {
				t.Fatalf("a down core group must be a clean skip: s2d=%v changed=%v err=%v", s2d, changed, err)
			}
			if stub.S2DEnabled {
				t.Fatal("S2D was enabled while the cluster had no identity on the network")
			}
			if len(conds) != 1 || conds[0].Status || conds[0].Reason != "AwaitingCoreGroup" {
				t.Fatalf("the skip must be reported with its reason, got %+v", conds)
			}
			// Silence would read as healthy, and a bare "failed" would send the
			// operator to the storage that is downstream of the actual fault.
			if !strings.Contains(conds[0].Message, "core group") {
				t.Fatalf("the condition must name the core group as the thing to fix, got %q", conds[0].Message)
			}
		})
	}
}

// A core group that was never reported is not an online one. Treating absence as
// permission is how the expensive retry loop started in the first place.
func TestStorageIsNotAttemptedWhenTheCoreGroupIsUnreported(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)

	_, conds, _, err := r.reconcileStorage(context.Background(), s2dCluster(), nil)

	if err != nil {
		t.Fatal(err)
	}
	if stub.S2DEnabled {
		t.Fatal("S2D was enabled on a cluster whose core group could not be read")
	}
	if len(conds) != 1 || conds[0].Reason != "AwaitingCoreGroup" {
		t.Fatalf("an unread core group must skip storage and say so, got %+v", conds)
	}
}

// And it must resume by itself once the cluster comes up — the skip is a wait,
// not a latch an operator has to clear.
func TestStorageResumesOnceTheCoreGroupIsOnline(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)

	if _, _, _, err := r.reconcileStorage(context.Background(), s2dCluster(), onlineCore()); err != nil {
		t.Fatal(err)
	}
	if !stub.S2DEnabled {
		t.Fatal("with the core group online, storage must be reconciled as before")
	}
}

func TestS2DStillRunsForAClusterAuthoredBeforeTheStorageKind(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	_, conds, _, err := r.reconcileStorage(context.Background(), formerOf(types.ClusterSpec{EnableS2D: true}), onlineCore())
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
	}), onlineCore())
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
	}), onlineCore())
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
	_, conds, changed, err := r.reconcileStorage(context.Background(), formerOf(types.ClusterSpec{}), onlineCore())
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
	_, conds, changed, err := r.reconcileStorage(context.Background(), a, onlineCore())
	if err != nil || changed || len(conds) != 0 {
		t.Fatalf("a non-former must not provision: changed=%v conds=%d err=%v", changed, len(conds), err)
	}
}

