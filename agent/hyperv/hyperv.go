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

	// EnsureVMHostPaths sets the host's default VM config and VHD directories
	// (Set-VMHost). Idempotent: a no-op when they already match. An empty path
	// leaves that default unchanged.
	EnsureVMHostPaths(ctx context.Context, vmPath, vhdPath string) (Outcome, error)

	// EnsureHostDNS sets the host's physical NIC IPv4 DNS servers to the given
	// list (idempotent; OutcomeUnchanged when already set). Works regardless of
	// domain membership, so it can run before a domain join to let the host
	// resolve the domain's SRV records.
	EnsureHostDNS(ctx context.Context, dns []string) (Outcome, error)

	// EnsureLiveMigration configures host live migration (enable, auth type,
	// concurrency, and which networks to use). Idempotent: a no-op when the host
	// already matches.
	EnsureLiveMigration(ctx context.Context, spec types.LiveMigrationSpec) (Outcome, error)

	// RemoveSwitch deletes a virtual switch from the host (Remove-VMSwitch).
	// Imperative Job. A no-op (no error) when the switch does not exist.
	RemoveSwitch(ctx context.Context, name string) error

	// RemoveVM stops and deletes a VM from the host (hard delete). Imperative
	// Job. A no-op (no error) when the VM does not exist.
	RemoveVM(ctx context.Context, name string) error

	// FormatDisk wipes a physical disk (by PhysicalDisk DeviceId) back to a raw,
	// poolable state. Destructive imperative Job; refuses the boot/system disk.
	FormatDisk(ctx context.Context, deviceID string) error
	// FormatDiskDrive initialises a physical disk, creates a single GPT partition,
	// formats it NTFS and assigns the requested drive letter. Refuses the OS disk.
	FormatDiskDrive(ctx context.Context, deviceID, driveLetter string) error

	// RepairHostDNS fixes a common multi-homed-host misconfiguration: it points
	// every non-management NIC's DNS at the domain controller (the management
	// NIC's DNS) and disables DNS registration on those NICs, so a DHCP NIC handed
	// the router as DNS no longer breaks AD/DNS registration (event 1196) or
	// resolution. dns, when non-empty, forces the DC's DNS address (recovery).
	// Returns a short summary of what changed. Imperative Job.
	RepairHostDNS(ctx context.Context, dns string) (string, error)

	// RepairNetworkProfile sets any host NIC on the Public network profile to
	// Private. A NIC stuck on Public — e.g. after a vSwitch was created or removed
	// — silently breaks WinRM and failover clustering; a managed host NIC should be
	// on Private or the automatic Domain-authenticated profile. Domain NICs are
	// left as they are. Returns a per-NIC summary. Imperative Job (operator-run).
	RepairNetworkProfile(ctx context.Context) (string, error)

	// EnsureNetworkProfilesPrivate is the reconcile-driven, idempotent counterpart
	// of RepairNetworkProfile: it flips any NIC left on the Public profile to
	// Private and reports OutcomeUnchanged when none were (Domain-authenticated and
	// Private NICs are left alone). The reconciler runs it after switch/vNIC work
	// so a host that a vSwitch operation stranded on Public heals itself on the
	// same or next pass instead of needing the operator to run the job — keeping
	// WinRM and clustering reachable even while the centre is offline.
	EnsureNetworkProfilesPrivate(ctx context.Context) (Outcome, error)

	// DestroyCluster tears the cluster down from this node (the former): remove VM
	// roles, disable S2D, Remove-Cluster -CleanupAD. Destructive imperative Job;
	// a no-op when no cluster exists.
	DestroyCluster(ctx context.Context) error

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
	// permits it; it is never invoked speculatively. When drain is set the node
	// is gracefully drained (roles live-migrated off, S2D storage suspended)
	// before the restart.
	RebootHost(ctx context.Context, drain bool) error

	// ShutdownHost powers the host off now. Only ever invoked from an explicit
	// operator job, never speculatively. drain has the same meaning as for
	// RebootHost.
	ShutdownHost(ctx context.Context, drain bool) error

	// EnableRDP turns on Remote Desktop on the host (clears fDenyTSConnections and
	// enables the Remote Desktop firewall group). Run as an operator job before an
	// RDP connection; idempotent.
	EnableRDP(ctx context.Context) error

	// GetClusterState observes the failover cluster this node belongs to, if
	// any. It is a pure read.
	GetClusterState(ctx context.Context) (ClusterState, error)

	// EnsureFailoverClusteringFeature installs the Failover-Clustering feature
	// and its tools if absent. The feature install does not require a reboot.
	EnsureFailoverClusteringFeature(ctx context.Context) (Outcome, error)

	// GetNetworkProfile returns the weakest network-location category across the
	// host's connection profiles (Public/Private/DomainAuthenticated). A pure read.
	GetNetworkProfile(ctx context.Context) (string, error)

	// PruneManagementVNICs removes stray management-OS vNICs on the given managed
	// switches that are not in keep and carry no manual static IPv4 — auto/leftover
	// vNICs from earlier switch iterations. Never removes a declared vNIC, one with
	// a manual IP, or the last management connection on a switch.
	PruneManagementVNICs(ctx context.Context, switches, keep []string) (Outcome, error)

	// RemoveMgmtVNIC removes a management-OS vNIC by name (imperative cleanup of a
	// stray). Idempotent: a no-op when absent.
	RemoveMgmtVNIC(ctx context.Context, name string) error

	// ResetPoolDisks wipes local non-OS, non-pooled disks so S2D can claim them —
	// used to add a node's storage to the pool. Safe on an existing member (no-op).
	ResetPoolDisks(ctx context.Context) (string, error)

	// ConvergedNetworkReady reports whether the named SET switches exist and the
	// host's management IP is on a switch vNIC — the former gates cluster formation
	// on this so it never forms over pre-switch networking. True when no switches.
	ConvergedNetworkReady(ctx context.Context, switchNames []string) (bool, error)

	// EnsureClusterFirewall enables the inbound firewall rule groups a cluster
	// member needs for node-to-node coordination — Failover Clusters and WMI
	// (the latter carries the RPC/WMI calls Add-ClusterVirtualMachineRole and
	// similar make to peer nodes). Idempotent: a no-op when already enabled.
	EnsureClusterFirewall(ctx context.Context) (Outcome, error)

	// EnsureMigrationDelegation configures Kerberos constrained delegation in AD
	// between the given cluster nodes' computer accounts (the migration + cifs
	// services), which cluster-initiated live migration requires when the host
	// migration auth is Kerberos. Run on the former (a domain admin). Installs the
	// AD PowerShell module if absent. Idempotent: only adds missing delegations.
	// nodes empty = discover via Get-ClusterNode.
	EnsureMigrationDelegation(ctx context.Context, nodes []string) (Outcome, error)

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

	// EnsureS2DPoolDisks adds any poolable physical disks across the cluster to
	// the S2D pool, so a node added after S2D was enabled actually contributes its
	// disks (Add-ClusterNode does not claim a late-joiner's disks — they stay
	// CanPool). Run by the former; idempotent: OutcomeUnchanged when no disk is
	// poolable, OutcomeUpdated when disks were added.
	EnsureS2DPoolDisks(ctx context.Context) (Outcome, error)

	// EnsureCSV makes the Cluster Shared Volume described by spec exist on the
	// S2D pool, idempotently: OutcomeUnchanged when it already exists,
	// OutcomeCreated when it had to be provisioned.
	EnsureCSV(ctx context.Context, spec CSVProvision) (Outcome, error)

	// RemoveCSV deletes the Cluster Shared Volume backed by the virtual disk of
	// the given name from the S2D pool (Remove-VirtualDisk, which also removes its
	// cluster resource). Destructive imperative Job run on the former; a no-op
	// (no error) when no such volume exists.
	RemoveCSV(ctx context.Context, name string) error

	// RepairStoragePool retires and removes disks that are no longer Healthy from
	// the S2D pool so it returns to Healthy (e.g. a departed node's orphaned disks
	// after a teardown). Returns a short summary. Idempotent: a no-op when all
	// disks are Healthy.
	RepairStoragePool(ctx context.Context) (string, error)

	// RebuildStoragePool DESTROYS the S2D pool and its volumes and re-enables S2D
	// to create a fresh pool from the cluster's current disks. For a stale/degraded
	// pool left over from a torn-down cluster that Repair cannot salvage. All data
	// on the pool is lost; gated behind an explicit operator action.
	RebuildStoragePool(ctx context.Context) (string, error)

	// GetVMState observes the named VM: whether it exists and, if so, its power
	// state and best-effort runtime metrics. Pure read.
	GetVMState(ctx context.Context, name string) (VMState, error)

	// ListObservedVMs enumerates every VM present on the host with enough config
	// to display and adopt it (power, generation, CPU/memory, disks, adapters,
	// clustered flag, replication). This is how the centre discovers VMs when
	// Ballast is added to existing infrastructure. Pure read.
	ListObservedVMs(ctx context.Context) ([]types.ObservedVM, error)

	// WatchVMState subscribes to VM power-state changes on the host and calls
	// onEvent once per change, so the agent can re-observe immediately instead of
	// waiting for the next polled cycle. It blocks until ctx is cancelled or the
	// subscription ends (provider restart, error), returning the reason so the
	// caller can re-establish it. The callback carries NO state: it only nudges a
	// re-observe — the reconcile still reads actual vs desired itself, so a missed
	// or spurious event is harmless and the periodic cycle remains the source of
	// truth. Purely an optimisation layered on the poll.
	WatchVMState(ctx context.Context, onEvent func()) error

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

	// RestartVM restarts a running VM — a one-shot imperative action (power is
	// never continuously enforced). Errors if the VM is not running.
	RestartVM(ctx context.Context, name string) error

	// GetVMScreen returns a small PNG snapshot of the VM's console (the Hyper-V
	// thumbnail). A pure read; returns nil (no error) when the VM has no screen
	// to capture (e.g. it is off). Read-only — not an interactive console.
	GetVMScreen(ctx context.Context, name string) ([]byte, error)

	// CreateVMCheckpoint takes a checkpoint of the VM. An imperative one-shot
	// action (Job), not part of reconcile.
	CreateVMCheckpoint(ctx context.Context, vmName, checkpointName string) error

	// ExportVM exports the VM (config + VHDs) to a directory. Imperative Job.
	ExportVM(ctx context.Context, vmName, path string) error

	// CloneVM makes an independent copy of an Off source VM into folder: it copies
	// the source's VHD(s), creates a new VM (fresh identity, dynamic MAC) matching
	// the source's generation/CPU/memory, and connects the NIC to the source's
	// switch. The source must be Off (its disk is locked while running). Imperative
	// Job. The centre then adopts the new VM into desired state.
	CloneVM(ctx context.Context, srcName, newName, folder string) error

	// CaptureTemplate copies vmName's first VHDX to dest (a path in the template
	// library) and returns the captured image's size in bytes. When generalise is
	// set it first runs sysprep /generalize in the guest over PowerShell Direct,
	// which needs a guest-local administrator credential and waits for the guest
	// to shut itself down; the source VM is left generalised, i.e. no longer a
	// usable machine. Otherwise the VM must already be Off. Imperative Job.
	CaptureTemplate(ctx context.Context, vmName, dest string, generalise bool, guestUser, guestPass string) (uint64, error)

	// DeployFromTemplate copies a template image from src to dest and, when
	// unattend is non-empty, mounts the copy and writes it to
	// \Windows\Panther\Unattend.xml so the guest customises itself on first boot.
	// It does NOT create the VM: the centre authors the VM's desired state when
	// this job succeeds and the reconcile loop builds it. Imperative Job.
	DeployFromTemplate(ctx context.Context, src, dest, unattend string) error

	// FetchISO downloads an ISO from url (the centre's ISO library over HTTP) to
	// dest on this host, creating dest's parent folder. Used to place an uploaded
	// ISO onto a CSV so any node can boot a VM from it. Agent-local (no WinRM) and
	// idempotent: a no-op when dest already exists. Imperative Job.
	//
	// Returns a note describing anything notable about how the transfer ran —
	// empty on the normal BITS path, and naming why BITS was skipped when the
	// Invoke-WebRequest fallback was used. That fallback is the fragile one, so an
	// operator needs to see that it happened even on success.
	FetchISO(ctx context.Context, url, dest string) (string, error)

	// GuestJoinDomain joins the VM's guest OS to domain (then reboots the guest)
	// via PowerShell Direct. guestUser/guestPass authenticate into the guest;
	// domainUser/domainPass authorise the join. Credentials must never be logged.
	GuestJoinDomain(ctx context.Context, vmName, domain, ouPath, guestUser, guestPass, domainUser, domainPass string) error

	// GuestSetIP sets a static IPv4 (addr in CIDR) on the guest's adapter via
	// PowerShell Direct. iface empty picks the first connected adapter; gateway
	// and dns (comma-separated) are optional.
	GuestSetIP(ctx context.Context, vmName, iface, addr, gateway, dns, guestUser, guestPass string) error

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

	// EnsureClusterVMRole registers an existing VM as a highly-available cluster
	// role (Add-ClusterVirtualMachineRole), so Failover Clustering owns its
	// placement and failover. Idempotent: a no-op once the role exists. Run on the
	// VM's owner node.
	EnsureClusterVMRole(ctx context.Context, vmName string) (Outcome, error)

	// MoveClusterGroup moves (fails over) a clustered role/group to node. Run
	// locally on a member. Imperative Job.
	MoveClusterGroup(ctx context.Context, group, node string) error

	// MoveClusterSharedVolume moves ownership of a CSV to node. Run locally on a
	// member. Imperative Job.
	MoveClusterSharedVolume(ctx context.Context, volume, node string) error

	// MoveClusterVM live-migrates a highly-available VM role to node with no
	// downtime (Move-ClusterVirtualMachineRole -MigrationType Live). Run locally
	// on a member. Imperative Job.
	// MoveClusterVM live-migrates a clustered VM role to node. onProgress (nil-safe)
	// receives streamed progress notes ("live migration N%") polled from
	// Msvm_MigrationJob while the move runs.
	MoveClusterVM(ctx context.Context, vm, node string, onProgress ProgressFunc) error

	// MigrateVM shared-nothing live-migrates a standalone (non-clustered) VM to
	// another host with no shared storage: Move-VM -DestinationHost -IncludeStorage
	// moves the VM and its files to destPath on the target. Run on the source host.
	// A running VM migrates live; a stopped one moves offline. It enables migration
	// on the source; the destination must also have it enabled. Kerberos delegation
	// between the two computer accounts is provisioned by the MigrateVM job (via
	// EnsureMigrationDelegation) before this runs. onProgress (nil-safe) receives
	// streamed progress notes. Imperative Job.
	MigrateVM(ctx context.Context, vm, destHost, destPath string, onProgress ProgressFunc) (string, error)

	// ValidateCluster runs Test-Cluster over the given nodes (empty = all
	// members) for the named test categories (empty = a safe non-disruptive
	// default) and returns a short result summary. Imperative Job.
	ValidateCluster(ctx context.Context, nodes, include []string) (string, error)

	// ClusterLog runs Get-ClusterLog for the recent window (span minutes) on this
	// node and returns the lines relevant to migration/errors (optionally also
	// matching filter, e.g. a VM name) — the per-operation detail the Windows
	// event log does not fully capture. Imperative Job.
	ClusterLog(ctx context.Context, span, filter string) (string, error)

	// EnsureReplicaServer configures this host to accept Hyper-V Replica
	// traffic: Set-VMReplicationServer plus the replica firewall listener rule.
	// Idempotent.
	EnsureReplicaServer(ctx context.Context, spec types.ReplicaServerSpec) (Outcome, error)

	// EnsureReplicaBroker provisions the Hyper-V Replica Broker cluster role
	// (client access point + broker resource). Run on the former. Idempotent.
	EnsureReplicaBroker(ctx context.Context, spec types.ReplicaBrokerSpec) (Outcome, error)

	// RemoveReplicaBroker deletes the Hyper-V Replica Broker cluster role and the
	// client access point it lives in. Run on the former. Idempotent: a cluster
	// with no broker is a no-op. The caller must clear the declared broker from
	// desired state first, or the next reconcile provisions it again.
	//
	// group optionally names the broker's cluster group, and is removed even if it
	// no longer holds a broker resource — a broker resource deleted on its own
	// strands the client access point, which still holds the name and IP.
	RemoveReplicaBroker(ctx context.Context, group string) (string, error)

	// EnsureVMReplication drives one VM's Hyper-V Replica relationship to the
	// declared spec: enable + initial replication when absent, adjust when it
	// drifts, remove when Enabled is false. Idempotent.
	EnsureVMReplication(ctx context.Context, vmName string, spec types.VMReplicationSpec) (Outcome, error)

	// The failover operations below are imperative Jobs, all run on the REPLICA
	// host (the target the VM replicates to). They are one-shot events, never
	// reconciled: the centre records the outcome and rewrites desired state on
	// success so the reconcile loop honours the inverted topology afterwards.

	// TestFailover starts a non-disruptive test failover (a temporary test VM);
	// the primary keeps running. network, when set, is the switch on this host to
	// connect the test VM's adapters to. Returns a short detail.
	TestFailover(ctx context.Context, vmName, network string) (string, error)

	// StopTestFailover tears down a test failover, removing the temporary VM.
	StopTestFailover(ctx context.Context, vmName string) error

	// PlannedFailover performs a zero-data-loss planned failover from primaryHost
	// to this replica host and reverses replication so the old primary becomes the
	// new replica. network, when set, is the switch on this host to connect the
	// VM's adapters to. Returns a short detail.
	PlannedFailover(ctx context.Context, vmName, primaryHost, network string) (string, error)

	// Failover performs an unplanned failover after the primary is lost, from the
	// latest replica data or the named recovery point. network, when set, is the
	// switch on this host to connect the VM's adapters to. Returns a short detail.
	Failover(ctx context.Context, vmName, recoveryPoint, network string) (string, error)

	// CancelFailover reverts a test or unplanned failover (Stop-VMFailover).
	CancelFailover(ctx context.Context, vmName string) error

	// ReverseReplication commits a pending failover and reverses replication so
	// the new primary replicates back to the former primary. Returns a detail.
	ReverseReplication(ctx context.Context, vmName string) (string, error)

	// RemoveReplicaVM removes an orphaned replica copy on this host (the replica
	// relationship, the VM, and its replica VHDs). Refuses to run unless the VM
	// here is a Replica, so it can never delete a primary/standalone VM.
	RemoveReplicaVM(ctx context.Context, vmName string) error
}

