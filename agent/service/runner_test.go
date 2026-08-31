package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/agent/reconcile"
	"github.com/joshua-fourie/ballast/agent/store"
	ballastpb "github.com/joshua-fourie/ballast/api/proto"
	"github.com/joshua-fourie/ballast/api/types"
)

// fakeClient is a programmable AgentServiceClient. errAll makes every RPC fail,
// simulating an unreachable centre.
type fakeClient struct {
	errAll bool

	desired *ballastpb.Host // returned by PullDesiredState when set

	registerCalls int
	reportCalls   int
	reported      []*ballastpb.HostStatus
}

var errUnreachable = errors.New("centre unreachable")

func (f *fakeClient) RegisterHost(_ context.Context, _ *ballastpb.RegisterHostRequest, _ ...grpc.CallOption) (*ballastpb.RegisterHostResponse, error) {
	f.registerCalls++
	if f.errAll {
		return nil, errUnreachable
	}
	return &ballastpb.RegisterHostResponse{Uid: "u-centre", Known: f.desired != nil}, nil
}

func (f *fakeClient) PullDesiredState(_ context.Context, _ *ballastpb.PullDesiredStateRequest, _ ...grpc.CallOption) (*ballastpb.PullDesiredStateResponse, error) {
	if f.errAll {
		return nil, errUnreachable
	}
	if f.desired == nil {
		return &ballastpb.PullDesiredStateResponse{HasDesiredState: false}, nil
	}
	return &ballastpb.PullDesiredStateResponse{HasDesiredState: true, Host: f.desired}, nil
}

func (f *fakeClient) ReportStatus(_ context.Context, in *ballastpb.ReportStatusRequest, _ ...grpc.CallOption) (*ballastpb.ReportStatusResponse, error) {
	f.reportCalls++
	if f.errAll {
		return nil, errUnreachable
	}
	f.reported = append(f.reported, in.GetStatus())
	return &ballastpb.ReportStatusResponse{Accepted: true}, nil
}

func (f *fakeClient) ReportJobResult(_ context.Context, _ *ballastpb.ReportJobResultRequest, _ ...grpc.CallOption) (*ballastpb.ReportJobResultResponse, error) {
	if f.errAll {
		return nil, errUnreachable
	}
	return &ballastpb.ReportJobResultResponse{Accepted: true}, nil
}

func newTestRunner(t *testing.T) *runner {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hv := &hyperv.Stub{}
	return &runner{
		cfg:        runnerConfig{hostName: "host01"},
		log:        log,
		hv:         hv,
		st:         st,
		reconciler: reconcile.New(hv, log),
	}
}

// When the centre is unreachable, a cycle must still complete: it journals an
// Autonomous status locally and leaves it queued for replay. It must not block.
func TestCycleAutonomousWhenCentreDown(t *testing.T) {
	r := newTestRunner(t)
	fc := &fakeClient{errAll: true}

	r.cycle(context.Background(), fc)

	if r.registered {
		t.Fatal("must not be marked registered when registration failed")
	}
	queued, err := r.st.Undelivered()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("want 1 queued status, got %d", len(queued))
	}
	if !queued[0].Status.Autonomous {
		t.Fatal("queued status should be marked Autonomous when centre is down")
	}
}

// On boot the agent adopts the UID from cached desired state, so it has an
// identity to report under before it can reach the centre.
func TestAdoptCachedStateRecoversIdentity(t *testing.T) {
	r := newTestRunner(t)
	cached := types.Host{
		Meta: types.ObjectMeta{Name: "host01", UID: "u-cached", Generation: 5},
		Spec: types.HostSpec{FQDN: "host01.lab.local", RebootPolicy: types.RebootNever},
	}
	if err := r.st.SaveDesiredHost(cached); err != nil {
		t.Fatal(err)
	}

	r.adoptCachedState()

	if r.uid != "u-cached" {
		t.Fatalf("want adopted uid u-cached, got %q", r.uid)
	}
}

