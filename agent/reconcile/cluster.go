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
	if state.Exists {
		conds = append(conds, r.condition("ClusterFormed", hyperv.OutcomeUnchanged, nil))
		return ClusterResult{
			Phase: types.PhaseReady, Honoured: true, Changed: changed,
			FormedMembers: state.Members, Conditions: conds,
		}, nil
	}

	// 3. Not formed. Only the designated former acts; others wait.
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

	// Re-observe so the reported members reflect reality.
	state, err = r.hv.GetClusterState(ctx)
	if err != nil {
		return ClusterResult{Phase: types.PhaseProgressing, Changed: true}, fmt.Errorf("re-observe cluster: %w", err)
	}
	conds = append(conds, r.condition("ClusterFormed", hyperv.OutcomeCreated, nil))
	return ClusterResult{
		Phase: types.PhaseReady, Honoured: state.Exists, Changed: true,
		FormedMembers: state.Members, Conditions: conds,
	}, nil
}
