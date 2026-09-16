package reconcile

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
)

/* A node falling out of its cluster cost every reading that host had.

   Cluster cmdlets do not fail fast against a member the cluster considers Down —
   they hang until RPC gives up. On HVNEW02, 2026-08-16, GetClusterState took
   2m6s while that node was Down; on HVNEW03 during the August incident, 4m31s.
   Against a five-minute cycle that starved everything after it, so the host's
   metrics, inventory, VM state and desired-state changes all stopped, not merely
   its cluster reading.

   The cluster OPERATIONS are deliberately left unbounded: New-Cluster,
   Enable-ClusterS2D and volume creation take minutes on purpose, and cutting one
   off mid-sequence is the harm cycleTimeout's comment warns about. Only the READ
   is bounded, because failing a read costs a deferred pass and nothing else. */

// hangingClusterHV never answers a cluster read, like a node that is Down.
type hangingClusterHV struct {
	*hyperv.Stub
}

func (h *hangingClusterHV) GetClusterState(ctx context.Context) (hyperv.ClusterState, error) {
	<-ctx.Done()
	return hyperv.ClusterState{}, ctx.Err()
}

func TestAHangingClusterReadIsAbandonedNotWaitedOut(t *testing.T) {
	r := New(&hangingClusterHV{Stub: &hyperv.Stub{}}, nil)
	r.clusterReadBudget = 50 * time.Millisecond
	// A cycle-sized budget: the read must give up on its OWN deadline, well
	// inside it, rather than running until the cycle is cut off.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	start := time.Now()
	_, err := r.clusterState(ctx)
	took := time.Since(start)

	if err == nil {
		t.Fatal("a read that never answers must return an error")
	}
	if took >= time.Second {
		t.Fatalf("it must give up at its own budget, not run to the cycle cap; took %s", took)
	}
}

// "context deadline exceeded" names the mechanism. What an operator needs is
// what it MEANS and where to look.
func TestATimedOutClusterReadSaysWhatItMeans(t *testing.T) {
	r := New(&hangingClusterHV{Stub: &hyperv.Stub{}}, nil)
	r.clusterReadBudget = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	_, err := r.clusterState(ctx)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{"did not answer", "considers Down", "Get-ClusterNode", "deferred"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message must explain the cause and the next step (%q): %q", want, msg)
		}
	}
	if strings.Contains(msg, "context deadline exceeded") {
		t.Errorf("a bare context error names the mechanism, not the problem: %q", msg)
	}
}

// A cancelled CYCLE is not the read's fault and must not be dressed up as one —
// that would blame the cluster for the agent stopping.
func TestACancelledCycleIsNotReportedAsASlowCluster(t *testing.T) {
	r := New(&hangingClusterHV{Stub: &hyperv.Stub{}}, nil)
	r.clusterReadBudget = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := r.clusterState(ctx)
	if err != nil && strings.Contains(err.Error(), "did not answer within") {
		t.Errorf("the cycle was cancelled, not the cluster slow: %q", err.Error())
	}
}

// A healthy read is untouched — the budget is a backstop, not a throttle.
func TestAHealthyClusterReadPassesStraightThrough(t *testing.T) {
	r := New(&hyperv.Stub{}, nil)
	if _, err := r.clusterState(context.Background()); err != nil {
		t.Fatalf("a healthy read must pass through unchanged: %v", err)
	}
}