// VMEnsureResult is what EnsureVM did.
type VMEnsureResult struct {
	// Outcome is Created/Updated/Unchanged for the parts that were applied.
	Outcome Outcome
	// PendingPowerOff is true when a desired processor-count or static-memory
	// change could not be applied because the VM is running; it will settle once
	// the VM is stopped. Disks and adapters (hot-pluggable) are still applied.
	PendingPowerOff bool
	// PendingDetail names WHICH change is waiting, with wanted-vs-actual. Without
	// it a VM stuck Progressing gives no clue which of four checks is unsatisfied.
	PendingDetail string
}

// VMState is the observed state of one VM on the host.
type VMState struct {
	// Exists is true when a VM by that name is present on the host.
	Exists bool
	// ID is the VM's Hyper-V GUID (Get-VM .Id). The centre uses it as the
	// preconnection-blob to open the VM's console over RDP (VMConnect). Empty
	// when the VM does not exist.
	ID string
	// GuestOS is the guest OS name (integration-services KVP); IPAddress is the
	// guest's address(es), comma-separated. Both empty until the guest is up with
	// integration services.
	GuestOS   string
	IPAddress string
	// GuestFQDN is the guest's fully-qualified domain name from the KVP exchange
	// (e.g. "host.ballast.local" when domain-joined, just "host" in a workgroup).
	GuestFQDN string
	// PowerState is the actual power state; empty when Exists is false.
	PowerState types.VMPowerState
	// AssignedMemoryBytes, CPUUsagePercent and UptimeSeconds are best-effort
	// runtime metrics, zero when the VM is off or not observed.
	AssignedMemoryBytes uint64
	// MemoryDemandBytes is what the guest actually wants. Assigned equals startup
	// for a static-memory VM, so it cannot express usage; demand can. Zero when
	// the VM is off or its integration services are not reporting.
	MemoryDemandBytes uint64
	// MemoryStatus is Hyper-V's own verdict: "OK", "Low", "Warning".
	MemoryStatus    string
	CPUUsagePercent int
	UptimeSeconds   int64
	// Checkpoints is the VM's current set of Hyper-V checkpoints (snapshots).
	Checkpoints []types.VMCheckpoint
	// Observed is the VM's actual configuration (CPU/memory/disks/adapters), for
	// showing and adopting VMs Ballast did not create.
	Observed *types.VMObserved
	// Replication is the VM's observed Hyper-V Replica state; nil when the VM
	// has no replication relationship.
	Replication *types.VMReplicationStatus
}

