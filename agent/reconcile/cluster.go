package reconcile

import (
	"context"
	"fmt"
	"strings"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

// ClusterAssignment is the centre's per-host cluster instruction, delivered with
// the desired-state pull and converted from the wire type by the agent.
type ClusterAssignment struct {
	IsMember bool
	IsFormer bool
	Cluster  types.Cluster
}

// ClusterResult is the outcome of one cluster reconcile pass.
type ClusterResult struct {
	Phase           types.Phase
	Honoured        bool
	Changed         bool
	FormedMembers   []string
	S2DEnabled      bool
	Conditions      []types.Condition
	Groups          []types.ClusterGroupStatus
	CSVs            []types.CSVStatus
	VMs             []types.ClusterVMStatus
	Nodes           []types.ClusterNodeStatus
	Pool            *types.ClusterPoolStatus
	Networks        []types.ClusterNetworkStatus
	Witness         *types.ClusterWitnessStatus
	ReplicaBroker   *types.ClusterReplicaBrokerStatus
	FunctionalLevel int
	NodeOSBuild     int
}

// ReconcileCluster drives this node towards its cluster assignment: ensure the
// Failover-Clustering feature, then, if this node is the designated former and
// no cluster exists yet, form it with New-Cluster. Non-former members simply
// wait to observe themselves in the cluster the former creates.
//
// Once a cluster exists, Failover Clustering owns its availability and quorum —
// the agent does not re-form or second-guess it, and cluster survival does not
// depend on the centre. So this runs only when the centre delivered a current
// assignment; it never tries to form a cluster autonomously.
func (r *Reconciler) ReconcileCluster(ctx context.Context, a ClusterAssignment) (ClusterResult, error) {
	if !a.IsMember {
		return ClusterResult{Phase: types.PhaseReady, Honoured: true}, nil
	}

	var (
		conds   []types.Condition
		changed bool
	)

	// 1. Failover-Clustering feature must be present on every member.
	out, err := r.hv.EnsureFailoverClusteringFeature(ctx)
	conds = append(conds, r.condition("FailoverClusteringInstalled", out, err))
	if err != nil {
		return ClusterResult{Phase: types.PhaseDegraded, Conditions: conds}, fmt.Errorf("ensure failover clustering: %w", err)
	}
	changed = changed || out != hyperv.OutcomeUnchanged

	// 1b. The firewall rule groups a cluster member needs for node-to-node
	// coordination (Failover Clusters + WMI). Without WMI, cross-node operations
	// like Add-ClusterVirtualMachineRole fail with "RPC server unavailable". A
	// failure here is surfaced but does not stop the pass.
	fwOut, fwErr := r.hv.EnsureClusterFirewall(ctx)
	conds = append(conds, r.condition("ClusterFirewall", fwOut, fwErr))
	if fwErr != nil {
		r.log.Error("ensure cluster firewall failed", "err", fwErr)
	} else {
		changed = changed || fwOut != hyperv.OutcomeUnchanged
	}

	// 2. Observe whether this node is already in the cluster.
	state, err := r.hv.GetClusterState(ctx)
	if err != nil {
		conds = append(conds, r.condition("ClusterFormed", hyperv.OutcomeUnchanged, err))
		return ClusterResult{Phase: types.PhaseDegraded, Changed: changed, Conditions: conds}, fmt.Errorf("get cluster state: %w", err)
	}
	// The node is clustered but the state could not be read this pass (cluster
	// service momentarily unavailable). Do NOT proceed: forming would be a spurious
	// New-Cluster on an already-joined node, and reporting would clobber the real
	// status with empties. Defer to the next pass.
	if state.Exists && !state.Known {
		conds = append(conds, types.Condition{
			Type: "ClusterFormed", Status: true, Reason: "StateUndetermined",
			Message:            "cluster present but its state was unreadable this pass — deferring",
			LastTransitionTime: r.now(),
		})
		return ClusterResult{Phase: types.PhaseProgressing, Honoured: true, Changed: changed, Conditions: conds}, nil
	}
	// 3. Not formed yet.
	if !state.Exists {
		// Only the designated former acts; others wait.
		if !a.IsFormer {
			conds = append(conds, types.Condition{
				Type: "ClusterFormed", Status: false, Reason: "AwaitingFormer",
				Message:            "waiting for the designated former to create the cluster",
				LastTransitionTime: r.now(),
			})
			return ClusterResult{Phase: types.PhaseProgressing, Changed: changed, Conditions: conds}, nil
		}
		// Gate: if this cluster runs a converged SET switch, do not form until the
		// former's networking is in place (switch up + management IP re-homed onto
		// its vNIC). Forming over the pre-switch network and moving the IP afterwards
		// is what churns heartbeats/DNS on a live cluster. Only gates when a switch
		// is defined; a switchless cluster forms immediately.
		var swNames []string
		for _, sw := range a.Cluster.Spec.Switches {
			if sw.Name != "" {
				swNames = append(swNames, sw.Name)
			}
		}
		if len(swNames) > 0 {
			ready, rerr := r.hv.ConvergedNetworkReady(ctx, swNames)
			if rerr != nil {
				r.log.Warn("converged-network readiness check failed; deferring formation", "err", rerr)
				ready = false
			}
			if !ready {
				conds = append(conds, types.Condition{
					Type: "ClusterFormed", Status: false, Reason: "AwaitingNetworking",
					Message:            "waiting for the converged switch and management vNIC before forming the cluster",
					LastTransitionTime: r.now(),
				})
				return ClusterResult{Phase: types.PhaseProgressing, Changed: changed, Conditions: conds}, nil
			}
		}
		formation := hyperv.ClusterFormation{
			Name:         a.Cluster.Meta.Name,
			Members:      a.Cluster.Spec.Members,
			ManagementIP: a.Cluster.Spec.ManagementIP,
		}
		r.log.Info("forming cluster", "name", formation.Name, "members", formation.Members)
		if err := r.hv.FormCluster(ctx, formation); err != nil {
			conds = append(conds, r.condition("ClusterFormed", hyperv.OutcomeUnchanged, err))
			return ClusterResult{Phase: types.PhaseDegraded, Changed: changed, Conditions: conds}, fmt.Errorf("form cluster: %w", err)
		}
		changed = true
		// Re-observe so the reported members reflect reality.
		state, err = r.hv.GetClusterState(ctx)
		if err != nil {
			return ClusterResult{Phase: types.PhaseProgressing, Changed: true}, fmt.Errorf("re-observe cluster: %w", err)
		}
		conds = append(conds, r.condition("ClusterFormed", hyperv.OutcomeCreated, nil))
	} else {
		conds = append(conds, r.condition("ClusterFormed", hyperv.OutcomeUnchanged, nil))
	}

	// 4. Storage Spaces Direct + Cluster Shared Volumes — the former provisions
	// cluster-wide storage once the cluster exists. Non-formers do not touch it.
	s2dEnabled, storageConds, storageChanged, storageErr := r.reconcileStorage(ctx, a)
	conds = append(conds, storageConds...)
	changed = changed || storageChanged

	// 5. Kerberos live migration needs constrained delegation between the nodes'
	// computer accounts. The former (a domain admin) configures it once when the
	// cluster's live-migration auth is Kerberos — so provisioning a cluster with
	// Kerberos migration sets this up automatically, no manual AD step. It needs a
	// reachable DC (AD Web Services / DNS); when AD is transiently unreachable this
	// must NOT degrade the cluster — it is advisory (best-effort) and retries each
	// pass, so a broken DNS/trust window surfaces as an advisory note, not a
	// failure. Live migration simply won't work until the delegation lands.
	if lm := a.Cluster.Spec.LiveMigration; a.IsFormer && lm != nil && lm.Enabled && strings.EqualFold(lm.AuthenticationType, "Kerberos") {
		dOut, dErr := r.hv.EnsureMigrationDelegation(ctx, a.Cluster.Spec.Members)
		conds = append(conds, r.advisoryCondition("MigrationDelegation", dOut, dErr))
		if dErr != nil {
			r.log.Warn("ensure migration delegation failed (best-effort, not degrading)", "err", dErr)
		} else {
			changed = changed || dOut != hyperv.OutcomeUnchanged
		}
	}

	// 5b. Quorum witness. Former-only, like the other cluster-wide settings:
	// every member can see the cluster, so without that gate all of them would
	// race to set the same witness and each would read the others' write as
	// drift.
	//
	// An empty Type means "not declared" and is left alone — a cluster whose
	// quorum an operator manages by hand must not be reconfigured just because
	// Ballast now knows how to. Declaring None is how you ask for no witness.
	//
	// Best-effort: a witness the agent cannot reach (share offline, permissions
	// wrong on the cluster computer object) is worth reporting loudly, but it
	// does not make the cluster degraded — quorum is unchanged from before the
	// attempt, and the existing configuration keeps working.
	if a.IsFormer && a.Cluster.Spec.Witness.Type != "" {
		wOut, wErr := r.hv.EnsureClusterWitness(ctx, a.Cluster.Spec.Witness)
		conds = append(conds, r.condition("ClusterWitness", wOut, wErr))
		if wErr != nil {
			r.log.Warn("ensure cluster witness failed (retries next pass)", "err", wErr)
		} else if wOut != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("cluster witness reconciled", "type", a.Cluster.Spec.Witness.Type,
				"path", a.Cluster.Spec.Witness.FileSharePath, "outcome", wOut)
			// Re-observe: the witness we just set is what should be reported, not
			// the state read before the change.
			if st2, serr := r.hv.GetClusterState(ctx); serr == nil && st2.Known {
				state.Witness = st2.Witness
			}
		}
	}

	// 6. Hyper-V Replica Broker — required for the cluster to send or receive
	// replica traffic; replication addresses the broker's client access point.
	// Former-only, like other cluster-wide roles. Best-effort: a transient
	// failure (e.g. AD unreachable for the CAP computer object) surfaces on the
	// condition and retries next pass without degrading the cluster.
	if a.IsFormer && a.Cluster.Spec.ReplicaBroker != nil {
		bOut, bErr := r.hv.EnsureReplicaBroker(ctx, *a.Cluster.Spec.ReplicaBroker)
		conds = append(conds, r.condition("ReplicaBroker", bOut, bErr))
		if bErr != nil {
			r.log.Warn("ensure replica broker failed (retries next pass)", "err", bErr)
		} else if bOut != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("replica broker reconciled", "outcome", bOut)
		}
	}

	phase, honoured := types.PhaseReady, true
	if storageErr != nil {
		phase, honoured = types.PhaseDegraded, false
	}
	return ClusterResult{
		Phase: phase, Honoured: honoured, Changed: changed,
		FormedMembers: state.Members, S2DEnabled: s2dEnabled, Conditions: conds,
		Groups: clusterGroupsToStatus(state.Groups), CSVs: clusterCSVsToStatus(state.CSVs),
		VMs: clusterVMsToStatus(state.VMs), Nodes: clusterNodesToStatus(state.Nodes),
		Pool: clusterPoolToStatus(state.Pool), Networks: clusterNetworksToStatus(state.Networks),
		Witness: clusterWitnessToStatus(state.Witness), ReplicaBroker: clusterBrokerToStatus(state.ReplicaBroker),
		FunctionalLevel: state.FunctionalLevel, NodeOSBuild: state.NodeOSBuild,
	}, storageErr
}

