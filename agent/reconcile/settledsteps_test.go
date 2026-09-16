package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* Two steps were paying a great deal, every pass, to learn that nothing had
   changed.

   EnsureReplicaServer is a write path that almost never writes. Finding that out
   runs Get-Service ClusSvc, Get-ClusterResource to locate the Replica Broker,
   Get-VMReplicationServer, and a Get-ClusterSharedVolume scan when the storage
   path is on a CSV. Measured on the rig 2026-08-13 it was 4.4-8.3s of every pass
   on every host — and on HVNEW03, whose replica storage sits on a CSV that had
   failed, 2m44s to 3m52s of four consecutive passes. Three of those four hit the
   five-minute cycle cap and were killed, so they never reached reportStatus and
   the host's readings froze at 16 minutes old while its keepalive held it green.
   The console was blind to a live cluster incident for that whole window. The
   step reported "AlreadyConfigured" every time.

   GetHostRoleState asks Get-WindowsFeature whether the Hyper-V role is installed
   — 1m4s after a reboot, 2m6s on a busy host — to re-learn a boolean that cannot
   change unless something deliberately uninstalls the role, plus a RebootPending
   field nothing in the agent reads.

   Neither is dropped. Both are asked at a rate that matches how often the answer
   can change, and both fall back to every pass in the case where the answer is
   live. */

// countingReplicaHV counts the calls whose cadence is under test.
type countingReplicaHV struct {
	*hyperv.Stub
	replicaCalls int
	roleCalls    int
	replicaErr   error
	replicaOut   hyperv.Outcome
}

func (c *countingReplicaHV) EnsureReplicaServer(ctx context.Context, s types.ReplicaServerSpec) (hyperv.Outcome, error) {
	c.replicaCalls++
	if c.replicaErr != nil {
		return hyperv.OutcomeUnchanged, c.replicaErr
	}
	return c.replicaOut, nil
}

func (c *countingReplicaHV) GetHostRoleState(ctx context.Context) (hyperv.HostRoleState, error) {
	c.roleCalls++
	return c.Stub.GetHostRoleState(ctx)
}

func replicaHost(gen int64) types.Host {
	return types.Host{
		Meta: types.ObjectMeta{Name: "HVNEW03", Generation: gen},
		Spec: types.HostSpec{
			EnableHyperVRole: true,
			ReplicaServer: &types.ReplicaServerSpec{
				Enabled: true, AuthenticationType: "Kerberos",
				DefaultStorageLocation: `C:\ClusterStorage\DS1\Replica`,
			},
		},
	}
}

func newCountingReconciler() (*Reconciler, *countingReplicaHV) {
	hv := &countingReplicaHV{Stub: &hyperv.Stub{HyperVInstalled: true}, replicaOut: hyperv.OutcomeUnchanged}
	return New(hv, nil), hv
}

func TestASettledReplicaServerIsNotReCheckedEveryPass(t *testing.T) {
	r, hv := newCountingReconciler()
	for i := 0; i < replicaServerEvery; i++ {
		if _, err := r.Reconcile(context.Background(), replicaHost(1), nil); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	// The first pass reads for real and settles it; the rest of the window skips.
	if hv.replicaCalls != 1 {
		t.Fatalf("a settled replica server must not be re-read every pass; %d calls in %d passes", hv.replicaCalls, replicaServerEvery)
	}
}

// Skipped is not the same as gone. The console builds its step list from the
// conditions, so a step that stops reporting on the passes it was skipped reads
// as a step that is no longer being reconciled.
func TestASkippedReplicaStepStillReportsItsState(t *testing.T) {
	r, _ := newCountingReconciler()
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, replicaHost(1), nil); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(ctx, replicaHost(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range res.Conditions {
		if c.Type == "ReplicaServer" {
			found = true
		}
	}
	if !found {
		t.Fatal("the step kept its cadence but lost its place in the console")
	}
}

// The every-pass retry is the convergence mechanism while a cluster member waits
// for its Replica Broker. A failing step must keep it.
func TestAFailingReplicaServerKeepsRetryingEveryPass(t *testing.T) {
	r, hv := newCountingReconciler()
	hv.replicaErr = errors.New("waiting for the cluster's broker to be provisioned")
	for i := 0; i < 4; i++ {
		if _, err := r.Reconcile(context.Background(), replicaHost(1), nil); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if hv.replicaCalls != 4 {
		t.Fatalf("a step that has not settled must run every pass; got %d calls in 4 passes", hv.replicaCalls)
	}
}

// A pass that CHANGED something has not settled either — the next pass confirms
// the change took.
func TestAChangeMeansItHasNotSettled(t *testing.T) {
	r, hv := newCountingReconciler()
	hv.replicaOut = hyperv.OutcomeUpdated
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), replicaHost(1), nil); err != nil {
			t.Fatal(err)
		}
	}
	if hv.replicaCalls != 3 {
		t.Fatalf("a step that keeps changing things must keep running; got %d calls in 3 passes", hv.replicaCalls)
	}
}

