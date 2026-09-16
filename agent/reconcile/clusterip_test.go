package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* Changing ManagementIP on a formed cluster used to do nothing.

   It was read in exactly one place — New-Cluster -StaticAddress at formation. An
   operator editing it afterwards got a stored value, a new generation, and no
   change on the cluster, with nothing reporting that the setting was not being
   honoured. On the rig S2DCluster was formed on an address that turned out to be
   in use; the edit to a free one was accepted and ignored, and the only route to
   actually fixing it was a PowerShell session on a node. */

func clusterWithIP(ip string, isFormer bool) ClusterAssignment {
	return ClusterAssignment{
		IsMember: true, IsFormer: isFormer,
		Cluster: types.Cluster{
			Meta: types.ObjectMeta{Name: "S2DCluster", Generation: 2},
			Spec: types.ClusterSpec{Members: []string{"HVNEW01", "HVNEW02", "HVNEW03"}, ManagementIP: ip},
		},
	}
}

func condByType(conds []types.Condition, t string) (types.Condition, bool) {
	for _, c := range conds {
		if c.Type == t {
			return c, true
		}
	}
	return types.Condition{}, false
}

func TestADeclaredClusterIPIsApplied(t *testing.T) {
	stub := &hyperv.Stub{ClusterIP: "192.168.1.40"}
	r := testReconciler(stub)

	res, _ := r.ReconcileCluster(context.Background(), clusterWithIP("192.168.1.230", true), nil)

	if stub.ClusterIP != "192.168.1.230" {
		t.Fatalf("the declared address must be applied, cluster is still on %q", stub.ClusterIP)
	}
	c, ok := condByType(res.Conditions, "ClusterIP")
	if !ok || !c.Status {
		t.Fatalf("the change must be reported, got %+v", res.Conditions)
	}
}

// Idempotency: the address already being right must not take the cluster's IP
// resource offline to set it to what it already is.
func TestAMatchingClusterIPIsANoOp(t *testing.T) {
	stub := &hyperv.Stub{ClusterIP: "192.168.1.230"}
	r := testReconciler(stub)

	res, _ := r.ReconcileCluster(context.Background(), clusterWithIP("192.168.1.230", true), nil)

	c, ok := condByType(res.Conditions, "ClusterIP")
	if !ok || !c.Status {
		t.Fatalf("a matching address must still report, got %+v", res.Conditions)
	}
	if c.Reason != "AlreadyConfigured" {
		t.Fatalf("a matching address must be a no-op, got reason %q", c.Reason)
	}
}

// One node changes a cluster-wide resource.
func TestOnlyTheFormerReAddressesTheCluster(t *testing.T) {
	stub := &hyperv.Stub{ClusterIP: "192.168.1.40"}
	r := testReconciler(stub)

	res, _ := r.ReconcileCluster(context.Background(), clusterWithIP("192.168.1.230", false), nil)

	if stub.ClusterIP != "192.168.1.40" {
		t.Fatal("a non-former must not re-address the cluster")
	}
	if _, ok := condByType(res.Conditions, "ClusterIP"); ok {
		t.Fatal("a non-former must not report on an address it does not own")
	}
}

// Empty means not declared. A cluster on DHCP must not be quietly pinned.
func TestAnUndeclaredClusterIPIsLeftAlone(t *testing.T) {
	stub := &hyperv.Stub{ClusterIP: "192.168.1.40"}
	r := testReconciler(stub)

	res, _ := r.ReconcileCluster(context.Background(), clusterWithIP("", true), nil)

	if stub.ClusterIP != "192.168.1.40" {
		t.Fatal("an undeclared address must not change the cluster")
	}
	if _, ok := condByType(res.Conditions, "ClusterIP"); ok {
		t.Fatal("nothing was declared, so nothing should be reported")
	}
}

/*
The refusal has to reach the operator.

	Re-addressing fails for reasons they must act on — the new address is also in
	use, or no cluster network covers it. Swallowing that would reproduce the
	original bug in a new place: a setting stored, apparently accepted, and not in
	effect.
*/
func TestARefusedReAddressIsReported(t *testing.T) {
	stub := &hyperv.Stub{
		ClusterIP:    "192.168.1.40",
		ClusterIPErr: errors.New("no cluster network covers 10.9.9.9, so no member has an adapter on that subnet"),
	}
	r := testReconciler(stub)

	res, _ := r.ReconcileCluster(context.Background(), clusterWithIP("10.9.9.9", true), nil)

	c, ok := condByType(res.Conditions, "ClusterIP")
	if !ok || c.Status {
		t.Fatalf("a refused re-address must be reported as failing, got %+v", res.Conditions)
	}
	if !strings.Contains(c.Message, "no cluster network covers") {
		t.Fatalf("the reason must reach the operator, got %q", c.Message)
	}
}

// And it must not stop the pass: the rest of the reconcile still has value, and
// the cluster still needs reporting on.
func TestARefusedReAddressDoesNotAbortThePass(t *testing.T) {
	stub := &hyperv.Stub{ClusterIP: "192.168.1.40", ClusterIPErr: errors.New("refused")}
	r := testReconciler(stub)

	res, _ := r.ReconcileCluster(context.Background(), clusterWithIP("10.9.9.9", true), nil)

	if _, ok := condByType(res.Conditions, "ClusterFormed"); !ok {
		t.Fatalf("the pass must continue past a refused re-address, got %+v", res.Conditions)
	}
}