// Recovery path: a cycle that fails to reach the centre queues a status; a
// later successful cycle registers, caches desired state, and drains the queue.
func TestQueuedStatusDrainsOnReconnect(t *testing.T) {
	r := newTestRunner(t)

	// Cycle 1: centre down. One autonomous status queued.
	down := &fakeClient{errAll: true}
	r.cycle(context.Background(), down)
	if q, _ := r.st.Undelivered(); len(q) != 1 {
		t.Fatalf("after outage want 1 queued, got %d", len(q))
	}

	// Cycle 2: centre back, serving desired state.
	up := &fakeClient{desired: ballastpb.HostToProto(types.Host{
		Meta: types.ObjectMeta{Name: "host01", UID: "u-centre", Generation: 2},
		Spec: types.HostSpec{FQDN: "host01.lab.local", RebootPolicy: types.RebootNever},
	})}
	r.cycle(context.Background(), up)

	if !r.registered {
		t.Fatal("expected registration to succeed on reconnect")
	}
	if up.registerCalls != 1 {
		t.Fatalf("want exactly 1 register call, got %d", up.registerCalls)
	}
	// Desired state cached.
	if h, ok, _ := r.st.LoadDesiredHost(); !ok || h.Meta.Generation != 2 {
		t.Fatalf("desired state not cached after reconnect: ok=%v gen=%d", ok, h.Meta.Generation)
	}
	// Both the queued outage status and the fresh one delivered; none left.
	if q, _ := r.st.Undelivered(); len(q) != 0 {
		t.Fatalf("want queue drained, got %d remaining", len(q))
	}
	if up.reportCalls != 2 {
		t.Fatalf("want 2 statuses delivered on reconnect (queued + current), got %d", up.reportCalls)
	}
}

// Once registered, the agent does not re-register every cycle.
func TestNoReRegisterOnceRegistered(t *testing.T) {
	r := newTestRunner(t)
	up := &fakeClient{}
	r.cycle(context.Background(), up)
	r.cycle(context.Background(), up)
	if up.registerCalls != 1 {
		t.Fatalf("want 1 register call across two cycles, got %d", up.registerCalls)
	}
}

// A job that holds a VM's files must stop the reconcile loop touching that VM.
// Hyper-V refuses Set-VM during a storage migration, so reconciling regardless
// reported ApplyFailed every cycle and marked the VM Degraded for the whole of an
// operation that was succeeding.
func TestJobHoldsVM(t *testing.T) {
	holds := []string{
		types.JobVMMoveStorage, types.JobMigrateVM, types.JobClusterMoveVM,
		types.JobVMClone, types.JobVMCaptureTemplate, types.JobVMExport,
		types.JobVMDiscardSavedState,
		// A delete holds the VM for the opposite reason to the rest: it is
		// destroying the VM, and a pass that reconciles alongside it recreates
		// exactly what the job has just removed.
		types.JobRemoveVM,
	}
	for _, k := range holds {
		if !jobHoldsVM(k) {
			t.Fatalf("%s takes hold of the VM's files and must stand the reconciler off", k)
		}
	}
	// A power action or a guest command runs happily alongside a reconcile;
	// standing off for those would delay settling for no reason.
	for _, k := range []string{types.JobVMStart, types.JobVMStop, types.JobGuestSetIP, types.JobResync} {
		if jobHoldsVM(k) {
			t.Fatalf("%s does not hold the VM's files; standing off would only delay settling", k)
		}
	}
}

// The claim is per VM name and case-insensitive, and it is released when the job
// finishes — a VM left marked busy would never reconcile again.
func TestVMBusyClaimAndRelease(t *testing.T) {
	r := &runner{jobsVMs: map[string]string{"windows": types.JobVMMoveStorage}}
	if kind, busy := r.vmBusy("Windows"); !busy || kind != types.JobVMMoveStorage {
		t.Fatalf("busy lookup must be case-insensitive: kind=%q busy=%v", kind, busy)
	}
	if _, busy := r.vmBusy("Linux"); busy {
		t.Fatal("an unrelated VM must not be held")
	}
	delete(r.jobsVMs, "windows")
	if _, busy := r.vmBusy("Windows"); busy {
		t.Fatal("the claim must be released when the job finishes")
	}
}

/*
A VMware copy pass gets hours, not the default ten minutes.

	The default would kill a base copy in its first pass, dismount the disk
	mid-write, and the retry would start again from nothing — for ever, on any
	VM big enough to matter.
*/
func TestACopyPassIsNotHeldToTheDefaultBudget(t *testing.T) {
	pass := jobTimeoutFor(types.Job{Kind: types.JobMigrationPass})
	if pass <= jobTimeout {
		t.Errorf("a copy pass is allowed %v, no more than the %v default", pass, jobTimeout)
	}
	if pass < 4*time.Hour {
		t.Errorf("a copy pass is allowed %v, which would cut a large base copy short", pass)
	}
	// Cleanup consolidates a snapshot on somebody else's datastore, which is
	// real I/O and must not be abandoned part-way.
	if c := jobTimeoutFor(types.Job{Kind: types.JobMigrationCleanup}); c <= jobTimeout {
		t.Errorf("snapshot cleanup is allowed only %v", c)
	}
}
