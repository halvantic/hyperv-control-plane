package reconcile

import (
	"context"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

func clusterAssignment(isFormer bool) ClusterAssignment {
	return ClusterAssignment{
		IsMember: true,
		IsFormer: isFormer,
		Cluster: types.Cluster{
			Meta: types.ObjectMeta{Name: "bcluster", Generation: 1},
			Spec: types.ClusterSpec{Members: []string{"HV01", "HV02", "HV03"}, ManagementIP: "192.168.1.200"},
		},
	}
}

// A non-member node does nothing and is trivially honoured.
func TestReconcileClusterNonMember(t *testing.T) {
	stub := &hyperv.Stub{}
	res, err := testReconciler(stub).ReconcileCluster(context.Background(), ClusterAssignment{IsMember: false}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Honoured {
		t.Fatalf("non-member should be honoured: %+v", res)
	}
	if stub.ClusteringInstalled {
		t.Fatal("non-member must not install the clustering feature")
	}
}

// The designated former installs the feature and forms the cluster.
func TestReconcileClusterFormerForms(t *testing.T) {
	stub := &hyperv.Stub{}
	res, err := testReconciler(stub).ReconcileCluster(context.Background(), clusterAssignment(true), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !stub.ClusteringInstalled {
		t.Fatal("former should install the clustering feature")
	}
	if !stub.FormCalled {
		t.Fatal("former should form the cluster")
	}
	if !res.Honoured || res.Phase != types.PhaseReady {
		t.Fatalf("want honoured/ready after forming, got %+v", res)
	}
	if len(res.FormedMembers) != 3 {
		t.Fatalf("want 3 formed members, got %v", res.FormedMembers)
	}
}

// A non-former member installs the feature but waits for the former; it must not
// run New-Cluster itself (that would race two formers).
func TestReconcileClusterNonFormerWaits(t *testing.T) {
	stub := &hyperv.Stub{}
	res, err := testReconciler(stub).ReconcileCluster(context.Background(), clusterAssignment(false), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !stub.ClusteringInstalled {
		t.Fatal("member should install the clustering feature")
	}
	if stub.FormCalled {
		t.Fatal("non-former must not form the cluster")
	}
	if res.Honoured || res.Phase != types.PhaseProgressing {
		t.Fatalf("non-former should be progressing/not-honoured while waiting, got %+v", res)
	}
}

func clusterAssignmentWithS2D(isFormer bool) ClusterAssignment {
	a := clusterAssignment(isFormer)
	a.Cluster.Spec.EnableS2D = true
	a.Cluster.Spec.Volumes = []types.CSVSpec{
		{Name: "Vol01", SizeBytes: 50 << 30, ResiliencyType: "Mirror"},
		{Name: "Vol02", SizeBytes: 50 << 30},
	}
	return a
}

// The former, with the cluster already formed, enables S2D and provisions the
// CSVs.
func TestReconcileClusterEnablesS2DAndCSVs(t *testing.T) {
	stub := &hyperv.Stub{
		ClusteringInstalled: true, ClusterExists: true,
		ClusterName: "bcluster", ClusterMembers: []string{"HV01", "HV02", "HV03"},
	}
	res, err := testReconciler(stub).ReconcileCluster(context.Background(), clusterAssignmentWithS2D(true), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !stub.EnableS2DCalled || !stub.S2DEnabled {
		t.Fatal("former should enable S2D")
	}
	if len(stub.CSVs) != 2 {
		t.Fatalf("want 2 CSVs created, got %v", stub.CSVs)
	}
	if !res.S2DEnabled || !res.Honoured || res.Phase != types.PhaseReady {
		t.Fatalf("want s2d/honoured/ready, got %+v", res)
	}
}

// A converged storage state (S2D on, CSVs present) makes no changes.
func TestReconcileClusterStorageIdempotent(t *testing.T) {
	stub := &hyperv.Stub{
		ClusteringInstalled: true, ClusterFirewallOpen: true, ClusterExists: true,
		ClusterName: "bcluster", ClusterMembers: []string{"HV01", "HV02", "HV03"},
		S2DEnabled: true, CSVs: []string{"Vol01", "Vol02"},
	}
	res, err := testReconciler(stub).ReconcileCluster(context.Background(), clusterAssignmentWithS2D(true), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stub.EnableS2DCalled {
		t.Fatal("must not re-enable S2D when already on")
	}
	if res.Changed {
		t.Fatal("converged storage should not report Changed")
	}
	if !res.S2DEnabled || !res.Honoured {
		t.Fatalf("want s2d/honoured, got %+v", res)
	}
}

// A non-former never touches storage, even with EnableS2D set.
func TestReconcileClusterNonFormerSkipsStorage(t *testing.T) {
	stub := &hyperv.Stub{
		ClusteringInstalled: true, ClusterExists: true,
		ClusterName: "bcluster", ClusterMembers: []string{"HV01", "HV02", "HV03"},
	}
	if _, err := testReconciler(stub).ReconcileCluster(context.Background(), clusterAssignmentWithS2D(false), nil); err != nil {
		t.Fatal(err)
	}
	if stub.EnableS2DCalled || len(stub.CSVs) != 0 {
		t.Fatal("non-former must not enable S2D or create CSVs")
	}
}

// Once the cluster exists, the pass is honoured with no forming.
func TestReconcileClusterAlreadyFormed(t *testing.T) {
	stub := &hyperv.Stub{
		ClusteringInstalled: true,
		ClusterExists:       true,
		ClusterName:         "bcluster",
		ClusterMembers:      []string{"HV01", "HV02", "HV03"},
	}
	res, err := testReconciler(stub).ReconcileCluster(context.Background(), clusterAssignment(true), nil)
	if err != nil {
		t.Fatal(err)
	}
	if stub.FormCalled {
		t.Fatal("must not re-form an existing cluster")
	}
	if !res.Honoured || res.Phase != types.PhaseReady || len(res.FormedMembers) != 3 {
		t.Fatalf("want honoured/ready with members, got %+v", res)
	}
}
