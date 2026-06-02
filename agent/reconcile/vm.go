package reconcile

import (
	"context"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

// VMResult is the outcome of reconciling one VM, for the agent to fold into the
// per-VM status it reports.
type VMResult struct {
	// Name is the VM this result is for.
	Name string

	Phase types.Phase

	// Honoured is true only when the VM's configuration and power state were both
	// applied successfully; the agent advances the VM's ObservedGeneration only
	// then.
	Honoured bool

	// Changed is true when this pass mutated the VM (created, reconfigured, or
	// changed power state).
	Changed bool

	// PowerState is the observed power state after reconciling.
	PowerState types.VMPowerState

	// AssignedMemoryBytes / CPUUsagePercent / UptimeSeconds are best-effort
	// observed runtime metrics.
	AssignedMemoryBytes uint64
	CPUUsagePercent     int
	UptimeSeconds       int64

	Conditions []types.Condition
}

// ReconcileVM drives one VM towards its desired state: ensure configuration
// (processor count, memory, disks, adapters), then drive power to the desired
// state, then observe the result. Idempotent — a settled VM produces no changes.
//
// It runs against the cached VM spec, so the agent keeps enforcing the last
// intent it was given even while the centre is offline, exactly as for the host.
func (r *Reconciler) ReconcileVM(ctx context.Context, vm types.VM) VMResult {
	res := VMResult{Name: vm.Meta.Name}

	out, err := r.hv.EnsureVM(ctx, vm)
	res.Conditions = append(res.Conditions, r.condition("VM/"+vm.Meta.Name, out, err))
	if err != nil {
		r.log.Error("ensure vm failed", "vm", vm.Meta.Name, "err", err)
		res.Phase = types.PhaseDegraded
		return res
	}
	if out != hyperv.OutcomeUnchanged {
		res.Changed = true
		r.log.Info("vm reconciled", "vm", vm.Meta.Name, "outcome", out)
	}

	// Drive power to the desired state. Only Running/Off are requested.
	if ps := vm.Spec.DesiredPowerState; ps == types.VMPowerRunning || ps == types.VMPowerOff {
		pout, perr := r.hv.SetVMPowerState(ctx, vm.Meta.Name, ps)
		res.Conditions = append(res.Conditions, r.condition("VMPower/"+vm.Meta.Name, pout, perr))
		if perr != nil {
			r.log.Error("set vm power failed", "vm", vm.Meta.Name, "desired", ps, "err", perr)
			res.Phase = types.PhaseDegraded
			return res
		}
		if pout != hyperv.OutcomeUnchanged {
			res.Changed = true
			r.log.Info("vm power reconciled", "vm", vm.Meta.Name, "desired", ps, "outcome", pout)
		}
	}

	// Observe the settled VM for status. A read failure does not fail the pass —
	// the configuration and power were honoured; we just report less detail.
	state, serr := r.hv.GetVMState(ctx, vm.Meta.Name)
	if serr != nil {
		r.log.Warn("get vm state failed; reporting without runtime metrics", "vm", vm.Meta.Name, "err", serr)
	} else {
		res.PowerState = state.PowerState
		res.AssignedMemoryBytes = state.AssignedMemoryBytes
		res.CPUUsagePercent = state.CPUUsagePercent
		res.UptimeSeconds = state.UptimeSeconds
	}

	res.Phase = types.PhaseReady
	res.Honoured = true
	return res
}

// ReconcileVMs reconciles every VM placed on this host, returning one result per
// VM in input order. It never stops at the first failure: each VM is independent
// and its status should reflect its own outcome.
func (r *Reconciler) ReconcileVMs(ctx context.Context, vms []types.VM) []VMResult {
	out := make([]VMResult, 0, len(vms))
	for _, vm := range vms {
		out = append(out, r.ReconcileVM(ctx, vm))
	}
	return out
}
