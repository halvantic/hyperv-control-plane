package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

/* Quorum is the one setting whose misconfiguration is invisible until a node is
   already lost, so these tests are mostly about restraint: who may touch it, and
   when it must be left alone. */

func witnessAssignment(isFormer bool, w types.WitnessSpec) ClusterAssignment {
	a := clusterAssignment(isFormer)
	a.Cluster.Spec.Witness = w
	return a
}

// A formed cluster with a declared witness gets one, and the observed result is
// reported rather than the state read before the change.
func TestFormerAppliesADeclaredFileShareWitness(t *testing.T) {
	stub := &hyperv.Stub{
		ClusteringInstalled: true, ClusterExists: true,
		ClusterName: "bcluster", ClusterMembers: []string{"HV01", "HV02", "HV03"},
	}
	spec := types.WitnessSpec{Type: types.WitnessFileShare, FileSharePath: `\\fs01\bcluster-witness`}

	res, err := testReconciler(stub).ReconcileCluster(context.Background(), witnessAssignment(true, spec), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.WitnessCalls) != 1 {
		t.Fatalf("want one witness call, got %d", len(stub.WitnessCalls))
	}
	if got := stub.WitnessCalls[0]; got.Type != types.WitnessFileShare || got.FileSharePath != spec.FileSharePath {
		t.Fatalf("wrong witness applied: %+v", got)
	}
	// The status must describe the witness that now exists. Reporting the
	// pre-change observation would show "None" for a full cycle after setting
	// one, which reads as a failure.
	if res.Witness == nil {
		t.Fatal("the applied witness must be reported, not the state read before it was set")
	}
	if res.Witness.Type != types.WitnessFileShare || res.Witness.Path != spec.FileSharePath {
		t.Fatalf("reported witness does not match what was applied: %+v", res.Witness)
	}
}

// Every member can see the cluster, so without a former-only gate all three
// would race to set the same witness and each would read the others' write as
// drift — Set-ClusterQuorum recreates the resource, so that is a rolling loss of
// a vote rather than a harmless duplicate.
func TestOnlyTheFormerTouchesQuorum(t *testing.T) {
	stub := &hyperv.Stub{
		ClusteringInstalled: true, ClusterExists: true,
		ClusterName: "bcluster", ClusterMembers: []string{"HV01", "HV02", "HV03"},
	}
	spec := types.WitnessSpec{Type: types.WitnessFileShare, FileSharePath: `\\fs01\bcluster-witness`}

	if _, err := testReconciler(stub).ReconcileCluster(context.Background(), witnessAssignment(false, spec), nil); err != nil {
		t.Fatal(err)
	}
	if len(stub.WitnessCalls) != 0 {
		t.Fatalf("a non-former must not touch quorum, got %+v", stub.WitnessCalls)
	}
}

