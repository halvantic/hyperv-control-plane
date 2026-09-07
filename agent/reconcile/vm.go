package reconcile

import (
	"context"
	"errors"
	"strings"

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
	// MemoryDemandBytes is what the guest is actually asking for, and it is the
	// only one of the two that measures anything. On a static-memory VM assigned
	// always equals startup, so "assigned vs configured" is 100% by definition —
	// which is exactly what the console was showing for every VM.
	//
	// It was observed by the agent, carried by the proto and stored by the centre,
	// and never once travelled: VMResult had no field for it, so it was dropped
	// between the observation and the report. Second instance of that gap in one
	// session, after ClusterStatus.ReplicaBroker.
	MemoryDemandBytes uint64
	// MemoryStatus is Hyper-V's own verdict on memory pressure: OK, Low, Warning.
	MemoryStatus    string
	CPUUsagePercent int
	UptimeSeconds   int64

	Conditions []types.Condition
}

// ReconcileVM drives one VM towards its desired state: ensure configuration
// (processor count, memory, disks, adapters), then drive power to the desired
// state, then observe the result. Idempotent — a settled VM produces no changes.
//
// It runs against the cached VM spec, so the agent keeps enforcing the last
// intent it was given even while the centre is offline, exactly as for the host.
func (r *Reconciler) ReconcileVM(ctx context.Context, vm types.VM) VMResult {
	// The single-VM entry point reads everything for itself: nobody has taken a
	// live reading on its behalf, and it has no batch to amortise against.
	return r.reconcileVM(ctx, vm, nil, vmObservation{full: true})
}

// observeVM returns the VM's state, taking the expensive full read only when
// this pass owes one. Otherwise the cheap live reading is merged over the last
// full observation, so the report is complete either way.
//
// A full read is also forced when nothing is cached — merging over nothing would
// report a VM with no configuration, which is how the console's config card went
// blank in the first place.
func (r *Reconciler) observeVM(ctx context.Context, name string, obs vmObservation) (hyperv.VMState, error) {
	key := strings.ToLower(name)
	cached, haveCached := r.vmFull[key]

	if !obs.full && obs.haveLive && haveCached {
		return cached.MergeLive(obs.live), nil
	}

	state, err := r.hv.GetVMState(ctx, name)
	if err != nil {
		return hyperv.VMState{}, err
	}
	if r.vmFull == nil {
		r.vmFull = map[string]hyperv.VMState{}
		r.vmFullPower = map[string]types.VMPowerState{}
	}
	r.vmFull[key] = state
	r.vmFullPower[key] = state.PowerState
	return state, nil
}

// forgetVM drops a VM's cached observation, so the next pass reads it fresh.
// Used when the VM is gone or its identity changed under us.
func (r *Reconciler) forgetVM(name string) {
	key := strings.ToLower(name)
	delete(r.vmFull, key)
	delete(r.vmFullPower, key)
}

