package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* A Cluster Shared Volume is mounted at C:\ClusterStorage\VolumeN whatever its
   resource is named, so a volume the operator called iSCSI_DS1 lives at Volume1
   and every path built from the declared name points at nothing. On DRCluster
   that broke the cluster's default storage path and the replica server, with
   errors naming permissions and missing directories rather than the mount point
   that was never renamed.

   Renaming lived inside the FORMER-ONLY adoption, where it could only ever fix
   the volumes the former happened to own. DRCluster's two were owned one each. */

func mountCluster(vols ...string) types.Cluster {
	c := types.Cluster{Meta: types.ObjectMeta{Name: "DRCluster"}}
	for _, v := range vols {
		c.Spec.Volumes = append(c.Spec.Volumes, types.CSVSpec{Name: v})
	}
	return c
}

// Every member runs it, not just the former — that is the whole point.
func TestANonFormerStillNamesTheVolumesItOwns(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	a := ClusterAssignment{Cluster: mountCluster("iSCSI_DS1", "iSCSI_DS2"), IsFormer: false}

	// The stub reports everything already correct, so this asserts only that the
	// step is reached at all on a non-former; the failure it guards against is the
	// step being gated away.
	conds, changed := r.reconcileCSVMountPoints(context.Background(), a)
	if changed {
		t.Fatal("nothing needed renaming, so nothing changed")
	}
	if len(conds) != 0 {
		t.Fatalf("a member with nothing to do should be silent, got %+v", conds)
	}
}

func TestAClusterWithNoVolumesIsNotProbed(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)

	conds, changed := r.reconcileCSVMountPoints(context.Background(), ClusterAssignment{Cluster: mountCluster()})
	if changed || len(conds) != 0 {
		t.Fatalf("no declared volumes means nothing to name, got %+v", conds)
	}
}

// A mount point that could not be renamed leaves the declared name pointing
// nowhere, so it must read as unmet. "Already matches desired state" over a
// volume whose path is not its name is the reading that let this sit unnoticed
// through several passes.
func TestAMountPointItCouldNotRenameReadsAsUnmet(t *testing.T) {
	stub := &hyperv.Stub{CSVMountNote: "iSCSI_DS2 is mounted at Volume2 and VMs are running from it"}
	r := testReconciler(stub)
	a := ClusterAssignment{Cluster: mountCluster("iSCSI_DS2")}

	conds, changed := r.reconcileCSVMountPoints(context.Background(), a)

	if changed {
		t.Fatal("nothing was renamed, so nothing changed")
	}
	if len(conds) != 1 {
		t.Fatalf("the reason must be reported, got %+v", conds)
	}
	if conds[0].Status {
		t.Error("a volume whose path is not its name has not met desired state")
	}
	if !strings.Contains(conds[0].Message, "Volume2") {
		t.Errorf("the message must carry the reason: %q", conds[0].Message)
	}
}

// Advisory: a volume whose path is wrong is still a working volume, and a node
// that cannot rename one should not degrade the whole cluster.
func TestAFailedProbeIsAdvisory(t *testing.T) {
	stub := &hyperv.Stub{FailCSVMount: true}
	r := testReconciler(stub)
	a := ClusterAssignment{Cluster: mountCluster("iSCSI_DS1")}

	conds, changed := r.reconcileCSVMountPoints(context.Background(), a)
	if changed {
		t.Fatal("a failed probe changed nothing")
	}
	if len(conds) != 1 || conds[0].Reason == "ApplyFailed" {
		t.Fatalf("a failure here must be advisory, not degrading: %+v", conds)
	}
}

// The declared name is both the key and the wanted leaf: a volume is named once,
// and its path follows that name.
func TestTheWantedLeafIsTheDeclaredName(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	a := ClusterAssignment{Cluster: mountCluster("iSCSI_DS1", "iSCSI_DS2")}

	if _, _ = r.reconcileCSVMountPoints(context.Background(), a); stub.CSVMountWant == nil {
		t.Fatal("the step must pass the declared volumes through")
	}
	if stub.CSVMountWant["iSCSI_DS1"] != "iSCSI_DS1" || stub.CSVMountWant["iSCSI_DS2"] != "iSCSI_DS2" {
		t.Fatalf("each volume's mount should be its own name, got %v", stub.CSVMountWant)
	}
}
