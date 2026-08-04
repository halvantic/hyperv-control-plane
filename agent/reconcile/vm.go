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
	return r.reconcileVM(ctx, vm, nil)
}

// reconcileVM does the work. knownRoles, when non-nil, is a cluster-role
// membership map the caller has already fetched for the whole set; nil means
// ask about this VM alone.
func (r *Reconciler) reconcileVM(ctx context.Context, vm types.VM, knownRoles map[string]bool) VMResult {
	res := VMResult{Name: vm.Meta.Name}

	// Stand off a VM an imperative job currently holds. Hyper-V refuses to modify
	// a VM mid storage-migration, and a clone or capture needs its disk quiet, so
	// reconciling would fail on every pass and report Degraded for the whole of an
	// operation that is working. Progressing is the honest phase: the VM is not
	// settled, nothing is wrong, and the reason is named.
	if r.vmBusy != nil {
		if kind, busy := r.vmBusy(vm.Meta.Name); busy {
			res.Phase = types.PhaseProgressing
			res.Conditions = append(res.Conditions, types.Condition{
				Type:               "VM/" + vm.Meta.Name,
				Status:             false, // not met yet, and not a failure
				Reason:             "JobInProgress",
				Message:            "standing off while " + kind + " runs on this VM — it holds the VM's files",
				LastTransitionTime: r.now(),
			})
			return res
		}
	}

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
		// The batched read already told us the role exists, so there is nothing
		// to ensure and no reason to spend a cluster-wide query saying so. The
		// condition is still reported: an operator reading status should not have
		// to know which internal path produced it.
		if present, known := knownRoles[vm.Meta.Name]; known && present {
			res.Conditions = append(res.Conditions, r.condition("VMClusterRole/"+vm.Meta.Name, hyperv.OutcomeUnchanged, nil))
		} else {
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

	// Processor count, static memory, Secure Boot and boot order cannot apply
	// while the VM runs; surface it and hold ObservedGeneration back (Progressing,
	// not Degraded) until the VM is stopped, rather than failing every cycle.
	// The agent does not stop the VM itself — power is the operator's to command —
	// so a VM desired Running holds here until they stop it, and the message has
	// to say so or the operator sees "Progressing" forever with no idea why.
	pending := ensured.PendingPowerOff
	if pending {
		// Name the change and what differs. "a processor, memory, Secure Boot or
		// boot-order change" left an operator with four candidates and no way to
		// tell which — and no way to spot a check that is drifting falsely against
		// a VM that already matches.
		msg := "a configuration change needs the VM off; stop it and it applies on the next reconcile"
		if d := ensured.PendingDetail; d != "" {
			msg = d + " needs the VM off; stop it and it applies on the next reconcile"
		}
		res.Conditions = append(res.Conditions, types.Condition{
			Type: "VMConfig/" + vm.Meta.Name, Status: false, Reason: "RequiresPowerOff",
			Message:            msg,
			LastTransitionTime: r.now(),
		})
		r.log.Info("vm config change pending power-off", "vm", vm.Meta.Name, "detail", ensured.PendingDetail)
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
	// the configuration and power were honoured; we just report less detail. It is
	// still surfaced as a condition: without one the VM reported Ready carrying no
	// power state at all, and the console filled that silence with the DESIRED
	// power state, so a VM nobody could read looked like a running, healthy one.
	state, serr := r.hv.GetVMState(ctx, vm.Meta.Name)
	if serr != nil {
		r.log.Warn("get vm state failed; reporting without runtime metrics", "vm", vm.Meta.Name, "err", serr)
		res.Conditions = append(res.Conditions, types.Condition{
			Type: "VMObserved/" + vm.Meta.Name, Status: false, Reason: "ObserveFailed",
			Message:            "cannot read this VM's live state from the host: " + serr.Error(),
			LastTransitionTime: r.now(),
		})
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
// and its status should reflect its own outcome. The whole cycle is bounded by the
// caller; a single VM is not separately timed out, because one VM's reconcile is a
// sequence of cmdlet calls (a fixed-VHD create alone can run for minutes) and a
// per-VM deadline would abort legitimate long work mid-flight.
func (r *Reconciler) ReconcileVMs(ctx context.Context, vms []types.VM) []VMResult {
	// Cluster role membership is per-CLUSTER data, so read it once for the whole
	// set. EnsureClusterVMRole answers it for a single VM by enumerating every
	// cluster group — 2437ms on the rig — so asking per VM re-read the same list
	// once per VM: 73s on a thirty-VM host, all of it confirming what the
	// previous VM had just confirmed.
	//
	// Best effort. A failure here leaves roles nil and every VM falls back to
	// asking for itself, which is exactly the old behaviour.
	var roles map[string]bool
	var names []string
	for _, vm := range vms {
		if vm.Spec.Placement.ClusterName != "" {
			names = append(names, vm.Meta.Name)
		}
	}
	if len(names) > 0 {
		if got, err := r.hv.ClusterVMRolesPresent(ctx, names); err != nil {
			r.log.Warn("batch cluster role query failed; falling back to per-VM", "err", err)
		} else {
			roles = got
		}
	}

	out := make([]VMResult, 0, len(vms))
	for _, vm := range vms {
		out = append(out, r.reconcileVM(ctx, vm, roles))
	}
	return out
}