// reconcileVM does the work. knownRoles, when non-nil, is a cluster-role
// membership map the caller has already fetched for the whole set; nil means
// ask about this VM alone. obs carries the batched live reading and whether the
// expensive config read is owed this pass.
func (r *Reconciler) reconcileVM(ctx context.Context, vm types.VM, knownRoles map[string]bool, obs vmObservation) VMResult {
	res := VMResult{Name: vm.Meta.Name}

	// Stand off a VM an imperative job currently holds. Hyper-V refuses to modify
	// a VM mid storage-migration, and a clone or capture needs its disk quiet, so
	// reconciling would fail on every pass and report Degraded for the whole of an
	// operation that is working. Progressing is the honest phase: the VM is not
	// settled, nothing is wrong, and the reason is named.
	if r.vmBusy != nil {
		if kind, busy := r.vmBusy(vm.Meta.Name); busy {
			// A delete is the one hold that is not about contention over files. It
			// says the VM is meant to be gone, so the honest message is not "wait
			// your turn" but "this is being removed" — and creating it here would
			// undo the operator's action, not merely fail.
			reason, msg := "JobInProgress", "standing off while "+kind+" runs on this VM — it holds the VM's files"
			if kind == types.JobRemoveVM {
				reason = "Removing"
				msg = "this VM is being deleted from the host — not recreating it while the removal settles"
			}
			res.Phase = types.PhaseProgressing
			res.Conditions = append(res.Conditions, types.Condition{
				Type:               "VM/" + vm.Meta.Name,
				Status:             false, // not met yet, and not a failure
				Reason:             reason,
				Message:            msg,
				LastTransitionTime: r.now(),
			})
			return res
		}
	}

	ensured, err := r.hv.EnsureVM(ctx, vm)
	// A VM that cannot be found on a DRAINING host has left, which is what a
	// drain is for. The cluster live-migrates the roles off; a pass that catches
	// one mid-move gets "the object was not found. The object might have been
	// deleted" from Hyper-V, and reporting that as ApplyFailed marks a VM
	// Degraded for migrating successfully — observed on TestServer2025 while
	// HVNEW04 drained, ninety seconds before its reboot even began.
	//
	// Gated on the host's own drain state rather than on the error text, for the
	// reason the vmBusy stand-off gives: the reconciler knowing what its own
	// cluster was asked to do beats teaching it to recognise how each conflict
	// happens to fail.
	if err != nil && r.hostDraining {
		res.Conditions = append(res.Conditions, types.Condition{
			Type: "VM/" + vm.Meta.Name, Status: false, Reason: "Draining",
			Message:            "this host is draining, so " + vm.Meta.Name + " is moving to another node — it is not readable here while it goes. Nothing to do; the node that receives it reports it from there.",
			LastTransitionTime: r.now(),
		})
		r.log.Info("vm not readable while host drains", "vm", vm.Meta.Name, "err", err)
		return res
	}
	res.Conditions = append(res.Conditions, r.condition("VM/"+vm.Meta.Name, ensured.Outcome, err))
	if err != nil {
		r.log.Error("ensure vm failed", "vm", vm.Meta.Name, "err", err)
		res.Phase = types.PhaseDegraded
		return res
	}
	if ensured.Outcome != hyperv.OutcomeUnchanged {
		res.Changed = true
		r.log.Info("vm reconciled", "vm", vm.Meta.Name, "outcome", ensured.Outcome)
		// We just changed the VM, so whatever configuration is cached describes
		// the state before this pass. Read it properly rather than report the
		// change we just made as if it had not happened.
		obs.full = true
	}
	/* The guest integration services, where the spec declares any.

	   Applied here rather than inside EnsureVM because they take effect on a
	   RUNNING guest — unlike Secure Boot or a generation change, which wait for
	   a power cycle. Nothing is touched that the spec does not name. */
	if is := vm.Spec.IntegrationServices; is != nil {
		out, ierr := r.hv.EnsureIntegrationServices(ctx, vm.Meta.Name, is)
		res.Conditions = append(res.Conditions, r.condition("IntegrationServices/"+vm.Meta.Name, out, ierr))
		if ierr != nil {
			r.log.Warn("set integration services failed", "vm", vm.Meta.Name, "err", ierr)
		} else if out != hyperv.OutcomeUnchanged {
			res.Changed = true
			r.log.Info("integration services reconciled", "vm", vm.Meta.Name, "outcome", out)
		}
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
		// Replication that is repairing itself holds the VM at Progressing without
		// reporting a fault. It was reading "ApplyFailed: replication is configured
		// but unhealthy", which invites reconfiguring a relationship in the middle
		// of fixing itself — and reconfiguring restarts the copy from the beginning.
		var working *hyperv.ReplicationWorkingError
		if errors.As(rerr, &working) {
			res.Conditions = append(res.Conditions, types.Condition{
				Type: "VMReplication/" + vm.Meta.Name, Status: false, Reason: "Resynchronising",
				Message: working.Error(), LastTransitionTime: r.now(),
			})
			replPending = true
			rerr = nil
		} else {
			res.Conditions = append(res.Conditions, r.condition("VMReplication/"+vm.Meta.Name, rout, rerr))
		}
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
	state, serr := r.observeVM(ctx, vm.Meta.Name, obs)
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
		res.MemoryDemandBytes = state.MemoryDemandBytes
		res.MemoryStatus = state.MemoryStatus
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
// ReconcileVMs reconciles every VM the host holds.
//
// fullSweep asks for the expensive per-VM observation on every VM regardless of
// what changed — the drift safety net, and what the caller sets after a job, a
// desired-state change, or on the first pass after start.
func (r *Reconciler) ReconcileVMs(ctx context.Context, vms []types.VM, fullSweep bool) []VMResult {
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

	// The cheap live reading for every VM at once: power, memory, CPU, uptime,
	// IPs, replication. Three host-wide queries rather than three per VM.
	//
	// Best effort. If it fails, live stays nil and every VM falls back to its own
	// full read — the behaviour before this existed.
	var live map[string]hyperv.VMLive
	if len(vms) > 0 {
		names := make([]string, 0, len(vms))
		for _, vm := range vms {
			names = append(names, vm.Meta.Name)
		}
		if got, err := r.hv.GetVMLiveStates(ctx, names); err != nil {
			r.log.Warn("batch vm live read failed; falling back to per-VM", "err", err)
		} else {
			live = got
		}
	}

	out := make([]VMResult, 0, len(vms))
	for _, vm := range vms {
		key := strings.ToLower(vm.Meta.Name)
		lv, haveLive := live[key]
		out = append(out, r.reconcileVM(ctx, vm, roles, vmObservation{
			live:     lv,
			haveLive: haveLive && live != nil,
			full:     fullSweep || r.shouldReadFullVMState(key, lv, haveLive && live != nil),
		}))
	}
	return out
}

// vmObservation carries what ReconcileVMs already learned about a VM into the
// per-VM pass, and whether that pass still owes the expensive full read.
type vmObservation struct {
	live     hyperv.VMLive
	haveLive bool
	full     bool
}

// shouldReadFullVMState decides whether this VM's configuration is worth
// re-reading, given the cheap live reading already taken.
//
// Config settles on a power cycle or an explicit job; between those it cannot
// change on its own, so re-deriving it every 45 seconds bought nothing and cost
// ~1.4s per VM. Read it when:
//
//   - there is no live reading (the batch failed, so nothing is known), or
//   - nothing is cached yet (first pass, or the agent restarted), or
//   - the power state has moved since the last full read — a power cycle is
//     exactly when deferred configuration changes land.
//
// The caller adds the other triggers it owns: a finished job, a desired-state
// change, and the periodic drift sweep that catches an edit made outside
// Ballast on a VM nobody ever reboots.
func (r *Reconciler) shouldReadFullVMState(key string, live hyperv.VMLive, haveLive bool) bool {
	if !haveLive {
		return true
	}
	if _, cached := r.vmFull[key]; !cached {
		return true
	}
	return r.vmFullPower[key] != live.PowerState
}