// An operator edit can change what the step must write, so a new generation
// reads for real however settled it was.
func TestAnEditPutsTheReplicaStepBackOnEveryPass(t *testing.T) {
	r, hv := newCountingReconciler()
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, replicaHost(1), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, replicaHost(2), nil); err != nil {
		t.Fatal(err)
	}
	if hv.replicaCalls != 2 {
		t.Fatalf("a generation change must be honoured at once; got %d calls", hv.replicaCalls)
	}
}

func TestAnInstalledHyperVRoleIsNotReReadEveryPass(t *testing.T) {
	r, hv := newCountingReconciler()
	for i := 0; i < 10; i++ {
		if _, err := r.Reconcile(context.Background(), replicaHost(1), nil); err != nil {
			t.Fatal(err)
		}
	}
	if hv.roleCalls != 1 {
		t.Fatalf("an installed role cannot uninstall itself; %d Get-WindowsFeature reads in 10 passes", hv.roleCalls)
	}
}

// While the role is absent the answer is live and the whole reconcile is waiting
// on it, so it is read every pass — the converging case, as with the broker.
func TestAnAbsentHyperVRoleIsReadEveryPass(t *testing.T) {
	hv := &countingReplicaHV{Stub: &hyperv.Stub{HyperVInstalled: false}, replicaOut: hyperv.OutcomeUnchanged}
	r := New(hv, nil)
	host := replicaHost(1)
	host.Spec.EnableHyperVRole = true
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), host, nil); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if hv.roleCalls != 3 {
		t.Fatalf("a host still waiting for its role must be read every pass; got %d calls in 3 passes", hv.roleCalls)
	}
}

/* Management vNICs, the third step to get the same treatment — and the one the
   fleet was paying most for: queryVNICsBatch was 7.3-8.6s of every pass on every
   host, about a quarter of a 32s pass.

   It is already batched into one invocation. The recorded cold-versus-warm
   figures from that work say why batching further would not help: Get-NetIPAddress
   654ms then 24ms in the same process. The first call into each module is the
   cost and one invocation pays it once, so hoisting the last per-vNIC cmdlets
   would save a third of a second out of seven. Not asking as often is the lever
   that remains. */

func vnicHost(gen int64, vnics ...string) types.Host {
	specs := make([]types.ManagementVNICSpec, 0, len(vnics))
	for _, n := range vnics {
		specs = append(specs, types.ManagementVNICSpec{Name: n, SwitchName: "ConvergedSwitch"})
	}
	return types.Host{
		Meta: types.ObjectMeta{Name: "HVNEW01", Generation: gen},
		Spec: types.HostSpec{
			EnableHyperVRole: true,
			Networking: types.HostNetworkingSpec{
				Switches:        []types.VirtualSwitchSpec{{Name: "ConvergedSwitch", TeamMembers: []string{"NIC1"}}},
				ManagementVNICs: specs,
			},
		},
	}
}

type countingVNICHV struct {
	*hyperv.Stub
	observes int
}

func (c *countingVNICHV) EnsureMgmtVNICs(ctx context.Context, s []types.ManagementVNICSpec) ([]hyperv.Outcome, []error) {
	c.observes++
	return c.Stub.EnsureMgmtVNICs(ctx, s)
}

func TestSettledManagementVNICsAreNotReObservedEveryPass(t *testing.T) {
	hv := &countingVNICHV{Stub: &hyperv.Stub{HyperVInstalled: true}}
	r := New(hv, nil)
	host := vnicHost(1, "Management", "Storage", "LiveMigration")

	for i := 0; i < mgmtVNICEvery; i++ {
		if _, err := r.Reconcile(context.Background(), host, nil); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	// Two, and both are earned: the first pass CREATES the vNICs, so it changed
	// something and has not settled; the second finds nothing to do and settles.
	// A step is only throttled once it has reported a pass that changed nothing,
	// which is the whole rule — a set still converging keeps every pass.
	if hv.observes != 2 {
		t.Fatalf("want one creating pass and one confirming pass, then silence; got %d observations in %d passes", hv.observes, mgmtVNICEvery)
	}
}

// The steps must keep their place in the console on the passes they are skipped,
// or a vNIC reads as one that stopped being reconciled.
func TestSkippedVNICsStillReportTheirState(t *testing.T) {
	hv := &countingVNICHV{Stub: &hyperv.Stub{HyperVInstalled: true}}
	r := New(hv, nil)
	host := vnicHost(1, "Management", "Storage")
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, host, nil); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(ctx, host, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range res.Conditions {
		seen[c.Type] = true
	}
	for _, want := range []string{"ManagementVNIC/Management", "ManagementVNIC/Storage"} {
		if !seen[want] {
			t.Errorf("%s lost its place in the console on a skipped pass", want)
		}
	}
}

// An operator edit is the main way these change, so a new generation reads for
// real however settled the set was.
func TestAnEditReObservesTheVNICsAtOnce(t *testing.T) {
	hv := &countingVNICHV{Stub: &hyperv.Stub{HyperVInstalled: true}}
	r := New(hv, nil)
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, vnicHost(1, "Management"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, vnicHost(2, "Management"), nil); err != nil {
		t.Fatal(err)
	}
	if hv.observes != 2 {
		t.Fatalf("an edit must be honoured at once; got %d observations", hv.observes)
	}
}