// clusterBrokerToStatus carries the observed Replica Broker through, nil for
// nil — same reason as the witness: "no broker" and "not read yet" are different
// facts and the console must be able to tell them apart.
func clusterBrokerToStatus(b *hyperv.ClusterReplicaBroker) *types.ClusterReplicaBrokerStatus {
	if b == nil {
		return nil
	}
	return &types.ClusterReplicaBrokerStatus{Name: b.Name, State: b.State, StorageLocation: b.StorageLocation}
}

// clusterWitnessToStatus carries the observed quorum configuration through.
// Nil stays nil: "the former has not reported quorum yet" and "this cluster has
// no witness" are different facts, and collapsing them would let a cluster with
// no witness at all look like one that simply has not been read.
func clusterWitnessToStatus(w *hyperv.ClusterWitness) *types.ClusterWitnessStatus {
	if w == nil {
		return nil
	}
	return &types.ClusterWitnessStatus{
		Type:       types.WitnessType(w.Type),
		Path:       w.Path,
		State:      w.State,
		QuorumType: w.QuorumType,
	}
}

func clusterNetworksToStatus(ns []hyperv.ClusterNetworkInfo) []types.ClusterNetworkStatus {
	out := make([]types.ClusterNetworkStatus, 0, len(ns))
	for _, n := range ns {
		out = append(out, types.ClusterNetworkStatus{Name: n.Name, CIDR: n.CIDR, Role: n.Role, State: n.State, Metric: n.Metric})
	}
	return out
}

