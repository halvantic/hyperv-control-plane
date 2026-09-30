package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/agent/reconcile"
	"github.com/halvantic/hyperv-control-plane/agent/store"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

// sessionCountingHV records the sessions a cycle opens and whether the VM
// inventory ran inside one.
type sessionCountingHV struct {
	*hyperv.Stub
	opened, closed  int
	observedInsideA bool
}

type cycleSessionKey struct{}

func (s *sessionCountingHV) Session(ctx context.Context) (context.Context, func()) {
	if ctx.Value(cycleSessionKey{}) != nil {
		return ctx, func() {} // joined, as the real one does
	}
	s.opened++
	return context.WithValue(ctx, cycleSessionKey{}, true), func() { s.closed++ }
}

func (s *sessionCountingHV) ListObservedVMs(ctx context.Context) ([]types.ObservedVM, error) {
	s.observedInsideA = ctx.Value(cycleSessionKey{}) != nil
	return s.Stub.ListObservedVMs(ctx)
}

// One session per cycle, ended with it, and the cycle's PowerShell reads made
// inside it: the Hyper-V module and each cmdlet warm up once a cycle.
func TestEachCycleOpensOneSessionAndEndsIt(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hv := &sessionCountingHV{Stub: &hyperv.Stub{}}
	r := &runner{cfg: runnerConfig{hostName: "host01"}, log: log, hv: hv, st: st, reconciler: reconcile.New(hv, log)}
	fc := &fakeClient{errAll: true}

	for i := 1; i <= 3; i++ {
		r.cycle(context.Background(), fc)
		if hv.opened != i || hv.closed != i {
			t.Fatalf("after %d cycles: opened %d, closed %d", i, hv.opened, hv.closed)
		}
	}
	if !hv.observedInsideA {
		t.Fatal("the VM inventory must run inside the cycle's session")
	}
}
