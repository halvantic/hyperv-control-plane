package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

func iscsiCluster(vols ...types.CSVSpec) types.Cluster {
	return types.Cluster{
		Meta: types.ObjectMeta{Name: "DRCluster", Generation: 1},
		Spec: types.ClusterSpec{
			Members: []string{"hv04", "hv05"},
			Storage: &types.ClusterStorageSpec{
				Kind: types.StorageKindISCSI,
				ISCSI: &types.ISCSIStorageSpec{
					Portals: []string{"10.0.60.52"},
					Targets: []string{"iqn.2000-01.com.synology:Xpenology.default-target.aa584d33bc2"},
				},
			},
			Volumes: vols,
		},
	}
}

func connectedISCSI() *types.ISCSIStatus {
	return &types.ISCSIStatus{
		Sessions:      []types.ISCSISession{{TargetIQN: "t", Connected: true}},
		MPIOInstalled: true, MPIOEffective: true,
	}
}

// Multipath declared but not in effect is the one state where adopting is
// actively unsafe rather than merely premature: Windows is presenting each LUN
// once per path as unrelated disks, so adoption takes ONE of the duplicates into
// the cluster. A cluster writing to one path's disk while a node uses another
// path to the same blocks is a corruption, not a performance problem.
//
// A warning condition cannot carry this. The reconcile loop does not read
// conditions, so a warning alone leaves the adoption happening anyway with the
// console displaying the warning beside it.
func TestAdoptionIsBlockedUntilMultipathIsInEffect(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	c := iscsiCluster(lunVolume("DS1", "CLEAN0001"))
	c.Spec.Storage.ISCSI.Portals = []string{"10.0.60.52", "10.0.60.53"}
	a := ClusterAssignment{Cluster: c, IsFormer: true}

	half := &types.ISCSIStatus{
		Sessions:      []types.ISCSISession{{TargetIQN: "t", Connected: true}},
		MPIOInstalled: true, MPIOEffective: false,
	}

	conds, changed, err := r.reconcileISCSIVolumes(context.Background(), a, half)

	if changed {
		t.Fatal("nothing may be adopted while each LUN is still presented once per path")
	}
	if err != nil {
		t.Fatalf("waiting for a restart is a deferral, not a failure: %v", err)
	}
	if len(conds) != 1 || conds[0].Reason != "NotAttempted" {
		t.Fatalf("the block must be visible and named as not attempted, got %+v", conds)
	}
	if !strings.Contains(conds[0].Message, "Restart") {
		t.Errorf("the message must name the one step that clears it: %q", conds[0].Message)
	}
	if stub.ISCSIAdopted["DS1"] {
		t.Fatal("the adoption reached the host despite the block")
	}
}

// A single-portal cluster has one path, so there is nothing for MPIO to protect
// and no reason to hold adoption up waiting for it.
func TestSinglePathAdoptionIsNotHeldUpByMPIO(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	c := iscsiCluster(lunVolume("DS1", "CLEAN0001"))
	c.Spec.Storage.ISCSI.Portals = []string{"10.0.60.52"}
	a := ClusterAssignment{Cluster: c, IsFormer: true}

	single := &types.ISCSIStatus{
		Sessions:      []types.ISCSISession{{TargetIQN: "t", Connected: true}},
		MPIOInstalled: false, MPIOEffective: false,
	}

	_, changed, err := r.reconcileISCSIVolumes(context.Background(), a, single)
	if err != nil || !changed {
		t.Fatalf("one portal is one path; adoption must proceed: changed=%v err=%v", changed, err)
	}
}

func lunVolume(name, serial string) types.CSVSpec {
	return types.CSVSpec{Name: name, Source: &types.CSVSourceSpec{SerialNumber: serial}}
}

// The load-bearing rule: adoption formats the LUN, so a LUN with contents is
// refused. An array presents LUNs to whoever it is told to, and a serial typed
// one character out looks exactly like a new disk until its contents are gone.
func TestALUNWithDataIsRefusedRatherThanFormatted(t *testing.T) {
	stub := &hyperv.Stub{ISCSIOccupiedSerials: []string{"OCCUPIED01"}}
	r := testReconciler(stub)
	a := ClusterAssignment{Cluster: iscsiCluster(lunVolume("DS1", "OCCUPIED01")), IsFormer: true}

	conds, changed, err := r.reconcileISCSIVolumes(context.Background(), a, connectedISCSI())

	if err == nil {
		t.Fatal("adopting a LUN that already contains a filesystem must fail, not proceed")
	}
	if changed {
		t.Fatal("nothing may be reported as changed when the adoption was refused")
	}
	var found bool
	for _, c := range conds {
		if c.Type == "CSV/DS1" {
			found = true
			if c.Status {
				t.Error("the condition must record the failure")
			}
			// The operator has to be able to tell "wrong LUN" from "array problem".
			if !strings.Contains(strings.ToLower(c.Message), "contains") {
				t.Errorf("the refusal must say what is on the disk: %q", c.Message)
			}
		}
	}
	if !found {
		t.Fatal("the volume must report a condition either way")
	}
}

