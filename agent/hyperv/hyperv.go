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

	// EnsureAdapterMTU sets the named physical adapters to carry an MTU of want,
	// choosing the value each driver actually offers rather than writing the
	// number verbatim — vendors count *JumboPacket differently and most accept
	// only values from their own menu.
	//
	// Idempotent, and that matters more here than usual: writing the property
	// resets the miniport, so an apply that ran every pass would bounce every
	// uplink in the fleet on every reconcile. Nothing is written when the
	// adapter's IP interface already reports the MTU.
	EnsureAdapterMTU(ctx context.Context, adapters []string, want int) (Outcome, error)

	// DisableLSO turns Large Send Offload off on a switch's uplinks and the
	// management vNICs over it. The third thing that has to agree for jumbo
	// frames, and the only one no configuration anywhere shows is wrong.
	//
	// LSO and not RSC: on the rig, RSC is enabled on every uplink of the hosts
	// where jumbo works. Verified against a fresh read, because the cmdlet is
	// silent on success and on a driver that declines.
	DisableLSO(ctx context.Context, adapters, switches []string) (string, error)

	// AdapterMTUs observes what the named adapters carry, and what their drivers
	// will accept. A pure read, folded into HostInventory.
	AdapterMTUs(ctx context.Context, adapters []string) ([]AdapterMTU, error)

	// EnsureInterfaceMTU sets the IPv4 interface MTU on a management vNIC. A
	// different layer from the adapter property: the uplink decides what can
	// cross the wire, this decides what the stack will put on it.
	//
	// mayCycle allows the adapter to be restarted, which is what actually puts
	// the change in force — Set-NetIPInterface resets nothing, and no reading
	// shows the difference. False for the management vNIC, which carries the
	// host's address and the agent's link to the centre.
	EnsureInterfaceMTU(ctx context.Context, vnicName string, want int, mayCycle bool) (Outcome, error)

	// EnsureMgmtVNIC makes the management OS vNIC described by spec exist on its
	// switch and carry its VLAN, IP and QoS-weight intent. The switch named by
	// spec.SwitchName is expected to exist already; the reconciler ensures
	// switches before vNICs.
	EnsureMgmtVNIC(ctx context.Context, spec types.ManagementVNICSpec) (Outcome, error)

	// EnsureMgmtVNICs reconciles a whole set of management vNICs, observing them
	// in one pass. Returns an Outcome and an error per spec, in order, so each
	// vNIC still reports against its own condition.
	//
	// Exists because the per-vNIC path costs two PowerShell invocations before it
	// can decide it has nothing to do, and each pays a fresh module load — ~7s per
	// vNIC on the rig, so a three-vNIC converged host burned ~21s of a 40s host
	// reconcile confirming that nothing had changed.
	EnsureMgmtVNICs(ctx context.Context, specs []types.ManagementVNICSpec) ([]Outcome, []error)

	// ClusterVMRolesPresent reports which of the named VMs already exist as
	// highly-available cluster roles, in one query. EnsureClusterVMRole answers
	// that for a single VM by enumerating every cluster group, so reconciling n
	// VMs ran the same cluster-wide query n times — 2437ms each on the rig.
	ClusterVMRolesPresent(ctx context.Context, names []string) (map[string]bool, error)

	// GetVMLiveStates observes what a running VM can change about itself —
	// power, memory, CPU, uptime, IPs, replication — for every named VM in ONE
	// invocation, keyed by lower-cased name.
	//
	// The counterpart to GetVMState, which reads everything about one VM at
	// ~1.4s a time. Most of what that returns (processor count, memory config,
	// generation, disks, adapters, VM ID) settles on a power cycle or an
	// explicit job, so re-reading it every pass bought nothing and scaled with
	// VM count.
	GetVMLiveStates(ctx context.Context, names []string) (map[string]VMLive, error)

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

	// EnsureClusterIP drives the cluster's own IP Address resource to the declared
	// address. ManagementIP used to be read only by New-Cluster at formation, so
	// changing it on a formed cluster was stored, generated a new generation, and
	// did nothing — with no condition anywhere saying it had not been honoured.
	//
	// Idempotent: an address that already matches is a no-op and nothing is taken
	// offline. Returns a short note describing what it did. Empty ip is not
	// declared and must not reach here.
	EnsureClusterIP(ctx context.Context, ip string) (Outcome, string, error)

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
	// Returns any DUPLICATE declared vNIC it found — two management OS vNICs with
	// the same name on one switch, which the prune cannot remove because the name
	// is in the keep set, and which nothing else can see.
	PruneManagementVNICs(ctx context.Context, switches, keep []string) (Outcome, []string, error)

	// RemoveMgmtVNIC removes a management-OS vNIC by name (imperative cleanup of a
	// stray). Idempotent: a no-op when absent.
	RemoveMgmtVNIC(ctx context.Context, name string) error

	// ResetPoolDisks wipes local non-OS, non-pooled disks so S2D can claim them —
	// used to add a node's storage to the pool. Safe on an existing member (no-op).
	ResetPoolDisks(ctx context.Context) (string, error)

	// ReleasePoolDisks releases disks a storage pool still claims on a host that
	// is NOT a cluster member: it destroys the leftover pool and resets pool
	// membership so the disks return to CanPool. deviceID names one disk (unique
	// id preferred, device id accepted); empty releases every non-OS local disk.
	// Refuses on a clustered host — there the pool is live and owned by S2D.
	// Returns a per-disk summary. Destructive.
	ReleasePoolDisks(ctx context.Context, deviceID string) (string, error)

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
	RemoveCSV(ctx context.Context, name string) (string, error)

	// RepairStoragePool retires and removes disks that are no longer Healthy from
	// the S2D pool so it returns to Healthy (e.g. a departed node's orphaned disks
	// after a teardown). Returns a short summary. Idempotent: a no-op when all
	// disks are Healthy.
	RepairStoragePool(ctx context.Context) (string, error)

	// UpdateClusterFunctionalLevel raises the cluster's operating mode to what its
	// nodes now support, after a rolling OS upgrade. Run on the former.
	// IRREVERSIBLE, so it is only ever an explicit operator action.
	UpdateClusterFunctionalLevel(ctx context.Context) (string, error)

	// EnsureISCSI connects this node to an iSCSI array — service, portals,
	// persistent logins, MPIO — and reports what it can see. Additive only: it
	// never disconnects a session or removes a portal.
	//
	// shared marks a CLUSTER member, whose newly arrived LUNs must NOT be brought
	// online automatically: a shared disk mounted on two nodes at once is the state
	// clustering exists to prevent, and the cluster will not take it. A standalone
	// host is the opposite — its LUN should come online to be provisioned.
	EnsureISCSI(ctx context.Context, spec types.ISCSIStorageSpec, chapUser, chapSecret string, shared bool, storageAddresses []string) (ISCSIState, Outcome, error)

	// AdoptISCSIDisk takes an array-presented LUN into the cluster, as a Cluster
	// Shared Volume or as the witness disk, and reports the disk's serial so a
	// volume authored by target can be pinned to a cluster-wide identifier.
	//
	// It REFUSES a LUN that already carries a partition or filesystem unless the
	// adoption asks to wipe it. Adoption formats the disk, and an array presents
	// LUNs to whoever it is told to: a serial typed one character out, or a LUN
	// re-presented from another cluster, is indistinguishable from a new one right
	// up to the moment its contents are gone.
	AdoptISCSIDisk(ctx context.Context, a ISCSIAdoption) (serial, note string, out Outcome, err error)

	// EnsureCSVMountPoints makes each named CSV's mount point match its declared
	// volume name, for the CSVs THIS NODE OWNS. want maps CSV name to the directory
	// leaf it should have; the returned note explains any it could not do.
	//
	// Per node, not per cluster, because renaming a mount point belongs with owning
	// the volume — and a cluster's volumes are not all owned by one member, so a
	// former-only step could never fix them all.
	EnsureCSVMountPoints(ctx context.Context, want map[string]string) (Outcome, string, error)

	// GetWindowsLicence observes the host's Windows edition and activation state.
	// A pure read; it never changes licensing.
	//
	// The edition matters as much as the activation: an EVALUATION edition cannot
	// be activated by any key, so a host reported as unlicensed with a countdown
	// has no remedy an operator would guess at — it needs converting first.
	GetWindowsLicence(ctx context.Context) (WindowsLicence, error)

	// EnsureWindowsEdition converts the host to targetEdition when it differs,
	// returning rebootRequired true when a conversion was staged — the change takes
	// effect only on restart.
	//
	// IRREVERSIBLE, so it refuses anything it is not certain of: it asks Windows
	// which target editions are valid rather than reasoning about which conversions
	// are legal, and refuses a domain controller, which Windows cannot convert.
	EnsureWindowsEdition(ctx context.Context, targetEdition, productKey string) (Outcome, bool, error)

	// EnsureWindowsActivation activates Windows by MAK or against a KMS host.
	//
	// Idempotent in the way that matters: a host already licensed, whose key and
	// KMS server already match, is not activated again — a repeat MAK activation
	// consumes another seat from the key's pool. An evaluation edition is refused
	// before anything is attempted, since no key can activate one.
	EnsureWindowsActivation(ctx context.Context, method, key, kmsServer string) (Outcome, error)

	// EnsureGuestAVMA installs an Automatic Virtual Machine Activation key inside
	// a guest so it activates against this host.
	//
	// Refuses on a host that cannot vouch for a guest — Standard, evaluation, or
	// not itself activated — BEFORE touching the guest, because that failure is the
	// host's and an error naming the guest sends an operator to the wrong machine.
	EnsureGuestAVMA(ctx context.Context, vmName, avmaKey, guestUser, guestPass string) (Outcome, error)

	// CheckISOLibrary probes an SMB boot-media share both as the agent and as the
	// node's computer account — the way Hyper-V will actually attach media. Read
	// only; it mounts nothing.
	CheckISOLibrary(ctx context.Context, path string) (ISOLibraryState, error)

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
	// discardSaved throws away a saved VM's memory image first, so it becomes Off
	// and can be copied. Ignored unless the VM is Saved.
	CaptureTemplate(ctx context.Context, vmName, dest string, generalise, discardSaved bool, guestUser, guestPass string, onProgress ProgressFunc) (uint64, error)

	// MoveVMStorage relocates a VM's files into folder without moving the VM —
	// Hyper-V storage migration, which runs live. Every destination path is
	// dictated explicitly so the result is predictable and the centre can author
	// the new disk paths into desired state. Reports progress. Imperative Job.
	MoveVMStorage(ctx context.Context, vm, folder string, onProgress ProgressFunc) (string, error)

	// DiscardVMSavedState turns a Saved VM into an Off one by throwing away its
	// saved memory. A clustered VM is Saved rather than Off whenever its role
	// goes offline (AutomaticStopAction defaults to Save), and a saved VM cannot
	// be captured, cloned or have its storage moved. Already-Off is a no-op.
	DiscardVMSavedState(ctx context.Context, vmName string) error

	// DeployFromTemplate copies a template image from src to dest and, when
	// unattend is non-empty, mounts the copy and writes it to
	// \Windows\Panther\Unattend.xml so the guest customises itself on first boot.
	// It does NOT create the VM: the centre authors the VM's desired state when
	// this job succeeds and the reconcile loop builds it. Imperative Job.
	DeployFromTemplate(ctx context.Context, src, dest, unattend string, onProgress ProgressFunc) error

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
	GuestJoinDomain(ctx context.Context, vmName, domain, ouPath, newName, guestUser, guestPass, domainUser, domainPass string) error

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

	// EnsureNodeMaintenance reads a cluster node's availability and, when the
	// intent says so, drives it —
	// paused and drained, or back in service — and reports what the cluster says.
	// Idempotent, and a no-op on a host that is not a cluster member, where
	// maintenance is a centre-side fact with nothing to enforce locally.
	//
	// This is the declarative counterpart to the Drain/Resume jobs below: the jobs
	// act once, this keeps the intent true across reboots and rejoins.
	// deepStorage asks for the STORAGE half as well: whether this node's disks are
	// still marked out of the pool. That read is cluster-wide (Get-PhysicalDisk in
	// an S2D cluster returns every disk in the cluster) and was measured at 3m53s
	// of a 4m27s pass, so the caller asks for it when it can matter rather than on
	// every heartbeat. The node's own cluster state is always read; a paused node
	// and a pass that changes anything read storage regardless of this flag.
	EnsureNodeMaintenance(ctx context.Context, node string, intent MaintenanceIntent, deepStorage bool) (Outcome, NodeMaintenanceState, error)

	// ResumeNode brings a paused cluster node back into service. Imperative Job.
	ResumeNode(ctx context.Context, node string) error

	// EnsureClusterVMRole registers an existing VM as a highly-available cluster
	// role (Add-ClusterVirtualMachineRole), so Failover Clustering owns its
	// placement and failover. Idempotent: a no-op once the role exists. Run on the
	// VM's owner node.
	EnsureClusterVMRole(ctx context.Context, vmName string) (Outcome, error)

	// MoveClusterGroup moves (fails over) a clustered role/group to node. Run
	// locally on a member. Imperative Job.
	// StartClusterCoreGroup brings the cluster's core group online, then the
	// storage stranded behind it. Returns what was started.
	StartClusterCoreGroup(ctx context.Context) (Outcome, string, error)

	// StartClusterVolume asks the cluster to bring one CSV or clustered disk
	// online. Narrower than the core-group repair above: that one exists because
	// the whole cluster is down and everything else is a symptom; this is an
	// operator pointing at one volume.
	StartClusterVolume(ctx context.Context, volume string) (Outcome, string, error)

	// ClearISCSIFavourites removes stale persistent logins for the declared
	// targets. Nothing is disconnected; see powershell_favourites.go.
	ClearISCSIFavourites(ctx context.Context, targets []string) (string, error)

	// GetNodeSelf reads this host's OWN cluster membership state. Answerable when
	// the cluster itself is not, which is the point of it.
	GetNodeSelf(ctx context.Context) (NodeSelf, error)

	// TakeTimings returns how long each call spent in PowerShell since the last
	// take, slowest first, and clears the record. Taken rather than read so each
	// pass reports its own cost rather than an average that hides one slow pass
	// among fast ones.
	TakeTimings() []CallTiming

	// ClearNodeQuarantine readmits a quarantined node. Run from another member.
	ClearNodeQuarantine(ctx context.Context, node string) (Outcome, string, error)

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

	// MigrateVM shared-nothing live-migrates a VM to
	// another host with no shared storage: Move-VM -DestinationHost -IncludeStorage
	// moves the VM and its files to destPath on the target. Run on the source host.
	// A running VM migrates live; a stopped one moves offline. It enables migration
	// on the source; the destination must also have it enabled. Kerberos delegation
	// between the two computer accounts is provisioned by the MigrateVM job (via
	// EnsureMigrationDelegation) before this runs. onProgress (nil-safe) receives
	// streamed progress notes. Imperative Job.
	// sourceCluster and targetCluster are the cluster halves of a cross-boundary
	// move, either or both empty for a standalone end. A clustered VM is owned by
	// its cluster and Move-VM will not touch it, so the HA role comes off the
	// source first and goes on at the destination afterwards.
	// networkMap points each source vSwitch at one on the destination. Without
	// it a move onto a host that names its switches differently fails outright
	// on compatibility, which is most of what an evacuation into a new
	// environment meets. A source network with no entry arrives DISCONNECTED.
	MigrateVM(ctx context.Context, vm, destHost, destPath, sourceCluster, targetCluster string, networkMap []types.EvacuationNIC, onProgress ProgressFunc) (string, error)

	/* CopyVM moves a VM by exporting it to the destination's storage, importing
	   it there and removing the original. Run on the source host; the guest must
	   be off.

	   The path that asks least of the two hosts. Move-VM needs live migration
	   configured at both ends, Kerberos delegation between the computer accounts
	   and a compatible destination; this needs SMB and somewhere to write. It is
	   also the form whose compatibility fixing works — Compare-VM's report is
	   resolvable on the host holding the files, which a report from
	   Compare-VM -DestinationHost is not. */
	CopyVM(ctx context.Context, vm, destHost, destPath, sourceCluster, targetCluster string, networkMap []types.EvacuationNIC, onProgress ProgressFunc) (string, error)

	// ClearVMExport removes an export a failed CopyVM left on the destination,
	// which otherwise refuses every retry. Run on the source host. Refuses to
	// touch files a VM is registered against.
	ClearVMExport(ctx context.Context, vm, destHost, destPath string) (string, error)

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

	// EnsureClusterWitness makes the cluster's quorum witness match the spec.
	// Former-only. Idempotent, and deliberately so: Set-ClusterQuorum recreates
	// the witness resource even when re-applying the same value, which drops a
	// vote for a moment, so re-applying every pass would be a recurring wobble
	// rather than a no-op.
	EnsureClusterWitness(ctx context.Context, w types.WitnessSpec, kind types.ClusterStorageKind) (Outcome, error)

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

	// DestroyS2D removes the cluster's shared volumes, virtual disks and storage
	// pool and disables Storage Spaces Direct; wipeDisks additionally returns the
	// local pool disks to raw. DESTRUCTIVE and operator-initiated. Refuses while
	// any clustered VM role still exists.
	DestroyS2D(ctx context.Context, wipeDisks bool) (string, error)

	// ResetISCSIInitiator clears every iSCSI session, persistent login and
	// discovery portal on this host, so the reconcile rebuilds the initiator from
	// the declared spec. DESTRUCTIVE and operator-initiated; refuses while any
	// iSCSI disk is clustered or online, checked before anything is changed.
	ResetISCSIInitiator(ctx context.Context) (string, error)

	// DisconnectISCSITarget logs this host out of one iSCSI target and clears its
	// persistent entry so the login does not return at boot. Operator-initiated:
	// the reconcile is additive and never retires a login on its own. Refuses
	// while the target's disks are clustered or online.
	DisconnectISCSITarget(ctx context.Context, targetIQN string) (string, error)

	// RepairISCSIPortals re-registers discovery portals whose source binding names
	// an address this host no longer has. Operator-initiated, because the fix
	// removes a portal entry and the iSCSI reconcile is strictly additive.
	// Narrow: a binding to an address the host DOES have is deliberate and is
	// left alone.
	RepairISCSIPortals(ctx context.Context) (string, error)

	// RediscoverISCSI clears every discovery portal so the reconcile rebuilds
	// them from the spec. Leaves sessions and persistent logins alone.
	RediscoverISCSI(ctx context.Context) (string, error)
	// PruneISCSIPortals removes discovery portals the declared list does not name.
	PruneISCSIPortals(ctx context.Context, declared, declaredTargets []string) (string, error)
	// AdoptISCSIDiskWithContents adopts a LUN the reconcile refused because it
	// already holds data: keep the existing volume, or wipe and format it.
	AdoptISCSIDiskWithContents(ctx context.Context, a ISCSIAdoption) (string, error)

	// ScanImportableVMs walks the given storage roots for VM configurations no
	// host has registered, with Compare-VM's verdict on each.
	//
	// Returns a scan record even when it finds nothing, and especially when it
	// FAILS: an empty list and a failed walk read identically to a console, and
	// the difference is whether "this volume holds no VMs" is a fact or a guess.
	ScanImportableVMs(ctx context.Context, roots []string) ([]types.ImportableVM, *types.ImportScanStatus, error)

	// ImportVM registers a VM whose files already sit on this host's storage.
	// Refuses an already-registered ID and open files; see VMImport.
	ImportVM(ctx context.Context, v VMImport) (string, error)

	// EnsureTestSwitch ensures the isolated switch a test failover runs inside
	// exists on this host. switchType is "Private" or "Internal". Idempotent; it
	// REFUSES rather than reconfiguring a switch that already exists as
	// something else, because a test failover attached to an external switch
	// puts duplicates of live machines on the production network.
	EnsureTestSwitch(ctx context.Context, name, switchType string) (string, error)

	// RemoveTestSwitch removes a test bubble. A no-op when it is already gone;
	// refuses while VMs are still attached to it.
	RemoveTestSwitch(ctx context.Context, name string) error

	// HealthProbe answers one question about one VM, once: is it heartbeating,
	// is a port open, does it answer a ping, does a script on this host say it is
	// healthy. A failing probe returns an error carrying what actually happened,
	// because that string is what an operator watching a tier that will not come
	// up has to read.
	//
	// One shot by design — the centre paces and retries. See
	// types.JobVMHealthProbe.
	HealthProbe(ctx context.Context, vmName, check, address string, port, expectExit int, script string) (string, error)

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
	// UnknownReason says why the state could not be read, when it could not.
	// "Unreadable this pass" is equally true of a cluster service still starting
	// and of a node the cluster has QUARANTINED, and only one of those clears
	// itself — so the reason travels with the deferral rather than the operator
	// being left to guess which they are waiting on.
	UnknownReason string
	// Name is the cluster's name (empty when Exists is false).
	Name string
	// Members are the node names currently in the cluster.
	Members []string
	// Nodes are the cluster nodes with their current state (Up/Paused/Down).
	Nodes []ClusterNodeState
	// Groups are the clustered roles/groups and their current owner node.
	Groups []ClusterGroup
	// CoreResources are the resources of the cluster's own core group — the
	// cluster name and one IP address per subnet. "Cluster Group is Pending"
	// names the group and not the fault, and which resource is down (and why) is
	// what an operator actually needs.
	CoreResources []ClusterCoreResource
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
	// Witness is the cluster's observed quorum configuration. Nil when it could
	// not be read this pass; a cluster with no witness reports Type "None", which
	// is a different and far more interesting answer than "unknown".
	Witness *ClusterWitness
	// ReplicaBroker is the observed Hyper-V Replica Broker, nil when the cluster
	// has none. A broker present here while the cluster spec declares none means
	// Ballast is not managing a live replication endpoint.
	ReplicaBroker *ClusterReplicaBroker
	// FunctionalLevel is the cluster's operating mode (ClusterFunctionalLevel).
	// Zero means it could not be read.
	FunctionalLevel int
	// NodeOSBuild is the reporting node's Windows build number, which bounds the
	// functional level the cluster could run at.
	NodeOSBuild int
}

