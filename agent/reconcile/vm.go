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

	// VMID is the VM's Hyper-V GUID, used by the centre as the console
	// preconnection-blob. Empty when the VM does not yet exist.
	VMID string

	// GuestOS / IPAddress / GuestFQDN are observed guest details (integration
	// services).
	GuestOS   string
	IPAddress string
	GuestFQDN string

	// Checkpoints is the VM's current Hyper-V checkpoints (snapshots).
	Checkpoints []types.VMCheckpoint

	// Observed is the VM's actual config (for showing/adopting unmanaged VMs).
	Observed *types.VMObserved

	// Replication is the VM's observed Hyper-V Replica state, when present.
	Replication *types.VMReplicationStatus

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

	ensured, err := r.hv.EnsureVM(ctx, vm)
	res.Conditions = append(res.Conditions, r.condition("VM/"+vm.Meta.Name, ensured.Outcome, err))
	if err != nil {
		r.log.Error("ensure vm failed", "vm", vm.Meta.Name, "err", err)
		res.Phase = types.PhaseDegraded
		return res
	}
	if ensured.Outcome != hyperv.OutcomeUnchanged {
		res.Changed = true
		r.log.Info("vm reconciled", "vm", vm.Meta.Name, "outcome", ensured.Outcome)
	}
	// A clustered VM must be registered as a highly-available role so Failover
	// Clustering owns its placement and failover. Idempotent: a no-op once the
	// role exists.
	if vm.Spec.Placement.ClusterName != "" {
		hout, herr := r.hv.EnsureClusterVMRole(ctx, vm.Meta.Name)
		res.Conditions = append(res.Conditions, r.condition("VMClusterRole/"+vm.Meta.Name, hout, herr))
		if herr != nil {
			r.log.Error("ensure cluster vm role failed", "vm", vm.Meta.Name, "err", herr)
			res.Phase = types.PhaseDegraded
			return res
		}
		if hout != hyperv.OutcomeUnchanged {
			res.Changed = true
			r.log.Info("vm registered as cluster role", "vm", vm.Meta.Name)
		}
	}
	// Hyper-V Replica: drive the VM's replication relationship to spec. A
	// failure surfaces on its condition and holds the VM at Progressing (the
	// target side may still be provisioning its replica server/broker — this
	// retries every pass) rather than degrading a VM that is otherwise healthy.
	replPending := false
	if vm.Spec.Replication != nil {
		rout, rerr := r.hv.EnsureVMReplication(ctx, vm.Meta.Name, *vm.Spec.Replication)
		res.Conditions = append(res.Conditions, r.condition("VMReplication/"+vm.Meta.Name, rout, rerr))
		if rerr != nil {
			r.log.Warn("ensure vm replication failed (retries next pass)", "vm", vm.Meta.Name, "err", rerr)
			replPending = true
		} else if rout != hyperv.OutcomeUnchanged {
			res.Changed = true
			r.log.Info("vm replication reconciled", "vm", vm.Meta.Name, "outcome", rout)
		}
	}

	// A processor-count or static-memory change cannot apply while the VM runs;
	// surface it and hold ObservedGeneration back (Progressing, not Degraded)
	// until the VM is stopped, rather than failing every cycle.
	pending := ensured.PendingPowerOff
	if pending {
		res.Conditions = append(res.Conditions, types.Condition{
			Type: "VMConfig/" + vm.Meta.Name, Status: false, Reason: "RequiresPowerOff",
			Message:            "processor/memory change settles once the VM is stopped",
			LastTransitionTime: r.now(),
		})
		r.log.Info("vm config change pending power-off", "vm", vm.Meta.Name)
	}

	// Power is imperative, not a continuously-enforced desired state: an operator
	// or the guest OS may stop, start, or restart a VM, and the agent must not
	// fight that by driving it back every pass. The *persistent* boot policy is
	// AutomaticStartAction (applied by EnsureVM above), which brings a VM back
	// after a host reboot. Here we only honour the requested initial power state,
	// once, at creation — a VM authored to run comes up running.
	if ensured.Outcome == hyperv.OutcomeCreated && vm.Spec.DesiredPowerState == types.VMPowerRunning {
		pout, perr := r.hv.SetVMPowerState(ctx, vm.Meta.Name, types.VMPowerRunning)
		res.Conditions = append(res.Conditions, r.condition("VMPower/"+vm.Meta.Name, pout, perr))
		if perr != nil {
			r.log.Error("initial vm power-on failed", "vm", vm.Meta.Name, "err", perr)
			res.Phase = types.PhaseDegraded
			return res
		}
		if pout != hyperv.OutcomeUnchanged {
			res.Changed = true
			r.log.Info("vm powered on at creation", "vm", vm.Meta.Name)
		}
	}

	// Observe the settled VM for status. A read failure does not fail the pass —
	// the configuration and power were honoured; we just report less detail.
	state, serr := r.hv.GetVMState(ctx, vm.Meta.Name)
	if serr != nil {
		r.log.Warn("get vm state failed; reporting without runtime metrics", "vm", vm.Meta.Name, "err", serr)
	} else {
		res.PowerState = state.PowerState
		res.VMID = state.ID
		res.GuestOS = state.GuestOS
		res.IPAddress = state.IPAddress
		res.GuestFQDN = state.GuestFQDN
		res.Checkpoints = state.Checkpoints
		res.Observed = state.Observed
		res.AssignedMemoryBytes = state.AssignedMemoryBytes
		res.CPUUsagePercent = state.CPUUsagePercent
		res.UptimeSeconds = state.UptimeSeconds
		res.Replication = state.Replication
	}

	if pending || replPending {
		// Configuration is not fully honoured until the deferred change applies.
		res.Phase = types.PhaseProgressing
		res.Honoured = false
		return res
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
