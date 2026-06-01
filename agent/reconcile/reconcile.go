// Package reconcile drives actual host state towards the cached desired Host.
//
// It is the agent-side embodiment of the kubelet pattern from CLAUDE.md: it
// compares desired to actual and emits idempotent ensure-operations to close
// the gap, holding no host knowledge of its own — every host action goes
// through hyperv.Interface. Running it repeatedly with the same desired state
// converges and then no-ops, which is what lets the agent keep enforcing cached
// intent on a tight loop, including while the centre is offline.
//
// For this slice it reconciles networking: SET-backed virtual switches and the
// management OS vNICs layered on them. The host role, storage and cluster
// reconcilers follow the same shape.
package reconcile

import (
	"context"
	"fmt"
	"time"

	"log/slog"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

// Reconciler converges a host towards desired state via hyperv.Interface.
type Reconciler struct {
	hv  hyperv.Interface
	log *slog.Logger

	// now is injectable so tests can pin condition timestamps.
	now func() time.Time
}

// New returns a Reconciler driving the given host interface.
func New(hv hyperv.Interface, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{hv: hv, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// Result is the outcome of one reconcile pass, for the agent to fold into the
// status it reports.
type Result struct {
	// Phase is Ready when every piece of desired state was honoured, Degraded
	// when at least one ensure-operation failed.
	Phase types.Phase

	// Honoured is true only when the whole desired spec was applied successfully.
	// The agent advances ObservedGeneration to the desired Generation only then;
	// a failed pass must not claim intent it did not achieve.
	Honoured bool

	// Changed is true when this pass actually mutated host state (a create or
	// update). A converged host produces Changed == false, the steady state.
	Changed bool

	// Conditions records one machine-readable fact per reconciled resource.
	Conditions []types.Condition
}

// Reconcile makes the host match desired and returns what it observed. Switches
// are ensured before the management vNICs that depend on them. It does not stop
// at the first failure: it attempts every resource so status reflects the whole
// host, and reports Honoured == false if any failed.
func (r *Reconciler) Reconcile(ctx context.Context, desired types.Host) (Result, error) {
	var (
		conds    []types.Condition
		changed  bool
		failures int
		firstErr error
	)

	net := desired.Spec.Networking

	for _, sw := range net.Switches {
		out, err := r.hv.EnsureSwitch(ctx, sw)
		conds = append(conds, r.condition("Switch/"+sw.Name, out, err))
		if err != nil {
			failures++
			if firstErr == nil {
				firstErr = fmt.Errorf("ensure switch %q: %w", sw.Name, err)
			}
			r.log.Error("ensure switch failed", "switch", sw.Name, "err", err)
			continue
		}
		if out != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("switch reconciled", "switch", sw.Name, "outcome", out)
		}
	}

	for _, v := range net.ManagementVNICs {
		out, err := r.hv.EnsureMgmtVNIC(ctx, v)
		conds = append(conds, r.condition("ManagementVNIC/"+v.Name, out, err))
		if err != nil {
			failures++
			if firstErr == nil {
				firstErr = fmt.Errorf("ensure vNIC %q: %w", v.Name, err)
			}
			r.log.Error("ensure management vNIC failed", "vnic", v.Name, "err", err)
			continue
		}
		if out != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("management vNIC reconciled", "vnic", v.Name, "outcome", out)
		}
	}

	res := Result{
		Conditions: conds,
		Changed:    changed,
		Honoured:   failures == 0,
	}
	if failures == 0 {
		res.Phase = types.PhaseReady
	} else {
		res.Phase = types.PhaseDegraded
	}
	return res, firstErr
}

// condition turns an ensure outcome into a status Condition. condType is the
// stable per-resource key (e.g. "Switch/ConvergedSwitch").
func (r *Reconciler) condition(condType string, out hyperv.Outcome, err error) types.Condition {
	c := types.Condition{
		Type:               condType,
		LastTransitionTime: r.now(),
	}
	if err != nil {
		c.Status = false
		c.Reason = "ApplyFailed"
		c.Message = err.Error()
		return c
	}
	c.Status = true
	switch out {
	case hyperv.OutcomeCreated:
		c.Reason = "Created"
		c.Message = "created to match desired state"
	case hyperv.OutcomeUpdated:
		c.Reason = "Updated"
		c.Message = "adjusted to match desired state"
	default:
		c.Reason = "AlreadyConfigured"
		c.Message = "already matches desired state"
	}
	return c
}
