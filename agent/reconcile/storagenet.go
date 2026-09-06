package reconcile

import (
	"strconv"
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

/*
mtuCondition reports whether the host's declared MTUs agree with each other.

	Separate from StorageNetwork because MTU is not only a storage concern — a
	management vNIC at 9000 talking to a 1500 router is its own slow, sporadic
	fault — and because the two answer different questions. StorageNetwork asks
	whether the redundancy is real; this asks whether the frames are the size
	everyone thinks they are.

	Returns nothing when no MTU is declared anywhere. Most hosts will never
	declare one, and a permanent "not applicable" row on every host in the fleet
	is noise that teaches operators to skip the section.

	What this canNOT answer is whether the physical switch between the hosts
	carries jumbo frames. Ballast has no presence on it and cannot read it, so
	the condition says what it verified and names the test that covers the rest,
	rather than implying an end-to-end guarantee it has not made.
*/
func mtuCondition(net types.HostNetworkingSpec, now func() time.Time) []types.Condition {
	if !declaresMTU(net) {
		return nil
	}
	problems := types.ValidateMTU(net)
	c := types.Condition{Type: "MTU", LastTransitionTime: now()}
	if len(problems) == 0 {
		c.Status = true
		c.Reason = "AlreadyConfigured"
		c.Message = describeMTU(net) + ". Whether the physical switch between the hosts carries them is not " +
			"visible from here — run the jumbo path test to confirm a frame that size actually crosses"
		return []types.Condition{c}
	}
	var whys []string
	for _, p := range problems {
		whys = append(whys, p.Error())
	}
	// Status stays true for the same reason StorageNetwork's does: the vNICs are
	// applied and traffic flows. A red host here would hide the ones that are
	// genuinely broken behind an arrangement that merely underperforms.
	c.Status = true
	c.Reason = "Mismatched"
	c.Message = strings.Join(whys, "; ")
	return []types.Condition{c}
}

func declaresMTU(net types.HostNetworkingSpec) bool {
	for _, sw := range net.Switches {
		if sw.MTUBytes > 0 {
			return true
		}
	}
	for _, v := range net.ManagementVNICs {
		if v.MTUBytes > 0 {
			return true
		}
	}
	return false
}

// describeMTU summarises what carries what, in one line.
func describeMTU(net types.HostNetworkingSpec) string {
	var parts []string
	for _, sw := range net.Switches {
		if sw.MTUBytes > 0 {
			parts = append(parts, sw.Name+"'s uplinks at "+strconv.Itoa(sw.MTUBytes))
		}
	}
	for _, v := range net.ManagementVNICs {
		if v.MTUBytes > 0 {
			parts = append(parts, v.Name+" at "+strconv.Itoa(v.MTUBytes))
		}
	}
	if len(parts) == 0 {
		return "no MTU declared"
	}
	return strings.Join(parts, ", ")
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
