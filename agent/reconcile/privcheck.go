package reconcile

import (
	"time"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

// privilegeConditions turns one CheckPrivileges observation into the
// Conditions HostStatus reports. Never attempts to fix anything a check
// finds missing — that decision belongs to whoever holds the actual AD
// rights to change it, not to the agent. See
// docs/agent-least-privilege-ad.md for what these rights are and why the
// agent is deliberately not granted enough to fix a shortfall itself.
func privilegeConditions(chk hyperv.PrivilegeCheck, now time.Time) []types.Condition {
	conds := []types.Condition{localAdminCondition(chk, now)}

	if chk.ClusterApplicable {
		conds = append(conds, clusterAccessCondition(chk, now))
	}
	if chk.ADDelegationApplicable {
		conds = append(conds, adDelegationCondition(chk, now))
	}
	return conds
}

func localAdminCondition(chk hyperv.PrivilegeCheck, now time.Time) types.Condition {
	if chk.IsLocalAdmin {
		return types.Condition{
			Type: "Privilege/LocalAdmin", Status: true, Reason: "OK",
			Message:            "running identity is a member of local Administrators",
			LastTransitionTime: now,
		}
	}
	return types.Condition{
		Type: "Privilege/LocalAdmin", Status: false, Reason: "NotLocalAdmin",
		Message: "the running identity is NOT a member of local Administrators on this host — Hyper-V, " +
			"storage and networking operations will fail. Add it to local Administrators (directly, or via " +
			"the domain group a GPO grants it through) and this will clear on the next check.",
		LastTransitionTime: now,
	}
}

func clusterAccessCondition(chk hyperv.PrivilegeCheck, now time.Time) types.Condition {
	if chk.ClusterAccessOK {
		return types.Condition{
			Type: "Privilege/ClusterAccess", Status: true, Reason: "OK",
			Message:            "running identity can query this host's cluster",
			LastTransitionTime: now,
		}
	}
	return types.Condition{
		Type: "Privilege/ClusterAccess", Status: false, Reason: "ClusterAccessDenied",
		Message: "this host is a cluster member but the running identity cannot query the cluster — cluster " +
			"operations (failover, CSV, live migration) will likely fail. Local Administrators normally " +
			"covers this; check the identity is actually a member and that a policy refresh has applied.",
		LastTransitionTime: now,
	}
}

func adDelegationCondition(chk hyperv.PrivilegeCheck, now time.Time) types.Condition {
	if chk.ADDelegationErr != "" {
		return types.Condition{
			Type: "Privilege/ADDelegation", Status: false, Reason: "CouldNotDetermine",
			Message: "could not determine whether the running identity holds the AD delegation this host's " +
				"OUPath needs — " + chk.ADDelegationErr + ". This is NOT a confirmed shortfall, only an " +
				"unreadable check (e.g. no DC reachable); it will be re-checked.",
			LastTransitionTime: now,
		}
	}
	if chk.ADDelegationOK {
		return types.Condition{
			Type: "Privilege/ADDelegation", Status: true, Reason: "OK",
			Message: "running identity holds write access to msDS-AllowedToDelegateTo and create/delete of " +
				"computer objects on this host's OU",
			LastTransitionTime: now,
		}
	}
	return types.Condition{
		Type: "Privilege/ADDelegation", Status: false, Reason: "MissingDelegation",
		Message: "the running identity does NOT appear to hold the AD delegation this host's OU needs " +
			"(write on msDS-AllowedToDelegateTo, create/delete of computer objects) — live-migration Kerberos " +
			"delegation and cluster (CNO) creation will likely fail with Access Denied. See " +
			"docs/agent-least-privilege-ad.md for the exact dsacls delegation to grant; this check does not " +
			"attempt to grant it.",
		LastTransitionTime: now,
	}
}
