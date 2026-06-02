package reconcile

import (
	"context"
	"fmt"

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

	// 2. Observe whether this node is already in the cluster.
	state, err := r.hv.GetClusterState(ctx)
	if err != nil {
		conds = append(conds, r.condition("ClusterFormed", hyperv.OutcomeUnchanged, err))
		return ClusterResult{Phase: types.PhaseDegraded, Changed: changed, Conditions: conds}, fmt.Errorf("get cluster state: %w", err)
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

	phase, honoured := types.PhaseReady, true
	if storageErr != nil {
		phase, honoured = types.PhaseDegraded, false
	}
	return ClusterResult{
		Phase: phase, Honoured: honoured, Changed: changed,
		FormedMembers: state.Members, S2DEnabled: s2dEnabled, Conditions: conds,
	}, storageErr
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
