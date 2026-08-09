package reconcile

import (
	"context"
	"fmt"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

// reconcileISCSI connects THIS node to the cluster's iSCSI array.
//
// Per-node, not former-only, unlike every other cluster-wide setting here. A
// login is a property of one initiator: there is no shared object for the
// members to race over, and gating it on the former would leave every other node
// unable to see the storage at all.
//
// Returns nil status for any cluster that is not iSCSI-backed, so an S2D cluster
// reports nothing rather than reporting an empty iSCSI state — "not applicable"
// and "connected to nothing" must not look the same.
func (r *Reconciler) reconcileISCSI(ctx context.Context, a ClusterAssignment, secrets map[string]types.Secret) (*types.ISCSIStatus, []types.Condition, bool) {
	spec := a.Cluster.Spec
	if spec.StorageKind() != types.StorageKindISCSI {
		return nil, nil, false
	}
	cfg := spec.Storage.ISCSI
	if cfg == nil || len(cfg.Portals) == 0 {
		return nil, []types.Condition{r.condition("ISCSIConnected", hyperv.OutcomeUnchanged,
			fmt.Errorf("this cluster is declared iSCSI-backed but no portal is configured, so no node can reach the array"))}, false
	}

	// The CHAP credential is delivered with the desired-state pull and never
	// stored: it is used for this pass and dropped, like every other secret.
	var chapUser, chapSecret string
	if cfg.CredentialSecret != "" {
		s, ok := secrets[cfg.CredentialSecret]
		if !ok {
			// Say only what is actually known here. The agent is HOLDING the cluster
			// spec that names this credential, so "check the cluster references it"
			// is asking the operator to verify something the agent can already see is
			// true — and when the fault was the centre not sending it, that sent
			// people to check two things that were both already correct.
			return nil, []types.Condition{r.condition("ISCSIConnected", hyperv.OutcomeUnchanged,
				fmt.Errorf("this cluster references CHAP credential %q, but the centre did not send it to this host, so the node cannot log in to the array. If the credential exists in Settings, this is a delivery fault at the centre rather than anything to fix on this host", cfg.CredentialSecret))}, false
		}
		// Same shape as a domain credential: username + password. CHAP calls the
		// second one a secret, but the vault stores it under the same key.
		chapUser, chapSecret = s.Data["username"], s.Data["password"]
		if chapSecret == "" {
			chapSecret = s.Data["secret"]
		}
	}

	st, out, err := r.hv.EnsureISCSI(ctx, *cfg, chapUser, chapSecret)
	conds := []types.Condition{r.condition("ISCSIConnected", out, err)}
	if err != nil {
		r.log.Error("ensure iscsi failed", "err", err)
		return nil, conds, false
	}

	status := &types.ISCSIStatus{
		// Node is stamped by the runner, which knows this agent's registered name.
		// Taking it from the OS hostname here would report a name that need not
		// match the one the centre keys hosts by.
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
		})
	}

	// MPIO that has been installed but not yet rebooted into is NOT protecting
	// anything. Reported as a failing condition rather than a note, because the
	// window between "installed" and "in effect" is exactly when putting a
	// multipath LUN under the cluster would present it twice.
	if st.RebootRequired {
		conds = append(conds, types.Condition{
			Type: "ISCSIMultipath", Status: false, Reason: "RebootRequired",
			Message:            "Multipath I/O was installed but needs a restart before it takes effect. Until this node restarts, a LUN reached by more than one path is presented as separate disks, so do not add multipath storage to the cluster yet.",
			LastTransitionTime: r.now(),
		})
	}

	// A target that is discovered but not logged in is worth its own condition:
	// "the array is reachable but this node is not attached" has a different
	// remedy from "the array cannot be reached", and both otherwise present as
	// simply having no disks.
	var notConnected []string
	for _, s := range status.Sessions {
		if !s.Connected {
			notConnected = append(notConnected, s.TargetIQN)
		}
	}
	if len(notConnected) > 0 {
		conds = append(conds, types.Condition{
			Type: "ISCSIConnected", Status: false, Reason: "TargetNotConnected",
			Message: fmt.Sprintf("discovered but not logged in to %v — the array is reachable, so this is usually the target's allowed-initiator list: it must include %s",
				notConnected, st.InitiatorIQN),
			LastTransitionTime: r.now(),
		})
	}

	return status, conds, out != hyperv.OutcomeUnchanged
}
