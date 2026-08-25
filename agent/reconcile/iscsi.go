package reconcile

import (
	"context"
	"fmt"
	"strings"
	"time"

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

	// BOUNDED, because this step can otherwise eat the whole pass.
	//
	// Several iSCSI cmdlets hang for minutes against a portal that answers on
	// 3260 but never completes the operation, and a TCP probe cannot tell those
	// apart. On Secondary, 2026-08-25, iSCSI took 3m43s of a 5m pass on both
	// members: the pass was cut off before the cluster was ever formed, so a
	// cluster that needs no storage to exist could not come up because its
	// storage was slow. Formation does not depend on iSCSI — only CSV adoption
	// does — and one slow step must not decide whether the rest of the pass runs.
	//
	// The step is given its own budget and the pass CONTINUES when it is spent.
	// That is the difference between "this cluster has a storage problem" and
	// "this cluster does not exist", which is what the console showed.
	//
	// It also makes the script's own phase timings reachable: a cancelled script
	// never emits them, so before this the slow step could not say what it was
	// slow in.
	ictx, icancel := context.WithTimeout(ctx, r.iscsiBudget())
	st, out, err := r.hv.EnsureISCSI(ictx, *cfg, chapUser, chapSecret, true, r.storageAddresses())
	icancel()
	if err != nil && ictx.Err() != nil && ctx.Err() == nil {
		// Ours, not the pass's. Named as the step giving up rather than as the
		// host failing, because nothing has been established about the array.
		return nil, []types.Condition{r.condition("ISCSIConnected", hyperv.OutcomeUnchanged,
			fmt.Errorf("the iSCSI step did not finish within %s and was stopped so the rest of this pass could run. "+
				"Something it called is blocking rather than failing — most often a portal that accepts a TCP connection but never completes discovery or login. "+
				"The cluster itself does not need the array to form, so formation and everything after it continue; only the volumes on this array wait. "+
				"Check the declared portals and targets still exist on the array", r.iscsiBudget()))}, false
	}
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
			Contents: d.Contents, ContentsKnown: d.ContentsKnown,
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
			Type: "ISCSIConnected", Status: false, Reason: "TargetNotConnected",
			Message: fmt.Sprintf("discovered but not logged in to %v — the array is reachable, so this is usually the target's allowed-initiator list: it must include %s",
				notConnected, st.InitiatorIQN),
			LastTransitionTime: r.now(),
		})
	}

	return status, conds, out != hyperv.OutcomeUnchanged
}

// declaredISCSITarget reports whether the spec asked for this target.
//
// An empty declared list means "log in to everything the portals advertise", so
// every discovered target counts. Otherwise only the named ones do: an array
// serves several consumers from one set of portals, and a target belonging to
// another cluster is not this node's to hold.
//
// Case-insensitive deliberately. The initiator service matches target names
// exactly, but Get-IscsiSession echoes them lower-cased while Get-IscsiTarget
// returns the array's own capitalisation — so comparing exactly would report a
// declared, connected target as undeclared.
func declaredISCSITarget(declared []string, target string) bool {
	if len(declared) == 0 {
		return true
	}
	for _, d := range declared {
		if strings.EqualFold(strings.TrimSpace(d), strings.TrimSpace(target)) {
			return true
		}
	}
	return false
}

// storageAddresses is the storage vNIC addresses the host pass last saw. The
// cluster pass has no Host of its own, and binding sessions to the interfaces
// the routing table picks is what left three declared portals sharing one cable.
func (r *Reconciler) storageAddresses() []string { return r.lastStorageAddresses }

// iscsiStepBudget caps the iSCSI step so a blocking cmdlet cannot consume the
// pass. Well inside the cycle limit, and well beyond a healthy run: the same
// step on Primary1's members completes in seconds.
const iscsiStepBudget = 90 * time.Second

func (r *Reconciler) iscsiBudget() time.Duration {
	if r.iscsiReadBudget > 0 {
		return r.iscsiReadBudget
	}
	return iscsiStepBudget
}
