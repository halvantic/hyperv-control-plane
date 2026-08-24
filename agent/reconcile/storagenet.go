package reconcile

import (
	"strings"
	"time"

	"github.com/joshua-fourie/ballast/api/types"
)

// Reporting whether a host's storage redundancy is real.
//
// Everything above the interface reports success either way. Two storage vNICs
// on separate subnets, MPIO in effect, three sessions, three paths — and SET
// placing all of them on one physical port. Observed across three hosts on the
// rig 2026-08-24: nothing anywhere said the redundancy was decorative, and only
// a pulled cable would have.
//
// So the host reports it. This is an ADVISORY condition: the arrangement works,
// it is just not redundant, and a lab is entitled to one path. What must not
// happen is silence.

// storageNetworkCondition reports the host's storage-network arrangement.
//
// Returns no condition at all when the host declares no storage vNICs: a host
// with local disks has nothing to say here, and a permanent "not applicable"
// row is noise on every host that will never have storage networking.
func storageNetworkCondition(net types.HostNetworkingSpec, requireMultipath bool, now func() time.Time) []types.Condition {
	if len(net.StorageVNICs()) == 0 {
		return nil
	}
	problems := types.ValidateStorageNetwork(net, requireMultipath)
	c := types.Condition{Type: "StorageNetwork", LastTransitionTime: now()}
	if len(problems) == 0 {
		c.Status = true
		c.Reason = "AlreadyConfigured"
		c.Message = types.DescribeStorageNetwork(net) + ", each on its own subnet — the paths are genuinely separate"
		return []types.Condition{c}
	}
	var whys []string
	for _, p := range problems {
		whys = append(whys, p.Error())
	}
	// Status stays true: the vNICs are applied and the storage works. Reporting
	// this as a failure would put a red host on the console for an arrangement
	// the operator may have chosen, and hide the ones that are actually broken.
	c.Status = true
	c.Reason = "NotRedundant"
	c.Message = strings.Join(whys, "; ")
	return []types.Condition{c}
}

// wantsMultipathStorage is whether more than one storage path is owed.
//
// A cluster member always: both S2D (SMB Multichannel) and a clustered iSCSI
// array (MPIO) lose their shared storage with one link. A standalone host only
// when its own array declares MPIO — a single host with one LUN over one link
// is an ordinary arrangement, not a defect.
func wantsMultipathStorage(desired types.Host) bool {
	if cm := desired.Spec.ClusterMembership; cm != nil && cm.ClusterName != "" {
		return true
	}
	if i := desired.Spec.Storage.ISCSI; i != nil {
		want, _ := i.MPIORequired()
		return want
	}
	return false
}
