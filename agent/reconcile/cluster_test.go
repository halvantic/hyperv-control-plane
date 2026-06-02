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
	res, err := testReconciler(stub).ReconcileCluster(context.Background(), ClusterAssignment{IsMember: false})
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
	res, err := testReconciler(stub).ReconcileCluster(context.Background(), clusterAssignment(true))
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
	res, err := testReconciler(stub).ReconcileCluster(context.Background(), clusterAssignment(false))
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

// Once the cluster exists, the pass is honoured with no forming.
func TestReconcileClusterAlreadyFormed(t *testing.T) {
	stub := &hyperv.Stub{
		ClusteringInstalled: true,
		ClusterExists:       true,
		ClusterName:         "bcluster",
		ClusterMembers:      []string{"HV01", "HV02", "HV03"},
	}
	res, err := testReconciler(stub).ReconcileCluster(context.Background(), clusterAssignment(true))
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
