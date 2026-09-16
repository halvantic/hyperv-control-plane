package reconcile

import (
	"context"
	"fmt"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

// reconcileHostISCSI connects a STANDALONE host to its own iSCSI array.
//
// The work is identical to a cluster member's — the initiator service, the
// portals, persistent logins, MPIO are all per-node — so this shares the same
// EnsureISCSI. What differs is afterwards: a cluster adopts the LUN as a Cluster
// Shared Volume, while a standalone host has nothing to share it with, so the
// disk simply appears in its inventory and is provisioned like any other local
// disk. That needs no code here, which is why this is only the connection half.
//
// A cluster member is refused rather than served. Two authorities over one
// initiator is how a node ends up logged in to targets nobody declared, and
// resolving it by precedence would mean an operator's edit silently doing
// nothing. See HostStorageSpec.ISCSI.
func (r *Reconciler) reconcileHostISCSI(ctx context.Context, desired types.Host, secrets map[string]types.Secret) (*types.ISCSIStatus, []types.Condition) {
	cfg := desired.Spec.Storage.ISCSI
	if cfg == nil {
		return nil, nil
	}
	if cm := desired.Spec.ClusterMembership; cm != nil && cm.ClusterName != "" {
		return nil, []types.Condition{{
			Type: "HostISCSI", Status: false, Reason: "Invalid",
			Message:            fmt.Sprintf("this host is a member of cluster %q, which declares its own iSCSI configuration, so the host-level one is not applied. Two authorities over one initiator would leave the node logged in to whichever was reconciled last — configure the array on the cluster, or remove this host from it.", cm.ClusterName),
			LastTransitionTime: r.now(),
		}}
	}
	if len(cfg.Portals) == 0 {
		return nil, []types.Condition{{
			Type: "HostISCSI", Status: false, Reason: "Invalid",
			Message:            "iSCSI is declared for this host but no portal is configured, so there is nothing to connect to",
			LastTransitionTime: r.now(),
		}}
	}

	var chapUser, chapSecret string
	if cfg.CredentialSecret != "" {
		s, ok := secrets[cfg.CredentialSecret]
		if !ok {
			return nil, []types.Condition{{
				Type: "HostISCSI", Status: false, Reason: "ApplyFailed",
				Message:            fmt.Sprintf("this host references CHAP credential %q, but the centre did not send it, so the node cannot log in to the array. If the credential exists in Settings, this is a delivery fault at the centre rather than anything to fix on this host", cfg.CredentialSecret),
				LastTransitionTime: r.now(),
			}}
		}
		chapUser, chapSecret = s.Data["username"], s.Data["password"]
		if chapSecret == "" {
			chapSecret = s.Data["secret"]
		}
	}

	st, out, err := r.hv.EnsureISCSI(ctx, *cfg, chapUser, chapSecret, false, desired.Spec.Networking.StorageAddresses())
	conds := []types.Condition{r.condition("HostISCSI", out, err)}
	if err != nil {
		r.log.Error("ensure host iscsi failed", "err", err)
		return nil, conds
	}

	status := &types.ISCSIStatus{
		Node:           desired.Meta.Name,
		InitiatorIQN:   st.InitiatorIQN,
		ServiceRunning: st.ServiceRunning,
		Portals:        st.Portals,
		MPIOInstalled:  st.MPIOInstalled,
		MPIOEffective:  st.MPIOEffective,
		Message:        st.Message,
	}
	for _, s := range st.Sessions {
		status.Sessions = append(status.Sessions, types.ISCSISession{
			TargetIQN: s.TargetIQN, Connected: s.Connected, Persistent: s.Persistent, Paths: s.Paths,
		})
	}
	for _, d := range st.Disks {
		status.Disks = append(status.Disks, types.ISCSIDisk{
			SerialNumber: d.SerialNumber, Number: d.Number, SizeBytes: d.SizeBytes,
			TargetIQN: d.TargetIQN, LUN: d.LUN, Clustered: d.Clustered, Offline: d.Offline,
			Contents: d.Contents, ContentsKnown: d.ContentsKnown,
		})
	}

	// Multipath installed but not restarted into protects nothing, and on a
	// standalone host the consequence is the same as on a cluster: the LUN is
	// presented once per path, and formatting one of the duplicates is how a disk
	// gets written through two devices that do not know about each other.
	if st.RebootRequired {
		conds = append(conds, types.Condition{
			Type: "HostISCSIMultipath", Status: false, Reason: "RebootRequired",
			Message:            "Multipath I/O was installed but needs a restart before it takes effect. Until this host restarts, a LUN reached by more than one path is presented as separate disks — do not format one yet.",
			LastTransitionTime: r.now(),
		})
	}

	// A target that is discovered but not logged in is worth its own condition:
	// "the array is reachable but this node is not attached" has a different
	// remedy from "the array cannot be reached", and both otherwise present as
	// simply having no disks.
	//
	// Judged ONLY against the targets this spec declares. An array commonly
	// serves several consumers from one set of portals, so discovery returns
	// targets belonging to other clusters and other hosts — and this node must
	// NOT be logged in to those. Flagging them made a node holding exactly what
	// it was asked for report a fault: on the rig 2026-08-24 HVNEW04, correctly
	// attached to DRCluster's target, was told it should also be logged in to
	// iqn1000, which is S2DCluster's LUN and the one thing it must never touch.
	//
	// An empty target list means "log in to everything advertised", and there
	// every discovered target really is one this node should hold.
	var notConnected []string
	for _, s := range status.Sessions {
		if s.Connected || !declaredISCSITarget(cfg.Targets, s.TargetIQN) {
			continue
		}
		notConnected = append(notConnected, s.TargetIQN)
	}
	if len(notConnected) > 0 {
		conds = append(conds, types.Condition{
			Type: "HostISCSI", Status: false, Reason: "TargetNotConnected",
			Message: fmt.Sprintf("discovered but not logged in to %v — the array is reachable, so this is usually the target's allowed-initiator list: it must include %s",
				notConnected, st.InitiatorIQN),
			LastTransitionTime: r.now(),
		})
	}
	return status, conds
}
