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

	// CollectMetrics observes live host utilisation — overall CPU load, physical
	// memory in use, and uptime. A pure read, refreshed each reconcile cycle and
	// reported in HostStatus.Metrics.
	CollectMetrics(ctx context.Context) (types.HostMetrics, error)

	// CollectResources observes pre-existing host objects the UI can offer as
	// choices: virtual switches, storage volumes (CSV mount points / fixed
	// volumes), and ISO files in conventional locations. A pure read.
	CollectResources(ctx context.Context) (types.HostResources, error)

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

	// GetHostIdentity observes the host's OS computer name and AD domain
	// (or workgroup). A pure read.
	GetHostIdentity(ctx context.Context) (HostIdentity, error)

	// RenameComputer renames the OS to newName. It does not reboot — the rename
	// takes effect on the next restart, which the reconciler drives per
	// RebootPolicy. Idempotency is the caller's concern (only call when the name
	// differs).
	RenameComputer(ctx context.Context, newName string) error

	// JoinDomain joins the host to the AD domain using the given account. It does
	// not reboot — the join takes effect on the next restart, driven by the
	// reconciler per RebootPolicy. The caller only invokes it when the host is
	// not already in the desired domain. The credentials must never be logged.
	JoinDomain(ctx context.Context, domain, ouPath, username, password string) error

	// EnsureHostIP assigns the static IP in spec to its named physical adapter,
	// idempotently: OutcomeUnchanged when the address is already present,
	// OutcomeUpdated when it had to be (re)configured.
	EnsureHostIP(ctx context.Context, spec types.PhysicalNICConfig) (Outcome, error)

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

	// GetVMState observes the named VM: whether it exists and, if so, its power
	// state and best-effort runtime metrics. Pure read.
	GetVMState(ctx context.Context, name string) (VMState, error)

	// EnsureVM makes the VM described by vm exist on this host and match its
	// configuration (processor count, memory, disks, network adapters),
	// idempotently. It does not change power state — that is SetVMPowerState, so
	// the reconciler can settle configuration before driving power. A vNIC's
	// switch is expected to exist already (the networking reconcile runs first).
	//
	// Hyper-V forbids changing processor count or static startup memory while a
	// VM is running. When such a change is desired on a running VM, EnsureVM
	// applies everything it safely can and reports PendingPowerOff rather than
	// failing, so the reconciler can surface "settles after the VM is stopped"
	// instead of erroring every cycle.
	EnsureVM(ctx context.Context, vm types.VM) (VMEnsureResult, error)

	// SetVMPowerState drives the VM to the requested power state (Running or
	// Off). OutcomeUnchanged when it is already there. The reconciler only
	// requests Running/Off; Paused/Saved are observed, never requested.
	SetVMPowerState(ctx context.Context, name string, desired types.VMPowerState) (Outcome, error)

	// GetVMScreen returns a small PNG snapshot of the VM's console (the Hyper-V
	// thumbnail). A pure read; returns nil (no error) when the VM has no screen
	// to capture (e.g. it is off). Read-only — not an interactive console.
	GetVMScreen(ctx context.Context, name string) ([]byte, error)

	// CreateVMCheckpoint takes a checkpoint of the VM. An imperative one-shot
	// action (Job), not part of reconcile.
	CreateVMCheckpoint(ctx context.Context, vmName, checkpointName string) error

	// ExportVM exports the VM (config + VHDs) to a directory. Imperative Job.
	ExportVM(ctx context.Context, vmName, path string) error

	// ApplyVMCheckpoint reverts the VM to a named checkpoint. Imperative Job.
	ApplyVMCheckpoint(ctx context.Context, vmName, checkpointName string) error

	// RemoveVMCheckpoint deletes a named checkpoint. Imperative Job.
	RemoveVMCheckpoint(ctx context.Context, vmName, checkpointName string) error

	// AddClusterNode adds node to the local failover cluster (run on a current
	// member; local execution avoids the WinRM double-hop). Imperative Job.
	AddClusterNode(ctx context.Context, node string) error

	// EvictClusterNode removes node from the local failover cluster. Imperative
	// Job, run locally on a member.
	EvictClusterNode(ctx context.Context, node string) error

	// DrainNode pauses a cluster node and moves its roles off (maintenance
	// mode). Imperative Job.
	DrainNode(ctx context.Context, node string) error

	// ResumeNode brings a paused cluster node back into service. Imperative Job.
	ResumeNode(ctx context.Context, node string) error

	// MoveClusterGroup moves (fails over) a clustered role/group to node. Run
	// locally on a member. Imperative Job.
	MoveClusterGroup(ctx context.Context, group, node string) error

	// MoveClusterSharedVolume moves ownership of a CSV to node. Run locally on a
	// member. Imperative Job.
	MoveClusterSharedVolume(ctx context.Context, volume, node string) error

	// ValidateCluster runs Test-Cluster over the given nodes (empty = all
	// members) for the named test categories (empty = a safe non-disruptive
	// default) and returns a short result summary. Imperative Job.
	ValidateCluster(ctx context.Context, nodes, include []string) (string, error)
}

// VMEnsureResult is what EnsureVM did.
type VMEnsureResult struct {
	// Outcome is Created/Updated/Unchanged for the parts that were applied.
	Outcome Outcome
	// PendingPowerOff is true when a desired processor-count or static-memory
	// change could not be applied because the VM is running; it will settle once
	// the VM is stopped. Disks and adapters (hot-pluggable) are still applied.
	PendingPowerOff bool
}

// VMState is the observed state of one VM on the host.
type VMState struct {
	// Exists is true when a VM by that name is present on the host.
	Exists bool
	// PowerState is the actual power state; empty when Exists is false.
	PowerState types.VMPowerState
	// AssignedMemoryBytes, CPUUsagePercent and UptimeSeconds are best-effort
	// runtime metrics, zero when the VM is off or not observed.
	AssignedMemoryBytes uint64
	CPUUsagePercent     int
	UptimeSeconds       int64
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
	// Groups are the clustered roles/groups and their current owner node.
	Groups []ClusterGroup
	// CSVs are the Cluster Shared Volumes and their current owner node.
	CSVs []ClusterCSV
}

// ClusterGroup is one clustered role/group and its current owner.
type ClusterGroup struct {
	Name      string
	OwnerNode string
	State     string
}

// ClusterCSV is one Cluster Shared Volume and its current owner.
type ClusterCSV struct {
	Name      string
	OwnerNode string
	State     string
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

// HostIdentity is the observed OS identity of the host.
type HostIdentity struct {
	// ComputerName is the current OS hostname.
	ComputerName string
	// Domain is the AD domain the host is joined to, or the workgroup name.
	Domain string
	// PartOfDomain is true when Domain is an AD domain rather than a workgroup.
	PartOfDomain bool
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
