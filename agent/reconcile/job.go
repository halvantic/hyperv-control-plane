package reconcile

import (
	"context"
	"fmt"

	"github.com/joshua-fourie/ballast/api/types"
)

// ExecuteJob runs one imperative Job locally and returns a short human-readable
// result detail (shown in the centre on success) plus any error. Unlike the
// reconcile loop, a Job is a one-shot action — it is not retried by the loop;
// the centre records the outcome and the operator re-issues on failure.
func (r *Reconciler) ExecuteJob(ctx context.Context, job types.Job) (string, error) {
	p := job.Params
	switch job.Kind {
	case types.JobVMStart:
		_, err := r.hv.SetVMPowerState(ctx, p["vm"], types.VMPowerRunning)
		return done(err, "started "+p["vm"])
	case types.JobVMStop:
		_, err := r.hv.SetVMPowerState(ctx, p["vm"], types.VMPowerOff)
		return done(err, "stopped "+p["vm"])
	case types.JobVMCheckpoint:
		return done(r.hv.CreateVMCheckpoint(ctx, p["vm"], p["name"]), "checkpoint created")
	case types.JobVMExport:
		return done(r.hv.ExportVM(ctx, p["vm"], p["path"]), "exported to "+p["path"])
	case types.JobVMApplyCheck:
		return done(r.hv.ApplyVMCheckpoint(ctx, p["vm"], p["name"]), "applied checkpoint "+p["name"])
	case types.JobVMRemoveCheck:
		return done(r.hv.RemoveVMCheckpoint(ctx, p["vm"], p["name"]), "removed checkpoint "+p["name"])
	case types.JobClusterAddNode:
		return done(r.hv.AddClusterNode(ctx, p["node"]), "added "+p["node"])
	case types.JobClusterEvict:
		return done(r.hv.EvictClusterNode(ctx, p["node"]), "evicted "+p["node"])
	case types.JobNodeDrain:
		return done(r.hv.DrainNode(ctx, p["node"]), "drained "+p["node"])
	case types.JobNodeResume:
		return done(r.hv.ResumeNode(ctx, p["node"]), "resumed "+p["node"])
	default:
		return "", fmt.Errorf("unknown job kind %q", job.Kind)
	}
}

// done pairs an op error with its success message.
func done(err error, msg string) (string, error) {
	if err != nil {
		return "", err
	}
	return msg, nil
}