func clusterPoolToStatus(p *hyperv.ClusterPool) *types.ClusterPoolStatus {
	if p == nil {
		return nil
	}
	return &types.ClusterPoolStatus{Name: p.Name, RawBytes: p.RawBytes, AllocatedBytes: p.AllocatedBytes,
		Health: p.Health, Operational: p.Operational, UnhealthyDisks: p.UnhealthyDisks,
		DisksInMaintenance: p.DisksInMaintenance, TotalDisks: p.TotalDisks,
		Resyncing: p.Resyncing, ResyncPercent: p.ResyncPercent, ResyncJob: p.ResyncJob,
		ResyncRemainingBytes: p.ResyncRemainingBytes}
}

func clusterNodesToStatus(ns []hyperv.ClusterNodeState) []types.ClusterNodeStatus {
	out := make([]types.ClusterNodeStatus, 0, len(ns))
	for _, n := range ns {
		out = append(out, types.ClusterNodeStatus{Name: n.Name, State: n.State})
	}
	return out
}

func clusterVMsToStatus(vs []hyperv.ClusterVM) []types.ClusterVMStatus {
	out := make([]types.ClusterVMStatus, 0, len(vs))
	for _, v := range vs {
		out = append(out, types.ClusterVMStatus{Name: v.Name, OwnerNode: v.OwnerNode, State: v.State})
	}
	return out
}

