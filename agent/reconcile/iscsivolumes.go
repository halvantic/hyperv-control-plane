package reconcile

import (
	"context"
	"fmt"
	"strings"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

// reconcileISCSIVolumes adopts the array's LUNs into the cluster: each declared
// volume becomes a CSV, and the witness LUN (when one is declared) becomes a
// clustered disk holding the quorum vote.
//
// FORMER ONLY, unlike the login step next to it. Logging in is per-node and every
// member must do its own; adopting a disk is a cluster-wide act on a shared
// object, and several members racing to adopt the same LUN is how a disk ends up
// half-added — partitioned by one node while another is formatting it.
//
// It also runs AFTER the login step, and depends on it having worked: a node that
// is not logged in cannot see the LUN, and "cannot see it" is reported as the
// array not presenting it, which sends the operator to the wrong place. So a
// former whose own iSCSI session is not up defers rather than diagnosing.
func (r *Reconciler) reconcileISCSIVolumes(ctx context.Context, a ClusterAssignment, iscsi *types.ISCSIStatus) (conds []types.Condition, changed bool, firstErr error) {
	spec := a.Cluster.Spec
	if !a.IsFormer || spec.StorageKind() != types.StorageKindISCSI {
		return nil, false, nil
	}
	nothingDeclared := len(spec.Volumes) == 0 && spec.Witness.Disk == nil

	// No sessions means this node cannot see any LUN, and every adoption would
	// fail with a message about the array. Say the true thing instead.
	if iscsi == nil || !anyConnected(iscsi) {
		if nothingDeclared {
			return nil, false, nil
		}
		return []types.Condition{{
			Type: "ISCSIVolumes", Status: false, Reason: "NotAttempted",
			Message: "this node is not logged in to the array yet, so the LUNs are not visible to adopt; adoption is deferred until the iSCSI session is up",
		}}, false, nil
	}

	// Multipath declared but not yet in effect is the one state where adopting is
	// actively unsafe rather than merely premature. Windows is presenting each LUN
	// once per path as unrelated disks, so adoption would take ONE of the
	// duplicates into the cluster — and a cluster writing to one path's disk while
	// a node uses another path to the same blocks is a data corruption, not a
	// performance problem.
	//
	// A condition warning the operator is not enough here. The reconcile loop does
	// not read conditions, so without this the adoption simply proceeds while the
	// console displays the warning next to it.
	if required, _ := iscsiMPIORequired(spec); required && !iscsi.MPIOEffective && !nothingDeclared {
		return []types.Condition{{
			Type: "ISCSIVolumes", Status: false, Reason: "NotAttempted",
			Message: "multipath is not in effect on this node yet, so each LUN is still presented once per path as separate disks; adopting one now would put a single path's disk under the cluster. Restart this node to bring MPIO into effect — adoption resumes by itself afterwards.",
		}}, false, nil
	}

	// Remembered for the ADOPT job, which the operator runs after the reconcile
	// has refused a LUN with contents on it. The job names the volume; the serial
	// that identifies the disk stays the single declared one here, so the job
	// cannot become a second authority over which disk is meant.
	r.rememberVolumeSources(spec.Volumes)

	for _, vol := range spec.Volumes {
		if vol.Source == nil {
			// Under iSCSI a volume without a source is not something Ballast can
			// create — the array owns the LUN — so name that rather than failing
			// inside a provisioning path that does not apply here.
			conds = append(conds, types.Condition{
				Type: "CSV/" + vol.Name, Status: false, Reason: "Invalid",
				Message: "this cluster's storage is an iSCSI array, so the LUN behind this volume already exists and Ballast adopts it rather than creating it. Give the volume the disk's serial number.",
			})
			if firstErr == nil {
				firstErr = fmt.Errorf("volume %q has no source LUN", vol.Name)
			}
			continue
		}
		serial, note, out, err := r.hv.AdoptISCSIDisk(ctx, hyperv.ISCSIAdoption{
			Name:   vol.Name,
			Source: *vol.Source,
			// Wipe is never set from desired state. Formatting a LUN that has
			// contents is a decision an operator makes once, about one disk, with
			// the contents named to them — not a field that sits in a spec and
			// re-applies itself on every pass.
			Wipe: false,
		})
		c := r.condition("CSV/"+vol.Name, out, err)
		// A note is something the adoption could not finish but did not fail on —
		// the mount point still named VolumeN because another node owns the volume,
		// or because VMs are running from the old path. Reporting only "already
		// matches desired state" would present a volume whose declared name is not
		// its path as fully settled.
		if err == nil && note != "" {
			c.Message = note
		}
		conds = append(conds, c)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("adopt volume %q: %w", vol.Name, err)
			}
			continue
		}
		if out != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("iSCSI volume adopted", "name", vol.Name, "serial", serial, "outcome", out)
		}
	}

	// The witness disk is adopted here too, because it is the same act on the same
	// kind of object — but as a clustered disk, never a CSV. Pointing quorum at it
	// is a separate step and belongs with the other witness handling.
	if w := spec.Witness; w.Type == types.WitnessDisk && w.Disk != nil {
		name := a.Cluster.Meta.Name + " Witness"
		_, _, out, err := r.hv.AdoptISCSIDisk(ctx, hyperv.ISCSIAdoption{
			Name: name, Source: *w.Disk, AsWitness: true,
		})
		conds = append(conds, r.condition("WitnessDisk", out, err))
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("adopt witness disk: %w", err)
			}
		} else if out != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("witness disk adopted", "name", name, "outcome", out)
		}
	}
	return conds, changed, firstErr
}