// Wipe is never taken from desired state. Formatting a disk with contents is a
// decision made once about one disk, not a field that re-applies every pass.
func TestReconcileNeverAsksToWipe(t *testing.T) {
	stub := &hyperv.Stub{ISCSIOccupiedSerials: []string{"OCCUPIED01"}}
	r := testReconciler(stub)
	a := ClusterAssignment{Cluster: iscsiCluster(lunVolume("DS1", "OCCUPIED01")), IsFormer: true}

	if _, _, err := r.reconcileISCSIVolumes(context.Background(), a, connectedISCSI()); err == nil {
		t.Fatal("the reconcile path must not be able to wipe a disk, however the volume is declared")
	}
}

func TestACleanLUNIsAdoptedAndIsIdempotent(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	a := ClusterAssignment{Cluster: iscsiCluster(lunVolume("DS1", "CLEAN0001")), IsFormer: true}

	_, changed, err := r.reconcileISCSIVolumes(context.Background(), a, connectedISCSI())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("the first pass adopts the LUN and must report the change")
	}

	_, changed, err = r.reconcileISCSIVolumes(context.Background(), a, connectedISCSI())
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("a second pass over an adopted LUN must be a no-op; re-reporting a change every pass is what makes a settled cluster look like it is churning")
	}
}

// Adopting is a cluster-wide act on a shared object. Several members racing to
// adopt the same LUN is how a disk ends up partitioned by one node while another
// is formatting it.
func TestOnlyTheFormerAdopts(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	a := ClusterAssignment{Cluster: iscsiCluster(lunVolume("DS1", "CLEAN0001")), IsFormer: false}

	conds, changed, err := r.reconcileISCSIVolumes(context.Background(), a, connectedISCSI())
	if err != nil || changed || len(conds) != 0 {
		t.Fatalf("a non-former must do nothing at all: conds=%v changed=%v err=%v", conds, changed, err)
	}
}

// A node not logged in cannot see any LUN. Reporting that as "the array does not
// present it" sends the operator to the array to fix something that is right.
func TestAdoptionDefersWhenTheNodeHasNoSession(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	a := ClusterAssignment{Cluster: iscsiCluster(lunVolume("DS1", "CLEAN0001")), IsFormer: true}

	conds, changed, err := r.reconcileISCSIVolumes(context.Background(), a, &types.ISCSIStatus{})
	if err != nil {
		t.Fatalf("no session is a reason to wait, not to fail: %v", err)
	}
	if changed {
		t.Fatal("nothing can have changed with no session")
	}
	if len(conds) != 1 || conds[0].Reason != "NotAttempted" {
		t.Fatalf("the deferral must be visible and named as not attempted, got %+v", conds)
	}
	if !strings.Contains(conds[0].Message, "logged in") {
		t.Errorf("the message must point at the session, not the array: %q", conds[0].Message)
	}
}

// Under iSCSI the array owns the LUN, so a volume with no source is not
// something Ballast can create. Falling through to the S2D provisioning path
// would fail with a message about a pool this cluster does not have.
func TestAVolumeWithNoSourceIsNamedAsTheProblem(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	a := ClusterAssignment{Cluster: iscsiCluster(types.CSVSpec{Name: "DS1", SizeBytes: 1 << 40}), IsFormer: true}

	conds, _, err := r.reconcileISCSIVolumes(context.Background(), a, connectedISCSI())
	if err == nil {
		t.Fatal("a volume with no LUN behind it cannot be adopted")
	}
	if len(conds) != 1 || !strings.Contains(conds[0].Message, "serial number") {
		t.Fatalf("the condition must say what is missing and how to supply it, got %+v", conds)
	}
}

// The witness is adopted as a clustered disk, never as a CSV: a CSV is mounted on
// every node at once, which is the opposite of what arbitrates quorum.
func TestTheWitnessLUNIsAdoptedAsAWitnessNotACSV(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	c := iscsiCluster()
	c.Spec.Witness = types.WitnessSpec{Type: types.WitnessDisk, Disk: &types.CSVSourceSpec{SerialNumber: "WITNESS001"}}
	a := ClusterAssignment{Cluster: c, IsFormer: true}

	conds, changed, err := r.reconcileISCSIVolumes(context.Background(), a, connectedISCSI())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("the witness disk should have been adopted")
	}
	var saw bool
	for _, cd := range conds {
		if cd.Type == "WitnessDisk" {
			saw = true
		}
		if strings.HasPrefix(cd.Type, "CSV/") {
			t.Errorf("the witness must not be adopted as a CSV, got condition %q", cd.Type)
		}
	}
	if !saw {
		t.Fatal("the witness adoption must report its own condition")
	}
}

// A cluster on S2D must not go down this path at all — its volumes are
// provisioned from the pool, not adopted from an array.
func TestAnS2DClusterIsUntouched(t *testing.T) {
	stub := &hyperv.Stub{}
	r := testReconciler(stub)
	c := iscsiCluster(lunVolume("DS1", "CLEAN0001"))
	c.Spec.Storage = &types.ClusterStorageSpec{Kind: types.StorageKindS2D}
	a := ClusterAssignment{Cluster: c, IsFormer: true}

	conds, changed, err := r.reconcileISCSIVolumes(context.Background(), a, connectedISCSI())
	if err != nil || changed || len(conds) != 0 {
		t.Fatalf("S2D must not reach the adoption path: conds=%v changed=%v err=%v", conds, changed, err)
	}
}
