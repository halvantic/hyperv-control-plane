package reconcile

import (
	"context"
	"testing"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

/* The expensive per-VM read (guest OS via KVP/XML, checkpoints, a Get-VHD per
   disk, a VLAN query per adapter) is ~1.4s per VM and ran every pass. Almost
   nothing it returns can change while the VM keeps running: processor count,
   memory config, generation, disks and adapters settle on a power cycle or an
   explicit job.

   These pin when it is spent and when it is skipped — and, more importantly,
   that skipping it never means reporting less. */

// observeCountingStub records the expensive read and can move a VM's power
// state underneath the reconciler.
type observeCountingStub struct {
	*hyperv.Stub
	fullReads map[string]int
	liveCalls int
	power     types.VMPowerState
}

func newObserveStub() *observeCountingStub {
	return &observeCountingStub{Stub: &hyperv.Stub{}, fullReads: map[string]int{}, power: types.VMPowerRunning}
}

func (o *observeCountingStub) GetVMState(ctx context.Context, name string) (hyperv.VMState, error) {
	o.fullReads[name]++
	return hyperv.VMState{
		Exists: true, ID: "id-" + name, PowerState: o.power,
		GuestOS:     "Windows Server 2022",
		Checkpoints: []types.VMCheckpoint{{Name: "cp1"}},
		Observed:    &types.VMObserved{ProcessorCount: 4, Generation: 2},
	}, nil
}

func (o *observeCountingStub) GetVMLiveStates(_ context.Context, names []string) (map[string]hyperv.VMLive, error) {
	o.liveCalls++
	out := make(map[string]hyperv.VMLive, len(names))
	for _, n := range names {
		out[lower(n)] = hyperv.VMLive{
			Exists: true, ID: "id-" + n, PowerState: o.power,
			CPUUsagePercent: 12, UptimeSeconds: 60, IPAddress: "192.168.1.90",
		}
	}
	return out, nil
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}

func oneVM() []types.VM {
	return []types.VM{{
		Meta: types.ObjectMeta{Name: "Windows", Generation: 1},
		Spec: types.VMSpec{Placement: types.VMPlacementSpec{HostName: "host01"}},
	}}
}

// A settled VM, nothing changed: the cheap read runs, the expensive one does not.
func TestSteadyPassSkipsTheExpensiveRead(t *testing.T) {
	stub := newObserveStub()
	r := testReconciler(stub)
	ctx := context.Background()

	r.ReconcileVMs(ctx, oneVM(), true) // first pass primes the cache
	before := stub.fullReads["Windows"]

	for i := 0; i < 5; i++ {
		r.ReconcileVMs(ctx, oneVM(), false)
	}

	if got := stub.fullReads["Windows"] - before; got != 0 {
		t.Fatalf("a settled VM must not be fully re-read: %d extra reads over 5 passes", got)
	}
	if stub.liveCalls != 6 {
		t.Fatalf("the cheap live read must still run every pass, got %d", stub.liveCalls)
	}
}

// Skipping the read must not mean reporting less — that is what blanked the
// console's config card in the first place.
func TestASkippedReadStillReportsCompleteStatus(t *testing.T) {
	stub := newObserveStub()
	r := testReconciler(stub)
	ctx := context.Background()

	r.ReconcileVMs(ctx, oneVM(), true)
	res := r.ReconcileVMs(ctx, oneVM(), false)

	got := res[0]
	if got.Observed == nil || got.Observed.ProcessorCount != 4 {
		t.Errorf("config must come from the cached read, got %+v", got.Observed)
	}
	if got.GuestOS != "Windows Server 2022" || len(got.Checkpoints) != 1 || got.VMID == "" {
		t.Errorf("guest details, checkpoints and identity must survive a skipped read, got %+v", got)
	}
	// And the live fields must be the fresh ones, not the cached zeroes.
	if got.CPUUsagePercent != 12 || got.UptimeSeconds != 60 || got.IPAddress != "192.168.1.90" {
		t.Errorf("live metrics must come from the live read, got cpu=%d up=%d ip=%q",
			got.CPUUsagePercent, got.UptimeSeconds, got.IPAddress)
	}
}

// A power cycle is when deferred configuration changes actually land, so it must
// trigger a fresh read without anyone asking.
func TestAPowerStateChangeTriggersAFreshRead(t *testing.T) {
	stub := newObserveStub()
	r := testReconciler(stub)
	ctx := context.Background()

	r.ReconcileVMs(ctx, oneVM(), true)
	r.ReconcileVMs(ctx, oneVM(), false)
	before := stub.fullReads["Windows"]

	stub.power = types.VMPowerOff
	r.ReconcileVMs(ctx, oneVM(), false)

	if got := stub.fullReads["Windows"] - before; got != 1 {
		t.Fatalf("a power-state change must force one fresh read, got %d", got)
	}
}

// The drift sweep is the safety net for an edit made outside Ballast to a VM
// nobody reboots. Without it that drift is invisible for ever.
func TestTheSweepForcesAReadWithNothingElseChanging(t *testing.T) {
	stub := newObserveStub()
	r := testReconciler(stub)
	ctx := context.Background()

	r.ReconcileVMs(ctx, oneVM(), true)
	r.ReconcileVMs(ctx, oneVM(), false)
	before := stub.fullReads["Windows"]

	r.ReconcileVMs(ctx, oneVM(), true) // the sweep

	if got := stub.fullReads["Windows"] - before; got != 1 {
		t.Fatalf("the sweep must re-read regardless of what changed, got %d", got)
	}
}

// Nothing cached yet — merging over nothing would report a VM with no
// configuration at all.
func TestFirstPassReadsFullyEvenWithoutASweep(t *testing.T) {
	stub := newObserveStub()
	r := testReconciler(stub)

	r.ReconcileVMs(context.Background(), oneVM(), false)

	if stub.fullReads["Windows"] != 1 {
		t.Fatalf("with nothing cached the VM must be read fully, got %d", stub.fullReads["Windows"])
	}
}

// If the batched live read fails, every VM falls back to reading itself — the
// behaviour before the batch existed.
func TestAFailedLiveReadFallsBackToFullReads(t *testing.T) {
	stub := &failingLiveStub{observeCountingStub: newObserveStub()}
	r := testReconciler(stub)

	r.ReconcileVMs(context.Background(), oneVM(), false)
	r.ReconcileVMs(context.Background(), oneVM(), false)

	if stub.fullReads["Windows"] != 2 {
		t.Fatalf("without a live reading each pass must read fully, got %d", stub.fullReads["Windows"])
	}
}

type failingLiveStub struct{ *observeCountingStub }

func (f *failingLiveStub) GetVMLiveStates(context.Context, []string) (map[string]hyperv.VMLive, error) {
	return nil, context.DeadlineExceeded
}

// A pass that changed the VM must re-read it: the cached configuration describes
// the state before the change.
func TestAChangedVMIsReReadNotReportedFromCache(t *testing.T) {
	stub := newObserveStub()
	r := testReconciler(stub)
	ctx := context.Background()

	r.ReconcileVMs(ctx, oneVM(), true)
	before := stub.fullReads["Windows"]

	// A spec change makes EnsureVM report Updated on the next pass.
	vms := oneVM()
	vms[0].Spec.ProcessorCount = 8
	r.ReconcileVMs(ctx, vms, false)

	if got := stub.fullReads["Windows"] - before; got != 1 {
		t.Fatalf("a VM we just changed must be re-read, got %d", got)
	}
}
