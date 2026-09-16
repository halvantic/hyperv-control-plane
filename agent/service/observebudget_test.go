package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* A cached observation that hangs cost the whole pass.

   Metrics, inventory and resources are best-effort reads whose values are
   already carried forward between refreshes, so abandoning one costs a stale
   reading. They were handed the whole cycle context, so NOT abandoning one cost
   the entire cycle: on bcluster2 2026-08-14, with the cluster's Health Service
   down and the pool degraded, CollectInventory took 4m58s of a 5m0s cycle on
   HVNEW02 and 2m44s on HVNEW01. Both cycles were killed at the cap before
   reaching reportStatus, so their readings froze at fifteen minutes old and
   every step after the stall reported NotAttempted.

   It looks host-local and is not: CollectInventory runs Get-PhysicalDisk, which
   on an S2D node enumerates every disk in the CLUSTER. One sick storage
   subsystem blinded the centre to three whole hosts. */

func TestAHangingObservationIsAbandonedNotWaitedOut(t *testing.T) {
	start := time.Now()
	_, err := bounded(context.Background(), 50*time.Millisecond,
		func(ctx context.Context) (types.HostInventory, error) {
			<-ctx.Done() // never answers, like a storage query on a sick cluster
			return types.HostInventory{}, ctx.Err()
		})
	if err == nil {
		t.Fatal("a read that never answers must return an error, not hang the pass")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("it must give up at its budget, not run to the cycle cap; took %s", time.Since(start))
	}
}

// The budget is per observation, so a healthy read is untouched.
func TestAHealthyObservationIsUnaffected(t *testing.T) {
	got, err := bounded(context.Background(), time.Minute,
		func(context.Context) (types.HostMetrics, error) {
			return types.HostMetrics{CPUUsagePercent: 42}, nil
		})
	if err != nil || got.CPUUsagePercent != 42 {
		t.Fatalf("want the reading through unchanged, got %+v err=%v", got, err)
	}
}

// An error from the read itself is passed through — the bound is about time, and
// must not turn a real failure into a silent success.
func TestARealFailureStillSurfaces(t *testing.T) {
	_, err := bounded(context.Background(), time.Minute,
		func(context.Context) (types.HostMetrics, error) {
			return types.HostMetrics{}, errors.New("Get-Counter refused")
		})
	if err == nil || err.Error() != "Get-Counter refused" {
		t.Fatalf("the read's own error must survive: %v", err)
	}
}

/* And what a host reports when a collection is abandoned.

   Inventory and resources already keep their cached value on failure. Metrics
   did not: it kept the zero value, so a host whose storage subsystem was hanging
   would report 0% CPU and 0 bytes of memory in use — stating the opposite of
   what is known, at exactly the moment someone is looking at those numbers.
   Absent is not zero. */

func TestFailedMetricsKeepTheLastRealReading(t *testing.T) {
	r, _ := newDrainCountingRunner(t)
	r.lastMetrics = types.HostMetrics{CPUUsagePercent: 63, MemoryInUseBytes: 90 << 30}

	// What the cycle does on a failed collection.
	metrics := types.HostMetrics{}
	if err := errors.New("timed out"); err != nil {
		metrics = r.lastMetrics
	}

	if metrics.CPUUsagePercent != 63 || metrics.MemoryInUseBytes != 90<<30 {
		t.Fatalf("a failed collection must report the last real reading, not zeros: %+v", metrics)
	}
}
