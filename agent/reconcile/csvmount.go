package reconcile

import (
	"context"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

// reconcileCSVMountPoints makes each declared volume's mount point match its
// name, for the volumes this node owns.
//
// A Cluster Shared Volume is mounted at C:\ClusterStorage\VolumeN whatever its
// resource is called, so a volume the operator named iSCSI_DS1 lives at Volume1
// and every path built from the declared name points at a directory that is not
// there. That is not cosmetic: the cluster's default storage path is where new
// VMs land, and the replica server's storage path is built the same way — both
// failed on DRCluster for exactly this, with errors naming permissions and
// missing directories rather than the mount point that was never renamed.
//
// PER NODE, deliberately. Renaming belongs with owning the volume, and a
// cluster's volumes are not all owned by one member: DRCluster's two were owned
// one each, so the former-only step this replaces could never have fixed both
// however many passes it ran.
//
// Advisory. A volume whose path is wrong is still a working volume, and a node
// that cannot rename one (VMs running from it, another member owning it) should
// say so rather than degrade the cluster.
func (r *Reconciler) reconcileCSVMountPoints(ctx context.Context, a ClusterAssignment) ([]types.Condition, bool) {
	want := map[string]string{}
	for _, v := range a.Cluster.Spec.Volumes {
		if v.Name != "" {
			want[v.Name] = v.Name
		}
	}
	if len(want) == 0 {
		return nil, false
	}

	out, note, err := r.hv.EnsureCSVMountPoints(ctx, want)
	if err != nil {
		r.log.Warn("ensure CSV mount points failed (advisory, retries next pass)", "err", err)
		return []types.Condition{r.advisoryCondition("CSVMountPoints", hyperv.OutcomeUnchanged, err)}, false
	}
	if out == hyperv.OutcomeUnchanged && note == "" {
		// Nothing owned here needed changing. Silent by design: every member runs
		// this, and a condition per member saying "nothing to do" would bury the
		// one that has something to say.
		return nil, false
	}

	c := r.advisoryCondition("CSVMountPoints", out, nil)
	if note != "" {
		c.Message = note
		// A mount point that could not be renamed leaves the declared name pointing
		// nowhere, so it is reported as unmet rather than as a note on success —
		// "already matches desired state" over a volume whose path is not its name
		// is the reading that let this sit unnoticed.
		if out == hyperv.OutcomeUnchanged {
			c.Status = false
			c.Reason = "NotApplied"
		}
	}
	if out != hyperv.OutcomeUnchanged {
		r.log.Info("CSV mount points renamed", "detail", note)
	}
	return []types.Condition{c}, out != hyperv.OutcomeUnchanged
}
