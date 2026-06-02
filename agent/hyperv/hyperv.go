// Package hyperv is the agent's sole boundary to the host's virtualisation,
// networking and storage stack. Every Hyper-V interaction goes through the
// Interface defined here, so the v1 PowerShell-module implementation (and the
// later WMI/CIM one) can drop in without touching the agent's service loop,
// store, or reconcilers.
//
// The surface is two kinds of operation: pure reads that observe the host, and
// idempotent ensure-operations that drive one piece of host state towards a
// declared spec. The reconciler composes these; it holds no host knowledge of
// its own.
package hyperv

import (
	"context"

	"github.com/joshua-fourie/ballast/api/types"
)

// Outcome reports what an idempotent ensure-operation did. It lets the
// reconciler build accurate status and detect convergence (an all-Unchanged
// pass means the host already matches desired) without the host layer knowing
// anything about generations or reporting.
type Outcome int

const (
	// OutcomeUnchanged: actual state already matched the spec; nothing was done.
	OutcomeUnchanged Outcome = iota
	// OutcomeCreated: the resource was absent and has been created.
	OutcomeCreated
	// OutcomeUpdated: the resource existed but differed and has been adjusted.
	OutcomeUpdated
)

func (o Outcome) String() string {
	switch o {
	case OutcomeCreated:
		return "Created"
	case OutcomeUpdated:
		return "Updated"
	default:
		return "Unchanged"
	}
}

// Interface is the host-facing capability set the agent depends on. All methods
// take a context so a slow PowerShell or WMI call can be cancelled when the
// service is stopping.
//
// Every Ensure* method must be idempotent: applying the same spec twice is a
// no-op that returns OutcomeUnchanged. No method may assume it runs exactly
// once. This is the contract the reconcile loop relies on.
type Interface interface {
	// CollectInventory observes host hardware the centre uses for placement:
	// physical network adapters at minimum, plus physical disks, total memory
	// and logical CPU count where the implementation can determine them.
	//
	// It is a pure read; it must never mutate host state.
	CollectInventory(ctx context.Context) (types.HostInventory, error)

	// EnsureSwitch makes the SET-backed virtual switch described by spec exist
	// and match it: creating it (with the named team members, teaming mode and
	// load-balancing algorithm) when absent, adjusting it when it differs, and
	// doing nothing when it already matches.
	EnsureSwitch(ctx context.Context, spec types.VirtualSwitchSpec) (Outcome, error)

	// EnsureMgmtVNIC makes the management OS vNIC described by spec exist on its
	// switch and carry its VLAN, IP and QoS-weight intent. The switch named by
	// spec.SwitchName is expected to exist already; the reconciler ensures
	// switches before vNICs.
	EnsureMgmtVNIC(ctx context.Context, spec types.ManagementVNICSpec) (Outcome, error)

	// GetHostRoleState observes whether the Hyper-V role is installed and active
	// and whether a reboot is pending. It is a pure read.
	GetHostRoleState(ctx context.Context) (HostRoleState, error)

	// EnsureHyperVRole installs the Hyper-V role and its management tools if they
	// are absent. It never reboots — installation only takes effect after a
	// reboot, which is governed by RebootPolicy and driven by the reconciler, not
	// here. OutcomeCreated means the role was installed this call (a reboot is now
	// needed to make it active); OutcomeUnchanged means it was already present.
	EnsureHyperVRole(ctx context.Context) (Outcome, error)

	// RebootHost restarts the host. The agent only calls this when RebootPolicy
	// permits it; it is never invoked speculatively.
	RebootHost(ctx context.Context) error

	// GetClusterState observes the failover cluster this node belongs to, if
	// any. It is a pure read.
	GetClusterState(ctx context.Context) (ClusterState, error)

	// EnsureFailoverClusteringFeature installs the Failover-Clustering feature
	// and its tools if absent. The feature install does not require a reboot.
	EnsureFailoverClusteringFeature(ctx context.Context) (Outcome, error)

	// FormCluster creates the failover cluster described by f, with this node as
	// the former. It uses New-Cluster (never hand-rolled quorum) and coordinates
	// with the Failover Clustering service. Idempotency is the caller's
	// responsibility: it must only be invoked when no cluster yet exists.
	FormCluster(ctx context.Context, f ClusterFormation) error

	// GetStorageState observes whether Storage Spaces Direct is enabled on the
	// cluster and which CSV volumes exist. Pure read.
	GetStorageState(ctx context.Context) (StorageState, error)

	// EnableS2D turns on Storage Spaces Direct for the cluster (creating the S2D
	// pool). A cluster-level operation run by the former; the caller invokes it
	// only when S2D is not already enabled.
	EnableS2D(ctx context.Context) (Outcome, error)

	// EnsureCSV makes the Cluster Shared Volume described by spec exist on the
	// S2D pool, idempotently: OutcomeUnchanged when it already exists,
	// OutcomeCreated when it had to be provisioned.
	EnsureCSV(ctx context.Context, spec CSVProvision) (Outcome, error)
}

// StorageState is the observed S2D/CSV state on the cluster.
type StorageState struct {
	// S2DEnabled is true when Storage Spaces Direct is on and the pool exists.
	S2DEnabled bool
	// Volumes are the CSV / virtual-disk names that currently exist.
	Volumes []string
}

// CSVProvision is the input to provisioning one Cluster Shared Volume.
type CSVProvision struct {
	Name      string
	SizeBytes uint64
	// ResiliencyType is "Mirror" or "Parity"; empty defaults to Mirror.
	ResiliencyType string
}

// ClusterState is the observed failover-cluster membership from one node's view.
type ClusterState struct {
	// Exists is true when this node is part of a formed cluster.
	Exists bool
	// Name is the cluster's name (empty when Exists is false).
	Name string
	// Members are the node names currently in the cluster.
	Members []string
}

// ClusterFormation is the input to New-Cluster: the cluster to create and the
// nodes to bring in.
type ClusterFormation struct {
	Name string
	// Members are the host names to include at formation.
	Members []string
	// ManagementIP is the cluster's static management address; empty asks
	// Failover Clustering to obtain one via DHCP.
	ManagementIP string
}

// HostRoleState is the observed state of the host's Hyper-V role.
type HostRoleState struct {
	// HyperVInstalled is true only when the role is installed and active (an
	// install that is staged but awaiting a reboot reports false).
	HyperVInstalled bool
	// RebootPending is true when the host has a reboot queued (for example a
	// staged role install) that has not yet happened.
	RebootPending bool
}