// An undeclared witness means "not managed", not "remove the witness". A cluster
// whose quorum an operator set up by hand must survive Ballast learning how to
// configure one.
func TestAnUndeclaredWitnessIsLeftAlone(t *testing.T) {
	stub := &hyperv.Stub{
		ClusteringInstalled: true, ClusterExists: true,
		ClusterName: "bcluster", ClusterMembers: []string{"HV01", "HV02", "HV03"},
		Witness: &hyperv.ClusterWitness{Type: "FileShare", Path: `\\manual\share`, State: "Online"},
	}

	res, err := testReconciler(stub).ReconcileCluster(context.Background(), witnessAssignment(true, types.WitnessSpec{}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.WitnessCalls) != 0 {
		t.Fatalf("an undeclared witness must not be reconfigured, got %+v", stub.WitnessCalls)
	}
	// Still observed and reported, though — not managing it is not a reason to
	// stop looking at it.
	if res.Witness == nil || res.Witness.Path != `\\manual\share` {
		t.Fatalf("an unmanaged witness must still be reported: %+v", res.Witness)
	}
}

// Re-applying an identical witness must be a no-op. Set-ClusterQuorum tears the
// resource down and recreates it even when nothing changes, so a reconciler that
// applied every pass would drop a vote every pass — a self-inflicted wobble on a
// cluster that was fine.
func TestApplyingTheSameWitnessTwiceChangesNothing(t *testing.T) {
	stub := &hyperv.Stub{
		ClusteringInstalled: true, ClusterExists: true,
		ClusterName: "bcluster", ClusterMembers: []string{"HV01", "HV02", "HV03"},
	}
	a := witnessAssignment(true, types.WitnessSpec{
		Type: types.WitnessFileShare, FileSharePath: `\\fs01\bcluster-witness`,
	})
	r := testReconciler(stub)

	first, err := r.ReconcileCluster(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed {
		t.Fatal("the first pass sets the witness, so it changed something")
	}
	second, err := r.ReconcileCluster(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Fatal("re-applying an identical witness must be a no-op; Set-ClusterQuorum recreates the resource and drops a vote")
	}
}

// A witness that cannot be applied — an unreachable share, or the cluster
// computer object lacking permission on it — is reported and retried. It must
// NOT degrade the cluster: quorum is exactly as it was before the attempt, and
// marking the cluster degraded over a failed improvement would send someone
// looking for a fault that does not exist.
func TestAnUnreachableWitnessDoesNotDegradeTheCluster(t *testing.T) {
	stub := &hyperv.Stub{
		ClusteringInstalled: true, ClusterExists: true,
		ClusterName: "bcluster", ClusterMembers: []string{"HV01", "HV02", "HV03"},
		WitnessErr: errors.New("the network path was not found"),
	}
	spec := types.WitnessSpec{Type: types.WitnessFileShare, FileSharePath: `\\gone\share`}

	res, err := testReconciler(stub).ReconcileCluster(context.Background(), witnessAssignment(true, spec), nil)
	if err != nil {
		t.Fatalf("a failed witness must not fail the pass: %v", err)
	}
	if res.Phase == types.PhaseDegraded {
		t.Fatal("a witness that could not be applied leaves quorum unchanged; the cluster is not degraded by it")
	}
	var found bool
	for _, c := range res.Conditions {
		if c.Type == "ClusterWitness" {
			found = true
			if c.Status {
				t.Fatal("the ClusterWitness condition must report the failure, not claim success")
			}
		}
	}
	if !found {
		t.Fatal("a failed witness must surface on a condition rather than passing silently")
	}
}

// Whether a disk witness is possible depends on the cluster's storage kind, and
// that is the point of the discriminator: it turns a flat invariant into a
// per-kind rule.
//
// S2D has no shared block storage, so a disk witness cannot work there and the
// refusal must say so — letting Set-ClusterQuorum fail three layers down tells
// the operator nothing. On an array-backed cluster the same request is
// legitimate (it is the classic choice), so the same refusal would be wrong.
func TestADiskWitnessIsRefusedOnS2DWithAReason(t *testing.T) {
	ps := &hyperv.PowerShell{}
	_, _, err := ps.EnsureClusterWitness(context.Background(),
		types.WitnessSpec{Type: types.WitnessDisk}, types.StorageKindS2D)
	if err == nil {
		t.Fatal("a disk witness must be refused on an S2D cluster")
	}
	if !strings.Contains(err.Error(), "Storage Spaces Direct") {
		t.Fatalf("the refusal must say why, got %q", err)
	}
}

// On iSCSI a disk witness is possible, so the refusal must NOT blame S2D. What
// remains missing is which LUN to use: quorum pointed at the wrong disk is the
// one failure in this area the console cannot undo, so the disk is named
// explicitly or not attempted.
func TestADiskWitnessOnISCSIAsksWhichLUN(t *testing.T) {
	ps := &hyperv.PowerShell{}
	_, _, err := ps.EnsureClusterWitness(context.Background(),
		types.WitnessSpec{Type: types.WitnessDisk}, types.StorageKindISCSI)
	if err == nil {
		t.Fatal("a disk witness that does not say which disk cannot be applied")
	}
	if strings.Contains(err.Error(), "Storage Spaces Direct") {
		t.Fatalf("an array-backed cluster must not be told S2D is the problem, got %q", err)
	}
	if !strings.Contains(err.Error(), "serial number") {
		t.Fatalf("the refusal must name what is missing and how to supply it, got %q", err)
	}
}

// A cluster that declares no storage kind at all cannot have a disk witness
// either, and must not be told it is an S2D limitation.
func TestADiskWitnessWithNoStorageKindSaysSo(t *testing.T) {
	ps := &hyperv.PowerShell{}
	_, _, err := ps.EnsureClusterWitness(context.Background(), types.WitnessSpec{Type: types.WitnessDisk}, "")
	if err == nil {
		t.Fatal("no storage means no disk witness")
	}
	if !strings.Contains(err.Error(), "does not declare any") {
		t.Fatalf("the refusal must say the cluster declares no storage, got %q", err)
	}
}

// A file share witness with no path is a mistake worth catching before it
// reaches PowerShell, where it becomes a parameter-binding error.
func TestAFileShareWitnessNeedsAPath(t *testing.T) {
	ps := &hyperv.PowerShell{}
	// A file share witness is valid on any storage kind, so the kind is irrelevant
	// here — the missing path is the fault.
	_, _, err := ps.EnsureClusterWitness(context.Background(),
		types.WitnessSpec{Type: types.WitnessFileShare}, types.StorageKindS2D)
	if err == nil {
		t.Fatal("a file share witness with no path must be refused")
	}
}
