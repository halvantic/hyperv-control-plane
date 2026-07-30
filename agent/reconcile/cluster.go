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
	Phase         types.Phase
	Honoured      bool
	Changed       bool
	FormedMembers []string
	S2DEnabled    bool
	Conditions    []types.Condition
	Groups        []types.ClusterGroupStatus
	CSVs          []types.CSVStatus
	VMs           []types.ClusterVMStatus
	Nodes         []types.ClusterNodeStatus
	Pool          *types.ClusterPoolStatus
	Networks      []types.ClusterNetworkStatus
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
	}, storageErr
}

func clusterNetworksToStatus(ns []hyperv.ClusterNetworkInfo) []types.ClusterNetworkStatus {
	out := make([]types.ClusterNetworkStatus, 0, len(ns))
	for _, n := range ns {
		out = append(out, types.ClusterNetworkStatus{Name: n.Name, CIDR: n.CIDR, Role: n.Role, State: n.State})
	}
	return out
}

func clusterPoolToStatus(p *hyperv.ClusterPool) *types.ClusterPoolStatus {
	if p == nil {
		return nil
	}
	return &types.ClusterPoolStatus{Name: p.Name, RawBytes: p.RawBytes, AllocatedBytes: p.AllocatedBytes,
		Health: p.Health, Operational: p.Operational, UnhealthyDisks: p.UnhealthyDisks, TotalDisks: p.TotalDisks,
		Resyncing: p.Resyncing, ResyncPercent: p.ResyncPercent, ResyncJob: p.ResyncJob}
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
			Health: v.Health, Operational: v.Operational, DetachedReason: v.DetachedReason})
	}
	return out
}

// reconcileStorage enables S2D (if requested and not already on) and provisions
// the desired CSVs. It is a no-op for non-formers or when EnableS2D is false.
func (r *Reconciler) reconcileStorage(ctx context.Context, a ClusterAssignment) (s2dEnabled bool, conds []types.Condition, changed bool, firstErr error) {
	if !a.IsFormer || !a.Cluster.Spec.EnableS2D {
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
