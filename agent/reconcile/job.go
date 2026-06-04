package reconcile

import (
	"context"
	"fmt"

	"github.com/joshua-fourie/ballast/api/types"
)

// ExecuteJob runs one imperative Job locally and returns any error. Unlike the
// reconcile loop, a Job is a one-shot action — it is not retried by the loop;
// the centre records the outcome and the operator re-issues on failure.
func (r *Reconciler) ExecuteJob(ctx context.Context, job types.Job) error {
	p := job.Params
	switch job.Kind {
	case types.JobVMStart:
		_, err := r.hv.SetVMPowerState(ctx, p["vm"], types.VMPowerRunning)
		return err
	case types.JobVMStop:
		_, err := r.hv.SetVMPowerState(ctx, p["vm"], types.VMPowerOff)
		return err
	case types.JobVMCheckpoint:
		return r.hv.CreateVMCheckpoint(ctx, p["vm"], p["name"])
	case types.JobClusterAddNode:
		return r.hv.AddClusterNode(ctx, p["node"])
	case types.JobClusterEvict:
		return r.hv.EvictClusterNode(ctx, p["node"])
	default:
		return fmt.Errorf("unknown job kind %q", job.Kind)
	}
}