func clusterGroupsToStatus(gs []hyperv.ClusterGroup) []types.ClusterGroupStatus {
	out := make([]types.ClusterGroupStatus, 0, len(gs))
	for _, g := range gs {
		out = append(out, types.ClusterGroupStatus{Name: g.Name, OwnerNode: g.OwnerNode, State: g.State, GroupType: g.GroupType})
	}
	return out
}

func clusterCSVsToStatus(vs []hyperv.ClusterCSV) []types.CSVStatus {
	out := make([]types.CSVStatus, 0, len(vs))
	for _, v := range vs {
		out = append(out, types.CSVStatus{Name: v.Name, OwnerNode: v.OwnerNode, State: v.State,
			Health: v.Health, Operational: v.Operational, DetachedReason: v.DetachedReason,
			SizeBytes: v.SizeBytes, FreeBytes: v.FreeBytes})
	}
	return out
}

// reconcileStorage enables S2D (if requested and not already on) and provisions
// the desired CSVs. It is a no-op for non-formers or for any storage kind other
// than S2D.
//
// Gated on StorageKind() rather than EnableS2D so the two eras of spec are
// indistinguishable here: a cluster authored before ClusterStorageSpec existed
// carries only the flag, one authored after carries only the kind, and this must
// behave identically for both. Reading the flag directly would silently stop
// provisioning storage for every cluster converted to the new field — the
// reconciler would simply return, with every condition it would have raised
// absent rather than failing.
func (r *Reconciler) reconcileStorage(ctx context.Context, a ClusterAssignment) (s2dEnabled bool, conds []types.Condition, changed bool, firstErr error) {
	if !a.IsFormer || a.Cluster.Spec.StorageKind() != types.StorageKindS2D {
		return false, nil, false, nil
	}

	state, err := r.hv.GetStorageState(ctx)
	if err != nil {
		return false, []types.Condition{r.condition("S2DEnabled", hyperv.OutcomeUnchanged, err)}, false, fmt.Errorf("get storage state: %w", err)
	}
	s2dEnabled = state.S2DEnabled

	// If the S2D state could not be determined this pass (the query was starved,
	// typically under heavy CSV I/O), do NOT attempt to enable it. Running
	// Enable-ClusterStorageSpacesDirect on an already-enabled cluster fails and is
	// heavy — a false "disabled" reading must never trigger it. Defer to the next
	// pass and skip the disk/CSV work that depends on S2D.
	if !state.S2DKnown {
		conds = append(conds, r.advisoryCondition("S2DEnabled", hyperv.OutcomeUnchanged,
			fmt.Errorf("S2D state undetermined (host busy) — deferring storage reconcile")))
		return s2dEnabled, conds, changed, nil
	}

	if !state.S2DEnabled {
		out, err := r.hv.EnableS2D(ctx)
		conds = append(conds, r.condition("S2DEnabled", out, err))
		if err != nil {
			return false, conds, changed, fmt.Errorf("enable S2D: %w", err)
		}
		r.log.Info("Storage Spaces Direct enabled")
		s2dEnabled, changed = true, true
	} else {
		conds = append(conds, r.condition("S2DEnabled", hyperv.OutcomeUnchanged, nil))
	}

	// Claim any poolable disks into the pool. A node added after S2D was enabled
	// keeps its disks CanPool until added, so without this a late-joiner
	// contributes no capacity. Best-effort: surfaced as a condition and logged,
	// but a transient failure does not fail the storage pass.
	if s2dEnabled {
		dOut, dErr := r.hv.EnsureS2DPoolDisks(ctx)
		conds = append(conds, r.condition("S2DPoolDisks", dOut, dErr))
		if dErr != nil {
			r.log.Error("claim S2D pool disks failed", "err", dErr)
		} else if dOut != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("S2D pool disks claimed")
		}
	}

	for _, vol := range a.Cluster.Spec.Volumes {
		out, err := r.hv.EnsureCSV(ctx, hyperv.CSVProvision{
			Name:           vol.Name,
			SizeBytes:      vol.SizeBytes,
			ResiliencyType: vol.ResiliencyType,
		})
		conds = append(conds, r.condition("CSV/"+vol.Name, out, err))
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("ensure CSV %q: %w", vol.Name, err)
			}
			continue
		}
		if out != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("CSV reconciled", "name", vol.Name, "outcome", out)
		}
	}
	return s2dEnabled, conds, changed, firstErr
}