// StorageState is the observed S2D/CSV state on the cluster.
type StorageState struct {
	// S2DEnabled is true when Storage Spaces Direct is on and the pool exists.
	S2DEnabled bool
	// S2DKnown is true when the S2D state could actually be determined. When false
	// (e.g. the query was starved under heavy I/O), S2DEnabled is not trustworthy
	// and callers must NOT act on a false reading — never enable on an unknown.
	S2DKnown bool
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
	// Known is false when the cluster state could not be determined this pass (e.g.
	// the cluster service was momentarily unavailable but the node IS clustered).
	// When false, Exists is reported true (to avoid a spurious New-Cluster) but the
	// detail fields are empty, so the reconciler must defer rather than clobber.
	Known bool
	// Name is the cluster's name (empty when Exists is false).
	Name string
	// Members are the node names currently in the cluster.
	Members []string
	// Nodes are the cluster nodes with their current state (Up/Paused/Down).
	Nodes []ClusterNodeState
	// Groups are the clustered roles/groups and their current owner node.
	Groups []ClusterGroup
	// CSVs are the Cluster Shared Volumes and their current owner node.
	CSVs []ClusterCSV
	// VMs are the highly-available VM roles and their current owner node.
	VMs []ClusterVM
	// Pool is the S2D storage pool's capacity, when one exists.
	Pool *ClusterPool
	// Networks are the cluster's networks (Get-ClusterNetwork) with their subnet,
	// role and state — used to select a live-migration network and to surface a
	// partitioned/down network.
	Networks []ClusterNetworkInfo
}

