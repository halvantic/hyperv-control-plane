package reconcile

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* One slow step must not decide whether the rest of the pass runs.

   Several iSCSI cmdlets hang for minutes against a portal that accepts a TCP
   connection but never completes the operation, and the reachability probe
   cannot tell those apart. On Secondary, 2026-08-25, iSCSI took 3m43s of a 5m
   pass on both members: the pass was cut off before the cluster was formed, so
   a cluster that needs no storage to exist could not come up because its storage
   was slow. The console showed no cluster at all rather than a cluster with a
   storage problem.

   Formation does not depend on iSCSI — only CSV adoption does. */

// blockingISCSI is a host whose iSCSI call never returns until cancelled, which
// is what a hung cmdlet looks like from here.
type blockingISCSI struct {
	*hyperv.Stub
	started chan struct{}
}

func (b *blockingISCSI) EnsureISCSI(ctx context.Context, _ types.ISCSIStorageSpec, _, _ string, _ bool, _ []string) (hyperv.ISCSIState, hyperv.Outcome, error) {
	close(b.started)
	<-ctx.Done()
	return hyperv.ISCSIState{}, hyperv.OutcomeUnchanged, ctx.Err()
}

func blockedCluster() ClusterAssignment {
	var c types.Cluster
	c.Meta.Name = "Secondary"
	c.Spec.Members = []string{"HVNEW04", "HVNEW05"}
	c.Spec.Storage = &types.ClusterStorageSpec{
		Kind:  types.StorageKindISCSI,
		ISCSI: &types.ISCSIStorageSpec{Portals: []string{"10.0.60.52", "10.0.61.52"}},
	}
	return ClusterAssignment{IsMember: true, Cluster: c}
}

func TestABlockingISCSIStepGivesUpSoThePassCanContinue(t *testing.T) {
	hv := &blockingISCSI{Stub: &hyperv.Stub{}, started: make(chan struct{})}
	r := New(hv, nil)
	r.iscsiReadBudget = 80 * time.Millisecond

	// The PASS has plenty of time left; it is the STEP that is slow.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	_, conds, _ := r.reconcileISCSI(ctx, blockedCluster(), nil)
	took := time.Since(start)

	if took > 3*time.Second {
		t.Fatalf("the step must give up on its own budget, not run until the pass dies (took %s)", took)
	}
	if ctx.Err() != nil {
		t.Fatal("the pass's own context must survive — that is the whole point")
	}
	if len(conds) == 0 {
		t.Fatal("giving up must be reported, not silent")
	}
	msg := conds[0].Message
	// Named as the step giving up, not as the host failing: nothing has been
	// established about the array.
	if !strings.Contains(msg, "did not finish within") {
		t.Errorf("it must say the step was stopped: %q", msg)
	}
	if !strings.Contains(msg, "blocking rather than failing") {
		t.Errorf("it must distinguish a hang from a refusal: %q", msg)
	}
	// And say what still works, or an operator reads it as the cluster being down.
	if !strings.Contains(msg, "does not need the array to form") {
		t.Errorf("it must say formation is unaffected: %q", msg)
	}
}

// A pass that is itself ending must not be reported as the step timing out —
// that would blame iSCSI for a cycle that ran out elsewhere.
func TestAPassEndingIsNotBlamedOnTheISCSIStep(t *testing.T) {
	hv := &blockingISCSI{Stub: &hyperv.Stub{}, started: make(chan struct{})}
	r := New(hv, nil)
	r.iscsiReadBudget = 10 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, conds, _ := r.reconcileISCSI(ctx, blockedCluster(), nil)

	for _, c := range conds {
		if strings.Contains(c.Message, "did not finish within") {
			t.Errorf("the pass ran out, not the step — it must not be blamed here: %q", c.Message)
		}
	}
}