// ClusterReplicaBroker is the observed Hyper-V Replica Broker role: the client
// access point a primary replicates to, its resource state, and where incoming
// replicas are stored.
type ClusterReplicaBroker struct {
	Name  string
	State string
	// StorageLocation comes from the replication authorization entry, which is
	// what actually decides where a replica lands — not the broker resource.
	StorageLocation string
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
	// DisksInMaintenance counts disks the cluster took into storage maintenance
	// mode, kept out of UnhealthyDisks. A drained node's disks report Warning
	// while they are out, and folding that into the failure count made a planned
	// drain look like broken hardware.
	DisksInMaintenance int
	TotalDisks         int
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

// ClusterWitness is the cluster's quorum configuration as Failover Clustering
// reports it: which witness (if any) holds the extra vote, where it lives, and
// whether its resource is actually online.
type ClusterWitness struct {
	// Type is "None", "FileShare", "Cloud" or "Disk".
	Type string
	// Path is the UNC share for a file-share witness, or the storage account
	// name for a cloud witness.
	Path string
	// State is the witness resource's state — Online, Offline, Failed. A
	// configured witness that is not Online is not voting.
	State string
	// QuorumType is the cluster's quorum model as Windows names it.
	QuorumType string
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
	// Resources are the group's own resources, carried only when the group is
	// neither Online nor Offline. A group state names the group and not the
	// fault; see types.ClusterGroupStatus.Resources.
	Resources []ClusterGroupResource
}

// ClusterGroupResource is one resource inside a cluster group. Distinct from
// ClusterCoreResource, which carries the agent's DIAGNOSIS of a core-group
// resource; this is the plain reading, collected for any group that is not
// resting.
type ClusterGroupResource struct {
	Name  string
	Type  string
	State string
}

// ClusterCoreResource is one resource in the cluster's core group, with the
// agent's reading of why it is not online where one can be established.
//
// The diagnosis is deliberately conservative. A duplicate address is asserted
// only when something ANSWERS at it while the resource is offline; silence is
// reported as nothing at all, because plenty of devices do not answer ping and
// "no reply" is not evidence the address is free.
type ClusterCoreResource struct {
	Name string
	// Type is the cluster resource type, e.g. "IP Address", "Network Name".
	Type  string
	State string
	// Address is the configured address for an IP Address resource.
	Address string
	// Note is the agent's diagnosis, empty when it could not establish one.
	Note string
}

// ClusterNodeState is a cluster node and its current state (Up, Paused — i.e.
// drained/maintenance — or Down).
type ClusterNodeState struct {
	Name  string
	State string
	// StatusInformation is Get-ClusterNode .StatusInformation — "Quarantined",
	// "Isolated", "Normal". A quarantined node reports State=Down exactly like a
	// switched-off one, so State alone cannot tell an outage from the cluster
	// deliberately holding a node out. See types.ClusterNodeStatus.
	StatusInformation string
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
	// SerialNumber is the serial of the DISK this volume sits on — the only
	// thing tying an observed volume to the LUN carrying it.
	SerialNumber string

	// SizeBytes/FreeBytes are the VOLUME's capacity as the cluster reports it,
	// not the backing virtual disk's. Those differ whenever a grow reached the
	// virtual disk but not the filesystem.
	SizeBytes uint64
	FreeBytes uint64
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