// ClusterPool is the S2D storage pool's name and capacity (raw total and the
// portion already allocated to volumes); free is Raw - Allocated.
type ClusterPool struct {
	Name           string
	RawBytes       uint64
	AllocatedBytes uint64
	Health         string
	Operational    string
	UnhealthyDisks int
	TotalDisks     int
	// Resyncing and its detail report an observed repair/regeneration job, so a
	// pool that is rebuilding itself is not mistaken for one that is broken.
	Resyncing     bool
	ResyncPercent int
	// ResyncRemainingBytes is the outstanding work across the running jobs. The
	// percentage resets when one job ends and the next begins; this does not, so
	// it is what tells a converging rebuild from one that keeps restarting.
	ResyncRemainingBytes uint64
	ResyncJob            string
}

// ClusterNetworkInfo is one cluster network: its name, subnet (CIDR), role
// (None/Cluster/ClusterAndClient) and state (Up/Down/Partitioned/Unavailable).
type ClusterNetworkInfo struct {
	Name  string
	CIDR  string
	Role  string
	State string
	// Metric decides which network carries cluster and CSV/SMB traffic — lowest
	// wins among those enabled for cluster use.
	Metric int
}

// ClusterVM is one highly-available VM role and its current owner.
type ClusterVM struct {
	Name      string
	OwnerNode string
	State     string
}

// ClusterGroup is one clustered role/group and its current owner.
type ClusterGroup struct {
	Name      string
	OwnerNode string
	State     string
	GroupType string
}

// ClusterNodeState is a cluster node and its current state (Up, Paused — i.e.
// drained/maintenance — or Down).
type ClusterNodeState struct {
	Name  string
	State string
}

// ClusterCSV is one Cluster Shared Volume and its current owner.
type ClusterCSV struct {
	Name      string
	OwnerNode string
	State     string
	// Health of the BACKING VIRTUAL DISK, observed separately from the pool's so
	// a volume fault is not attributed to healthy storage underneath it.
	Health         string
	Operational    string
	DetachedReason string
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