// iscsiMPIORequired answers whether this cluster's storage needs multipath,
// tolerating a spec that declares iSCSI without a config block.
func iscsiMPIORequired(spec types.ClusterSpec) (required bool, overridden bool) {
	if spec.Storage == nil || spec.Storage.ISCSI == nil {
		return false, false
	}
	return spec.Storage.ISCSI.MPIORequired()
}

// anyConnected reports whether this node holds at least one live iSCSI session.
// Portals registered without a session is the common half-configured state — the
// array is reachable but has not granted this initiator anything — and it looks
// like success to anything that only checks the service is running.
func anyConnected(st *types.ISCSIStatus) bool {
	for _, s := range st.Sessions {
		if s.Connected {
			return true
		}
	}
	return false
}

// iscsiVolumeSummary describes what was adopted, for the log line and the
// cluster's message. Kept separate so the reconcile path stays readable.
func iscsiVolumeSummary(vols []types.CSVSpec) string {
	names := make([]string, 0, len(vols))
	for _, v := range vols {
		names = append(names, v.Name)
	}
	return strings.Join(names, ", ")
}

// rememberVolumeSources records the declared LUN behind each volume, so the
// adopt job can find it by volume name alone.
func (r *Reconciler) rememberVolumeSources(vols []types.CSVSpec) {
	m := make(map[string]types.CSVSourceSpec, len(vols))
	for _, v := range vols {
		if v.Source != nil {
			m[strings.ToLower(v.Name)] = *v.Source
		}
	}
	r.volumeSources = m
}

// volumeSource returns the declared LUN for a volume the adopt job names.
//
// It refuses rather than guessing. A job that arrives for a volume this node has
// no spec for is one where the cluster changed underneath the operator, and
// adopting SOME disk because a name nearly matched is how the wrong LUN gets
// formatted.
func (r *Reconciler) volumeSource(name string) (types.CSVSourceSpec, error) {
	src, ok := r.volumeSources[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return types.CSVSourceSpec{}, fmt.Errorf("this node holds no declared volume called %q, so there is no LUN to adopt — the cluster spec may have changed since the console offered this", name)
	}
	if strings.TrimSpace(src.SerialNumber) == "" && strings.TrimSpace(src.TargetIQN) == "" {
		return types.CSVSourceSpec{}, fmt.Errorf("volume %q declares no serial and no target, so there is nothing that identifies which disk to adopt", name)
	}
	return src, nil
}
