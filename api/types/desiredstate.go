// Package types defines the desired-state schema for Ballast.
//
// This is the single source of truth shared by the control plane and the
// host agent. The control plane stores and serves DesiredState; the agent
// persists the last-honoured copy locally and reconciles actual host state
// towards it.
//
// Design rules:
//   - Declarative. A spec describes the intended end state, never imperative
//     steps. The agent works out how to get there.
//   - Versioned. Every object carries a Generation that the control plane
//     increments on change and the agent echoes back once honoured.
//   - Idempotent by construction. Applying the same spec twice is a no-op.
//   - Hyper-V native vocabulary. We model SET switches and management OS
//     vNICs, not VMware dvSwitches/dvports. The mapping is documented inline
//     where the VMware mental model differs.
package types

import (
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Common envelope
// ---------------------------------------------------------------------------

// ObjectMeta is embedded in every top-level desired-state object.
type ObjectMeta struct {
	// Name is a stable, human-meaningful identifier, unique within its kind.
	Name string `json:"name"`

	// UID is the immutable system-assigned identity. Set by the control plane.
	UID string `json:"uid"`

	// Generation is incremented by the control plane each time the Spec
	// changes. The agent reports the Generation it has successfully honoured
	// in Status.ObservedGeneration. When the two match, the object is settled.
	Generation int64 `json:"generation"`

	// Labels are free-form key/value tags used for selection and grouping
	// (for example, "rack=R12", "role=storage"). Reserved keys: "site" and,
	// for VMs, "folder". Other keys are user tags.
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations hold non-identifying metadata such as a free-text "notes"
	// field. Centre-only; not used for selection and not sent to agents.
	Annotations map[string]string `json:"annotations,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Phase is a coarse lifecycle state shared across object kinds.
type Phase string

const (
	PhasePending     Phase = "Pending"     // accepted, not yet acted on
	PhaseProgressing Phase = "Progressing" // agent is reconciling
	PhaseReady       Phase = "Ready"       // actual matches desired
	PhaseDegraded    Phase = "Degraded"    // partially honoured, see Conditions
	PhaseError       Phase = "Error"       // reconcile failed, see Conditions
)

// Condition is a single, machine-readable status fact. Several conditions
// together explain why an object is in its current Phase.
type Condition struct {
	Type               string    `json:"type"`   // e.g. "SwitchConfigured"
	Status             bool      `json:"status"` // true = condition met
	Reason             string    `json:"reason"` // short CamelCase code
	Message            string    `json:"message"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// ---------------------------------------------------------------------------
// Host
// ---------------------------------------------------------------------------

// Host is the desired state for a single Hyper-V (typically Server Core) node.
// It is the unit an agent owns: one agent reconciles exactly one Host object.
type Host struct {
	Meta ObjectMeta `json:"meta"`
	Spec HostSpec   `json:"spec"`

	// Status is reported by the agent. The control plane treats it as
	// read-only and never sets it from the desired side.
	Status HostStatus `json:"status,omitempty"`
}

type HostSpec struct {
	// FQDN the agent should expect the host to be reachable as.
	FQDN string `json:"fqdn"`

	// ComputerName is the desired OS hostname. When it differs from the actual
	// name the agent renames the host (a reboot, governed by RebootPolicy).
	// Empty leaves the name as-is. This is distinct from the agent's stable
	// registration identity, so a rename never changes the host's identity in
	// the centre.
	ComputerName string `json:"computerName,omitempty"`

	// ManagementNIC, when set, assigns a static IP to a physical adapter — the
	// day-0 management address, set before any Hyper-V switch exists. Distinct
	// from a management-vNIC IP, which lives on a vSwitch.
	ManagementNIC *PhysicalNICConfig `json:"managementNIC,omitempty"`

	// DomainJoin, when set, joins the host to an Active Directory domain using
	// the referenced credential secret. A reboot, governed by RebootPolicy.
	DomainJoin *DomainJoinSpec `json:"domainJoin,omitempty"`

	// EnableHyperVRole instructs the agent to install/enable the Hyper-V role
	// if it is not already present. May require a reboot, which the agent
	// schedules and reports rather than forcing.
	EnableHyperVRole bool `json:"enableHyperVRole"`

	// Networking describes physical NIC intent, SET switches, and the
	// management OS vNICs layered on them.
	Networking HostNetworkingSpec `json:"networking"`

	// Storage describes host-local and shared storage intent.
	Storage HostStorageSpec `json:"storage,omitempty"`

	// ClusterMembership, when set, declares which cluster this host should
	// belong to. Nil means standalone.
	ClusterMembership *ClusterMembershipSpec `json:"clusterMembership,omitempty"`

	// RebootPolicy governs whether the agent may reboot autonomously to
	// honour spec (role install, driver changes) or must wait for approval.
	RebootPolicy RebootPolicy `json:"rebootPolicy"`

	// LiveMigration, when set, configures Hyper-V live migration on the host
	// (enable, authentication type, concurrency, and which networks to use).
	// Cluster-wide intent is fanned here from ClusterSpec.LiveMigration.
	LiveMigration *LiveMigrationSpec `json:"liveMigration,omitempty"`
}

// LiveMigrationSpec configures host live migration. On a cluster all members get
// the same settings so a VM can migrate between any of them.
type LiveMigrationSpec struct {
	// Enabled turns live migration on (Enable-VMMigration) when true.
	Enabled bool `json:"enabled"`

	// AuthenticationType is CredSSP or Kerberos. Empty leaves it unchanged.
	AuthenticationType string `json:"authenticationType,omitempty"`

	// MaxConcurrent caps simultaneous live migrations. Zero leaves it unchanged.
	MaxConcurrent int `json:"maxConcurrent,omitempty"`

	// Networks restricts migration to these CIDR subnets (e.g. the IPv4
	// management subnet, avoiding a bad IPv6 listener). Empty means any network.
	Networks []string `json:"networks,omitempty"`
}

// PhysicalNICConfig is a static IP assignment on a named physical adapter.
type PhysicalNICConfig struct {
	AdapterName string   `json:"adapterName"`
	IPConfig    IPConfig `json:"ipConfig"`
}

// DomainJoinSpec joins the host to an AD domain. The credential is referenced
// by the name of a stored Secret (type DomainCredential), never inline.
type DomainJoinSpec struct {
	DomainName string `json:"domainName"`
	// OUPath optionally places the computer object in a specific OU.
	OUPath string `json:"ouPath,omitempty"`
	// CredentialSecret is the name of the Secret holding the join account.
	CredentialSecret string `json:"credentialSecret"`
}

type RebootPolicy string

const (
	// RebootNever: agent surfaces "reboot required" and waits.
	RebootNever RebootPolicy = "Never"
	// RebootIfNeeded: agent may reboot to honour spec, respecting maintenance.
	RebootIfNeeded RebootPolicy = "IfNeeded"
)

type HostStatus struct {
	Phase Phase `json:"phase"`

	// ObservedGeneration is the Meta.Generation the agent has fully honoured.
	ObservedGeneration int64 `json:"observedGeneration"`

	// HyperVInstalled reflects actual role presence.
	HyperVInstalled bool `json:"hyperVInstalled"`

	// ComputerName and Domain are the host's observed identity (OS hostname and
	// AD domain, or empty/workgroup). Used to show rename / domain-join drift.
	ComputerName string `json:"computerName,omitempty"`
	Domain       string `json:"domain,omitempty"`

	// RebootRequired is true when spec cannot be fully honoured until reboot
	// and RebootPolicy forbids the agent doing it autonomously.
	RebootRequired bool `json:"rebootRequired"`

	// Autonomous is true when the agent is currently running on its
	// last-honoured cached state because the control plane is unreachable.
	Autonomous bool `json:"autonomous"`

	// AgentVersion is the reporting agent's build version, for the UI/diagnostics.
	AgentVersion string `json:"agentVersion,omitempty"`

	// LastContact is when the agent last reached the control plane.
	LastContact time.Time `json:"lastContact"`

	// Inventory is observed hardware the control plane uses for placement
	// decisions (physical adapters, disks, memory, CPU).
	Inventory HostInventory `json:"inventory,omitempty"`

	// Metrics is live host utilisation (CPU, memory in use, uptime), refreshed
	// each reconcile. Distinct from Inventory, which is the static hardware.
	Metrics HostMetrics `json:"metrics,omitempty"`

	// Resources are existing host objects the control plane and UI can offer as
	// choices (vSwitches to attach vNICs to, storage volumes to place VHDXs on,
	// ISO files to boot from). Observed each cycle; a pure read, never authored.
	Resources HostResources `json:"resources,omitempty"`

	Conditions []Condition `json:"conditions,omitempty"`
}

// HostResources is the set of pre-existing host objects available for use when
// authoring desired state (so the UI can offer real choices rather than free
// text). All best-effort and observed, not desired.
type HostResources struct {
	// Switches are the names of virtual switches that already exist on the host.
	// Kept as a flat name list for pickers; SwitchDetails carries the full state.
	Switches []string `json:"switches,omitempty"`
	// SwitchDetails is the observed per-switch state: uplink NICs, whether the
	// management OS shares the switch, and the management vNIC VLAN.
	SwitchDetails []VirtualSwitchInfo `json:"switchDetails,omitempty"`
	// Volumes are storage volumes (CSV mount points, fixed local volumes) a VM's
	// disks can be placed on.
	Volumes []StorageVolume `json:"volumes,omitempty"`
	// ISOs are paths of ISO files discovered in conventional locations
	// (each volume's ISOs folder, C:\ISOs), offered as boot media.
	ISOs []string `json:"isos,omitempty"`
}

// VirtualSwitchInfo is the observed state of an existing virtual switch on a
// host. All fields are best-effort, reported by the agent.
type VirtualSwitchInfo struct {
	// Name is the vSwitch name.
	Name string `json:"name"`
	// NetAdapters are the physical uplink NIC names backing the switch. More than
	// one means a SET team.
	NetAdapters []string `json:"netAdapters,omitempty"`
	// AllowManagementOS is true when the management OS shares the switch (a
	// management vNIC exists on it).
	AllowManagementOS bool `json:"allowManagementOS,omitempty"`
	// VLANID is the access VLAN of the management OS vNIC on this switch; 0 means
	// untagged or no management vNIC.
	VLANID int `json:"vlanId,omitempty"`
}

// StorageVolume is an observed place to put VM storage.
type StorageVolume struct {
	// Name is a human label (CSV resource name, or volume folder name).
	Name string `json:"name"`
	// Path is the mount point to build file paths under, e.g.
	// C:\ClusterStorage\Volume1.
	Path string `json:"path"`
	// SizeBytes / UsedBytes are the volume's capacity and usage. Best effort.
	SizeBytes uint64 `json:"sizeBytes,omitempty"`
	UsedBytes uint64 `json:"usedBytes,omitempty"`
}

// HostMetrics is observed, dynamic host utilisation. All fields are best-effort;
// zero means not observed this cycle.
type HostMetrics struct {
	// CPUUsagePercent is the host's overall processor load, 0-100.
	CPUUsagePercent int `json:"cpuUsagePercent,omitempty"`
	// MemoryInUseBytes is physical memory currently in use (total minus free).
	MemoryInUseBytes uint64 `json:"memoryInUseBytes,omitempty"`
	// UptimeSeconds is how long the host has been up since last boot.
	UptimeSeconds int64 `json:"uptimeSeconds,omitempty"`
}

type HostInventory struct {
	PhysicalAdapters []PhysicalAdapter `json:"physicalAdapters,omitempty"`
	PhysicalDisks    []PhysicalDisk    `json:"physicalDisks,omitempty"`
	TotalMemoryBytes uint64            `json:"totalMemoryBytes,omitempty"`
	LogicalCPUs      int               `json:"logicalCPUs,omitempty"`
	// OSVersion is the host OS caption/version (e.g. "Microsoft Windows Server
	// 2025 Datacenter 10.0.26100").
	OSVersion string `json:"osVersion,omitempty"`
	// UsedDriveLetters are the single-character drive letters currently in use
	// on the host (e.g. ["C","D"]). The UI uses this to prevent assigning a
	// letter that is already taken when formatting a disk to a volume.
	UsedDriveLetters []string `json:"usedDriveLetters,omitempty"`
}

type PhysicalAdapter struct {
	Name         string `json:"name"` // OS-visible name
	MAC          string `json:"mac"`
	LinkSpeedBps uint64 `json:"linkSpeedBps,omitempty"`
	Up           bool   `json:"up"`
	// IsManagement is true for the adapter carrying the host's route to the
	// centre (its management path). The centre uses this to avoid teaming the
	// management NIC when auto-building default switches.
	IsManagement bool `json:"isManagement,omitempty"`
	// IPv4 is the host IPv4 address bound to this adapter, if any (empty when the
	// NIC has no host IP — e.g. it's free or already bound to a vSwitch). The UI
	// uses it to mark a NIC that carries host connectivity and must not be teamed.
	IPv4 string `json:"ipv4,omitempty"`
	// DNSServers are the IPv4 DNS servers configured on this adapter. On a
	// domain-joined host a NIC whose DNS does not include the domain controller
	// (e.g. a DHCP NIC handed the router as DNS) breaks AD/DNS registration and
	// Kerberos; the UI flags it.
	DNSServers []string `json:"dnsServers,omitempty"`
	// RegistersDNS is whether this adapter registers its address in DNS
	// (RegisterThisConnectionsAddress). A secondary/DHCP NIC that registers can
	// publish a wrong A record for the host.
	RegistersDNS bool `json:"registersDNS,omitempty"`
	// Gateway is the IPv4 default-route next hop on this adapter, if any. It is
	// captured so that when a management IP is re-homed onto a converged switch's
	// management vNIC, the host's default route can be reproduced on the vNIC.
	Gateway string `json:"gateway,omitempty"`
}

type PhysicalDisk struct {
	DeviceID  string `json:"deviceId"`
	SizeBytes uint64 `json:"sizeBytes"`
	MediaType string `json:"mediaType,omitempty"` // SSD/HDD/SCM
	CanPool   bool   `json:"canPool,omitempty"`
	// IsOSDisk is true for the disk backing the host's boot/system volume, so the
	// UI can exclude it from the data disks available for S2D.
	IsOSDisk bool `json:"isOSDisk,omitempty"`
	// DriveLetter is the Windows drive letter assigned to this disk's primary
	// partition (e.g. "E"), empty when the disk is raw or pooled.
	DriveLetter string `json:"driveLetter,omitempty"`
}

// ---------------------------------------------------------------------------
// Networking
// ---------------------------------------------------------------------------

// HostNetworkingSpec is intentionally SET-first. Switch Embedded Teaming is
// the supported way to team adapters under a Hyper-V vSwitch on current
// Windows Server; legacy LBFO teams under a vSwitch are deprecated and are
// not modelled here.
type HostNetworkingSpec struct {
	// Switches are the SET-backed virtual switches to exist on this host.
	Switches []VirtualSwitchSpec `json:"switches,omitempty"`

	// ManagementVNICs are management OS vNICs to create on a switch. This is
	// the Hyper-V equivalent of what a VMware admin thinks of as host
	// VMkernel ports; "dvport" has no direct Hyper-V object, so port-level
	// intent (VLAN, QoS) lives on the vNIC and on VMNetworkAdapter port
	// profiles, expressed here per-vNIC.
	ManagementVNICs []ManagementVNICSpec `json:"managementVNICs,omitempty"`

	// DNSServers are the IPv4 DNS servers (typically the domain controllers) the
	// agent sets on the host's physical NICs. Fanned from the centre's global
	// Domain & DNS setting. Empty leaves DNS untouched. Setting this before a
	// domain join is what lets the host resolve the domain's SRV records.
	DNSServers []string `json:"dnsServers,omitempty"`

	// NICConfigs assigns static IPs to named physical adapters. Applied after
	// switches so a NIC being teamed gets its IP removed cleanly first.
	NICConfigs []PhysicalNICConfig `json:"nicConfigs,omitempty"`
}

type VirtualSwitchSpec struct {
	// Name of the vSwitch.
	Name string `json:"name"`

	// TeamMembers are the physical adapter names bound into the SET team.
	// One member is valid (no teaming); two or more enables SET teaming.
	TeamMembers []string `json:"teamMembers"`

	// TeamingMode for SET. Switch-independent is the only mode SET supports;
	// kept explicit so the schema is unambiguous.
	TeamingMode SETTeamingMode `json:"teamingMode"`

	// LoadBalancing algorithm for the SET team.
	LoadBalancing SETLoadBalancing `json:"loadBalancing"`

	// AllowManagementOS controls whether the host shares the switch for its
	// own management traffic (true) or the switch is VM-only (false).
	AllowManagementOS bool `json:"allowManagementOS"`
}

type SETTeamingMode string

const (
	SETSwitchIndependent SETTeamingMode = "SwitchIndependent"
)

type SETLoadBalancing string

const (
	SETHyperVPort SETLoadBalancing = "HyperVPort"
	SETDynamic    SETLoadBalancing = "Dynamic"
)

// ManagementVNICSpec is a host management OS vNIC on a named switch.
type ManagementVNICSpec struct {
	Name       string `json:"name"`
	SwitchName string `json:"switchName"`

	// VLANID 0 means untagged/access to native VLAN.
	VLANID int `json:"vlanID,omitempty"`

	// IPConfig for the vNIC. Nil means DHCP.
	IPConfig *IPConfig `json:"ipConfig,omitempty"`

	// MinBandwidthWeight expresses relative QoS weight (1-100) for this vNIC
	// when the switch uses weight-based bandwidth management.
	MinBandwidthWeight int `json:"minBandwidthWeight,omitempty"`
}

type IPConfig struct {
	Address    string   `json:"address"` // CIDR, e.g. 10.0.0.5/24
	Gateway    string   `json:"gateway,omitempty"`
	DNSServers []string `json:"dnsServers,omitempty"`
}

// ---------------------------------------------------------------------------
// Storage
// ---------------------------------------------------------------------------

// HostStorageSpec is deliberately minimal for v1, scoped to the S2D + CSV
// path. SAN/iSCSI/SMB3 backends are future kinds, not crammed in here.
type HostStorageSpec struct {
	// ContributeToS2D, when true, marks this host as a Storage Spaces Direct
	// contributor. Pool/volume creation is a cluster-level concern and lives
	// on the Cluster object, not here, to avoid every host racing to create
	// the same pool.
	ContributeToS2D bool `json:"contributeToS2D,omitempty"`

	// EligibleDiskSelector picks which physical disks may be claimed for S2D.
	// Empty means all poolable disks.
	EligibleDiskSelector map[string]string `json:"eligibleDiskSelector,omitempty"`

	// DefaultVMPath is the default directory for new VM configuration files
	// (Set-VMHost -VirtualMachinePath). On a cluster this should point at a CSV
	// so VMs land on shared storage and can migrate. Empty leaves the host
	// default unchanged.
	DefaultVMPath string `json:"defaultVMPath,omitempty"`

	// DefaultVHDPath is the default directory for new virtual hard disks
	// (Set-VMHost -VirtualHardDiskPath). Empty leaves the host default unchanged.
	DefaultVHDPath string `json:"defaultVHDPath,omitempty"`
}

// ---------------------------------------------------------------------------
// Cluster
// ---------------------------------------------------------------------------

// ClusterMembershipSpec is the host-side declaration of cluster intent.
type ClusterMembershipSpec struct {
	// ClusterName is the failover cluster this host should join.
	ClusterName string `json:"clusterName"`
}

// Cluster is the desired state for a failover cluster as a whole. It is owned
// by the control plane's cluster controller, not by any single host agent.
// The agent coordinates with the Failover Clustering service rather than
// replacing it; cluster quorum survives the control plane being offline.
type Cluster struct {
	Meta   ObjectMeta    `json:"meta"`
	Spec   ClusterSpec   `json:"spec"`
	Status ClusterStatus `json:"status,omitempty"`
}

// Site is an organisational container that groups clusters and standalone hosts,
// analogous to a vCenter datacentre but Hyper-V-native in name. It is a centre /
// console concept only — never sent to agents and not reconciled. A cluster or
// host joins a site via the "site" label on its ObjectMeta.
type Site struct {
	Name        string `json:"name"`
	Location    string `json:"location,omitempty"`
	Description string `json:"description,omitempty"`
}

// Dvport is a distributed virtual port: a user-named pairing of a vSwitch and a
// VLAN, giving operators a stable abstraction to attach VM NICs to instead of
// juggling raw switch names and VLAN IDs. It is centre-only metadata; a VM NIC
// references it by name and the centre resolves it to the NIC's SwitchName and
// VLANID, which the existing per-VM reconciler applies. The name is globally
// unique; SwitchName is immutable after creation.
type Dvport struct {
	Name        string `json:"name"`
	SwitchName  string `json:"switchName"`
	VLANID      int    `json:"vlanId"`
	Description string `json:"description,omitempty"`
}

// SiteLabel is the ObjectMeta label key by which clusters and hosts declare the
// site they belong to.
const SiteLabel = "site"

type ClusterSpec struct {
	// Members are the host names that should form the cluster.
	Members []string `json:"members"`

	// ManagementIP is the cluster's virtual management address (CIDR).
	ManagementIP string `json:"managementIP"`

	// Witness configures quorum. For a small cluster this is typically a
	// cloud or file-share witness.
	Witness WitnessSpec `json:"witness"`

	// EnableS2D requests Storage Spaces Direct be enabled on the cluster
	// once it is formed. Pool and volume definitions follow.
	EnableS2D bool `json:"enableS2D,omitempty"`

	// Volumes are Cluster Shared Volumes to provision on the S2D pool.
	Volumes []CSVSpec `json:"volumes,omitempty"`

	// Switches are cluster-wide virtual switches: one SET switch, identical in
	// name on every member (a requirement for VM migration — a VM's vNIC
	// reconnects by switch name on the destination host), backed by each host's
	// own chosen physical NICs. The centre fans each one into the member hosts'
	// HostSpec.Networking, where the existing per-host SET reconciler builds it.
	Switches []ClusterSwitchSpec `json:"switches,omitempty"`

	// DefaultStoragePath is the cluster-wide default directory for VM config and
	// VHDs — typically a CSV (e.g. C:\ClusterStorage\Vol01) so VMs land on shared
	// storage and can migrate. The centre fans it into each member host's
	// HostSpec.Storage default paths. Empty leaves host defaults unchanged.
	DefaultStoragePath string `json:"defaultStoragePath,omitempty"`

	// LiveMigration is cluster-wide live-migration configuration fanned into
	// every member's HostSpec so a VM can migrate between any of them.
	LiveMigration *LiveMigrationSpec `json:"liveMigration,omitempty"`
}

// ClusterSwitchSpec is a virtual switch defined once at the cluster and created
// identically on every member, with per-host physical NIC backing.
type ClusterSwitchSpec struct {
	// Name of the switch, identical on every member.
	Name string `json:"name"`

	// TeamingMode for SET (switch-independent only). Empty defaults to
	// SwitchIndependent.
	TeamingMode SETTeamingMode `json:"teamingMode,omitempty"`

	// LoadBalancing algorithm. Empty defaults to Dynamic.
	LoadBalancing SETLoadBalancing `json:"loadBalancing,omitempty"`

	// AllowManagementOS shares the switch for host management traffic when true.
	AllowManagementOS bool `json:"allowManagementOS,omitempty"`

	// HostNICs maps each member host name to the physical adapter names that
	// back the switch on that host. A host with no entry is skipped (the switch
	// is not created there until NICs are chosen for it).
	HostNICs map[string][]string `json:"hostNICs"`

	// ManagementVNICs are host management OS vNICs to create on this switch,
	// each tagged to a VLAN — the converged-networking pattern (a Management,
	// Cluster and Live-Migration vNIC, each on its own VLAN). Fanned into every
	// member's HostSpec.Networking.ManagementVNICs. This is where a VLAN is
	// "assigned" for a switch: Hyper-V tags the port (vNIC), not the switch.
	ManagementVNICs []ClusterMgmtVNIC `json:"managementVNICs,omitempty"`
}

// ClusterMgmtVNIC defines a management OS vNIC on a cluster switch: a name, its
// VLAN, optional QoS weight, and optional per-host IPs (empty = DHCP).
type ClusterMgmtVNIC struct {
	Name string `json:"name"`

	// VLANID 0 means untagged (access to the native VLAN).
	VLANID int `json:"vlanID,omitempty"`

	// MinBandwidthWeight is the QoS weight (1-100) when the switch uses
	// weight-based bandwidth management.
	MinBandwidthWeight int `json:"minBandwidthWeight,omitempty"`

	// HostIPs maps a member host to this vNIC's IP (CIDR) on that host. A host
	// with no entry gets DHCP.
	HostIPs map[string]string `json:"hostIPs,omitempty"`

	// HostGateways and HostDNS carry the default gateway and DNS servers for this
	// vNIC per host, so re-homing a host's management IP onto a converged switch's
	// vNIC preserves its default route and resolvers. Keyed like HostIPs; empty
	// entries leave the gateway/DNS unset (on-subnet-only).
	HostGateways map[string]string   `json:"hostGateways,omitempty"`
	HostDNS      map[string][]string `json:"hostDNS,omitempty"`
}

type WitnessSpec struct {
	Type WitnessType `json:"type"`
	// FileSharePath for FileShare witnesses.
	FileSharePath string `json:"fileSharePath,omitempty"`
	// CloudAccount / endpoint for cloud witnesses (secret handled out of band).
	CloudAccount string `json:"cloudAccount,omitempty"`
}

type WitnessType string

const (
	WitnessFileShare WitnessType = "FileShare"
	WitnessCloud     WitnessType = "Cloud"
	WitnessDisk      WitnessType = "Disk"
)

type CSVSpec struct {
	Name           string `json:"name"`
	SizeBytes      uint64 `json:"sizeBytes"`
	ResiliencyType string `json:"resiliencyType,omitempty"` // Mirror/Parity
	NumberOfCopies int    `json:"numberOfCopies,omitempty"`
}

type ClusterStatus struct {
	Phase              Phase                  `json:"phase"`
	ObservedGeneration int64                  `json:"observedGeneration"`
	FormedMembers      []string               `json:"formedMembers,omitempty"`
	S2DEnabled         bool                   `json:"s2dEnabled,omitempty"`
	Conditions         []Condition            `json:"conditions,omitempty"`
	Groups             []ClusterGroupStatus   `json:"groups,omitempty"`
	CSVs               []CSVStatus            `json:"csvs,omitempty"`
	VMs                []ClusterVMStatus      `json:"vms,omitempty"`
	Nodes              []ClusterNodeStatus    `json:"nodes,omitempty"`
	Pool               *ClusterPoolStatus     `json:"pool,omitempty"`
	Networks           []ClusterNetworkStatus `json:"networks,omitempty"`
}

// ClusterNetworkStatus is one cluster network: subnet (CIDR), role
// (None/Cluster/ClusterAndClient) and state (Up/Down/Partitioned/Unavailable).
// The console uses it to pick a live-migration network and flag a bad one.
type ClusterNetworkStatus struct {
	Name  string `json:"name"`
	CIDR  string `json:"cidr,omitempty"`
	Role  string `json:"role,omitempty"`
	State string `json:"state,omitempty"`
}

// ClusterPoolStatus is the S2D storage pool's capacity and health. Free is
// RawBytes minus AllocatedBytes; volumes' three-way mirror copies count against
// AllocatedBytes.
type ClusterPoolStatus struct {
	Name           string `json:"name,omitempty"`
	RawBytes       uint64 `json:"rawBytes,omitempty"`
	AllocatedBytes uint64 `json:"allocatedBytes,omitempty"`

	// Health is the pool's HealthStatus ("Healthy", "Warning", "Unhealthy") and
	// OperationalStatus (e.g. "Degraded") as reported by Storage Spaces. A CSV
	// cannot be provisioned while the pool is not Healthy.
	Health      string `json:"health,omitempty"`
	Operational string `json:"operational,omitempty"`
	// UnhealthyDisks is the number of physical disks in the pool that are not
	// Healthy (lost communication, transient error, failed). These degrade the
	// pool and block resilient-volume creation until retired/replaced.
	UnhealthyDisks int `json:"unhealthyDisks,omitempty"`
	// TotalDisks is the pool's physical-disk count, for context.
	TotalDisks int `json:"totalDisks,omitempty"`
}

// ClusterNodeStatus is a cluster node and its observed state — Up, Paused (the
// node is drained / in maintenance), or Down. The UI greys a drained/down node.
type ClusterNodeStatus struct {
	Name  string `json:"name"`
	State string `json:"state,omitempty"`
}

// ClusterVMStatus is one highly-available VM role observed on the cluster and its
// current owner node. Reported by the former so the UI can show clustered VMs in
// the VM section regardless of which node currently runs them.
type ClusterVMStatus struct {
	Name      string `json:"name"`
	OwnerNode string `json:"ownerNode,omitempty"`
	State     string `json:"state,omitempty"`
}

// ClusterGroupStatus is one clustered role/group and its current owner node, as
// observed by the cluster (reported by the former). Lets the UI show and move
// roles without the operator typing names.
type ClusterGroupStatus struct {
	Name      string `json:"name"`
	OwnerNode string `json:"ownerNode,omitempty"`
	State     string `json:"state,omitempty"`
	// GroupType is the failover-cluster group type (VirtualMachine, Cluster,
	// AvailableStorage, CoreSddc, ClusterStoragePool, ...). The UI uses it to
	// separate user roles from the cluster's own infrastructure groups.
	GroupType string `json:"groupType,omitempty"`
}

// CSVStatus is one Cluster Shared Volume and its current owner node.
type CSVStatus struct {
	Name      string `json:"name"`
	OwnerNode string `json:"ownerNode,omitempty"`
	State     string `json:"state,omitempty"`
}

// ---------------------------------------------------------------------------
// Jobs (imperative actions)
// ---------------------------------------------------------------------------

// Job is a one-shot imperative action the centre asks a host's agent to perform
// — the complement to declarative desired state. Where desired state says "this
// should be true" and is reconciled continuously, a Job says "do this now"
// (start/stop a VM, checkpoint, evict/add a cluster node, live-migrate). The
// agent executes it LOCALLY (no WinRM double-hop) and reports the outcome.
//
// Jobs are not part of desired state and are never reconciled: a failed Job is
// not retried by the loop; the operator re-issues it. This keeps the autonomy
// model clean — only declarative state is enforced when the centre is offline.
type Job struct {
	ID        string            `json:"id"`
	HostName  string            `json:"hostName"` // target host whose agent runs it
	Kind      string            `json:"kind"`
	Params    map[string]string `json:"params,omitempty"`
	State     JobState          `json:"state"`
	Message   string            `json:"message,omitempty"` // result detail / error
	// CreatedBy is the operator who triggered the job (the authenticated REST
	// session's username), or "system" for centre-initiated jobs. Centre-only
	// metadata for the activity log; never sent to agents.
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type JobState string

const (
	JobPending   JobState = "Pending"   // enqueued, not yet picked up
	JobRunning   JobState = "Running"   // agent claimed and is executing
	JobSucceeded JobState = "Succeeded" // completed successfully
	JobFailed    JobState = "Failed"    // failed, see Message
	JobCancelled JobState = "Cancelled" // cancelled by an operator (terminal)
)

// Terminal reports whether a job state is final (no further transitions).
func (s JobState) Terminal() bool {
	return s == JobSucceeded || s == JobFailed || s == JobCancelled
}

// Job kinds. Params carry the operands (e.g. "vm" for a VM name, "node" for a
// cluster node, "target" for a migration destination).
const (
	JobVMStart        = "VMStart"            // params: vm
	JobVMStop         = "VMStop"             // params: vm
	JobVMRestart      = "VMRestart"          // params: vm — one-shot guest restart
	JobVMCheckpoint   = "VMCheckpoint"       // params: vm, name
	JobVMApplyCheck   = "VMApplyCheckpoint"  // params: vm, name
	JobVMRemoveCheck  = "VMRemoveCheckpoint" // params: vm, name
	JobVMExport       = "VMExport"           // params: vm, path
	JobClusterAddNode = "ClusterAddNode"     // params: node
	JobClusterEvict   = "ClusterEvict"       // params: node
	JobNodeDrain      = "NodeDrain"          // params: node — pause + move roles off (maintenance)
	JobNodeResume     = "NodeResume"         // params: node — resume into the cluster

	JobClusterMoveGroup    = "ClusterMoveGroup"    // params: group, node — move/fail over a clustered role to node
	JobClusterMoveCSV      = "ClusterMoveCSV"      // params: volume, node — move CSV ownership to node
	JobClusterValidate     = "ClusterValidate"     // params: nodes (optional, comma list), include (optional) — Test-Cluster
	JobClusterMoveVM       = "ClusterMoveVM"       // params: vm, node — live-migrate a clustered VM role to node
	JobMigrateVM           = "MigrateVM"           // params: vm, destHost, destPath — shared-nothing live migration of a standalone VM to another host (run on the source host)
	JobClusterLog          = "ClusterLog"          // params: span (minutes), filter (optional substring) — Get-ClusterLog, relevant lines
	JobMigrationDelegation = "MigrationDelegation" // run on the former: params: nodes (optional comma list) — set Kerberos constrained delegation for live migration

	JobRemoveSwitch = "RemoveSwitch" // params: switch — delete a virtual switch from the host
	JobRemoveVM     = "RemoveVM"     // params: vm — stop and delete a VM from the host (hard delete)
	JobRemoveCSV    = "RemoveCSV"    // run on the former: params: volume — delete a Cluster Shared Volume from the S2D pool (destructive)
	JobRepairPool   = "RepairPool"   // run on a member: retire and remove unhealthy disks from the S2D pool so it returns to Healthy
	JobRebuildPool  = "RebuildPool"  // run on a member: DESTRUCTIVE — destroy the S2D pool and its volumes, then re-enable S2D fresh (for a stale/degraded pool from a torn-down cluster)

	JobFormatDisk      = "FormatDisk"      // params: deviceId — wipe a physical disk back to a poolable raw state (destructive)
	JobFormatDiskDrive = "FormatDiskDrive" // params: deviceId, driveLetter — initialise, partition, format NTFS and assign a drive letter

	JobRepairHostDNS = "RepairHostDNS" // no params — point non-management NICs' DNS at the DC and stop them registering in DNS

	JobRepairNetworkProfile = "RepairNetworkProfile" // no params — set any host NIC on the Public network profile to Private (Public breaks WinRM/clustering); Domain NICs are left as-is

	JobRebootHost = "RebootHost" // no params — restart the host now (Restart-Computer -Force)

	JobShutdownHost = "ShutdownHost" // no params — power the host off now (Stop-Computer -Force)

	JobEnableRDP = "EnableRDP" // no params — enable Remote Desktop (clear fDenyTSConnections, enable the RDP firewall group)

	JobClusterDestroy = "ClusterDestroy" // run on the former: remove VM roles, disable S2D, Remove-Cluster -CleanupAD (destructive)

	JobFetchISO = "FetchISO" // params: url, dest, name — download an ISO from the centre's library to dest (a CSV's ISOs folder), agent-local

	JobGuestJoinDomain = "GuestJoinDomain" // params: vm, domain, ou, guestUser, guestPass, domainUser, domainPass — join the guest OS to the domain via PowerShell Direct (reboots the guest)
	JobGuestSetIP      = "GuestSetIP"      // params: vm, interface, address (CIDR), gateway, dns, guestUser, guestPass — set a static IP in the guest via PowerShell Direct
)

// sensitiveJobParams are Job.Params keys whose values are credential material.
// They are stripped from every UI/REST view of a job and scrubbed from the
// stored job once it reaches a terminal state, so secrets do not linger at rest
// in the job history. The agent receives the real values over the gRPC job
// channel before the job completes, so scrubbing afterwards costs it nothing.
var sensitiveJobParams = map[string]bool{
	"guestuser": true, "guestpass": true, "domainuser": true, "domainpass": true,
	"username": true, "password": true, "pass": true, "pw": true, "secret": true,
}

// IsSensitiveJobParam reports whether a Job.Params key carries credential
// material (case-insensitive).
func IsSensitiveJobParam(key string) bool {
	return sensitiveJobParams[strings.ToLower(key)]
}

// ScrubSensitiveParams returns a copy of params with credential values removed.
// It is the single point that decides what counts as a secret, shared by the
// REST redaction (display) and the store's terminal-job scrub (at rest). An
// empty map is returned unchanged.
func ScrubSensitiveParams(params map[string]string) map[string]string {
	if len(params) == 0 {
		return params
	}
	clean := make(map[string]string, len(params))
	for k, v := range params {
		if IsSensitiveJobParam(k) {
			continue
		}
		clean[k] = v
	}
	return clean
}

// ---------------------------------------------------------------------------
// Secrets
// ---------------------------------------------------------------------------

// Secret is a named bag of sensitive key/value data the control plane holds on
// behalf of an operation that needs credentials — domain join, later iSCSI
// CHAP. Desired-state specs reference a secret by name (never by value); the
// centre stores the Data encrypted at rest and delivers it to the agent only
// over the gRPC channel, only to the host that needs it. Data is never returned
// on read APIs or written to logs.
type Secret struct {
	Name string `json:"name"`
	// Type categorises the secret so consumers know its shape, e.g.
	// "DomainCredential" expects keys "username" and "password".
	Type string `json:"type"`
	// Data holds the sensitive values. Omitted from any UI-facing serialisation.
	Data map[string]string `json:"data,omitempty"`
}

const (
	// SecretDomainCredential carries "username" + "password" for domain join.
	SecretDomainCredential = "DomainCredential"
)

// User is an operator account that can sign in to the centre's UI/REST surface.
// Authentication is a centre-only concern — agents never use it (they keep their
// own identity, so the autonomy story is unaffected) — so it lives alongside the
// other centre-managed records. The plaintext password is never stored or
// returned; only its bcrypt hash is persisted.
type User struct {
	Username string `json:"username"`
	// PasswordHash is the bcrypt hash of the user's password. Never serialised to
	// any UI/automation surface.
	PasswordHash string `json:"-"`
	// Role governs what the user may do: RoleAdmin or RoleOperator.
	Role string `json:"role"`
	// Source is UserSourceLocal for built-in accounts; AD/LDAP users (a later
	// phase) will carry a different source.
	Source    string    `json:"source"`
	UpdatedAt time.Time `json:"updatedAt"`
}

const (
	RoleAdmin       = "admin"
	RoleOperator    = "operator"
	UserSourceLocal = "local"
)

// ---------------------------------------------------------------------------
// Virtual machine
// ---------------------------------------------------------------------------

// VM is the desired state for a single virtual machine. Like Cluster it is a
// top-level, control-plane-owned object rather than a field of a Host: it is
// placed on a host via Spec.Placement.HostName, and the agent on that host
// reconciles it. Placement is static intent in v1 — live migration on host
// death stays a Failover Clustering concern and is not modelled here.
//
// The same desired-state rules apply: the agent drives actual VM state towards
// the spec idempotently, advancing Status.ObservedGeneration only once the spec
// is fully honoured, and keeps enforcing the cached spec when the centre is
// offline.
type VM struct {
	Meta ObjectMeta `json:"meta"`
	Spec VMSpec     `json:"spec"`

	// Status is reported by the owning host's agent; the control plane treats it
	// as read-only.
	Status VMStatus `json:"status,omitempty"`
}

type VMSpec struct {
	// Placement assigns the VM to a host. Exactly one agent — the one for
	// Placement.HostName — owns and reconciles this VM.
	Placement VMPlacementSpec `json:"placement"`

	// HyperVGeneration is the Hyper-V VM generation, 1 or 2. Generation 2 is
	// UEFI-based and the default for modern guests; it is immutable once the VM
	// exists. Named to avoid colliding with Meta.Generation, which is unrelated.
	// Zero is treated as 2 by the agent.
	HyperVGeneration int `json:"hyperVGeneration,omitempty"`

	// ProcessorCount is the number of virtual processors assigned.
	ProcessorCount int `json:"processorCount"`

	// MemoryStartupBytes is the startup memory. With DynamicMemory unset this is
	// also the fixed assignment.
	MemoryStartupBytes uint64 `json:"memoryStartupBytes"`

	// DynamicMemory, when set, lets the VM's memory float between Min and Max.
	// Nil means a fixed assignment of MemoryStartupBytes.
	DynamicMemory *DynamicMemorySpec `json:"dynamicMemory,omitempty"`

	// Disks are the virtual hard disks attached to the VM, in attachment order.
	Disks []VMDiskSpec `json:"disks,omitempty"`

	// NetworkAdapters are the VM's vNICs, each bound to a named vSwitch that is
	// expected to exist on the placement host.
	NetworkAdapters []VMNetworkAdapterSpec `json:"networkAdapters,omitempty"`

	// ISOPath, when set, attaches a DVD drive backed by this ISO so the VM can
	// boot from it. Empty means no boot media (or one already attached is left
	// as-is). For a Generation 2 VM the agent also makes the DVD a boot entry.
	ISOPath string `json:"isoPath,omitempty"`

	// DesiredPowerState is the power state the agent should drive the VM to.
	DesiredPowerState VMPowerState `json:"desiredPowerState"`

	// AutomaticStartAction governs what the host does with the VM when the host
	// itself boots. Empty defaults to the Hyper-V default (StartIfRunning).
	AutomaticStartAction VMStartAction `json:"automaticStartAction,omitempty"`
}

// VMPlacementSpec assigns a VM to a host. It is the only link between a VM and
// the agent that reconciles it.
type VMPlacementSpec struct {
	// HostName is the host the VM runs on. The matching agent owns it. Set for a
	// VM on a standalone host (traditional placement).
	HostName string `json:"hostName,omitempty"`

	// ClusterName, when set, places the VM as a highly-available cluster role on
	// the named cluster instead of a single host. The centre routes the VM to the
	// current owner node's agent (auto-picking an initial owner at creation), and
	// the agent registers it with Failover Clustering. Mutually exclusive with
	// HostName.
	ClusterName string `json:"clusterName,omitempty"`
}

// DynamicMemorySpec bounds dynamic memory. MemoryStartupBytes must lie within
// [MinBytes, MaxBytes].
type DynamicMemorySpec struct {
	MinBytes uint64 `json:"minBytes"`
	MaxBytes uint64 `json:"maxBytes"`
}

type VMDiskSpec struct {
	// Path is the VHDX path on the host, or on a CSV (C:\ClusterStorage\...) for
	// a clustered VM.
	Path string `json:"path"`

	// SizeBytes is the provisioned size of a disk the agent must create. Zero
	// means the VHDX already exists at Path and is attached as-is rather than
	// created.
	SizeBytes uint64 `json:"sizeBytes,omitempty"`

	// Dynamic selects a dynamically-expanding VHDX (true) over a fixed one. Only
	// consulted when the agent creates the disk (SizeBytes > 0).
	Dynamic bool `json:"dynamic,omitempty"`
}

type VMNetworkAdapterSpec struct {
	// Name identifies the adapter within the VM (stable key for reconciliation).
	Name string `json:"name"`

	// SwitchName is the vSwitch this adapter connects to.
	SwitchName string `json:"switchName"`

	// VLANID 0 means untagged/access to the native VLAN.
	VLANID int `json:"vlanID,omitempty"`

	// DvportName, when set, binds this adapter to a named distributed virtual port
	// (a vSwitch + VLAN abstraction). The centre resolves it to SwitchName and
	// VLANID at author time and re-resolves every VM using it when the dvport's
	// VLAN changes, so the agent only ever sees the resolved switch + VLAN. It is
	// centre-only metadata (kept for display and re-resolution), never on the wire.
	DvportName string `json:"dvportName,omitempty"`

	// MACAddress, when empty, means the host assigns a dynamic MAC.
	MACAddress string `json:"macAddress,omitempty"`
}

// VMPowerState is both the requested (Spec.DesiredPowerState) and observed
// (Status.PowerState) power state. The agent only drives towards Running or
// Off; Paused/Saved are reported when observed but never requested in v1.
type VMPowerState string

const (
	VMPowerRunning VMPowerState = "Running"
	VMPowerOff     VMPowerState = "Off"
	VMPowerPaused  VMPowerState = "Paused"
	VMPowerSaved   VMPowerState = "Saved"
)

type VMStartAction string

const (
	VMStartNothing      VMStartAction = "Nothing"
	VMStartIfWasRunning VMStartAction = "StartIfRunning"
	VMStartAlways       VMStartAction = "Start"
)

type VMStatus struct {
	Phase Phase `json:"phase"`

	// ObservedGeneration is the Meta.Generation the agent has fully honoured.
	ObservedGeneration int64 `json:"observedGeneration"`

	// PowerState is the actual observed power state of the VM.
	PowerState VMPowerState `json:"powerState,omitempty"`

	// VMID is the VM's Hyper-V GUID (Get-VM .Id). The centre uses it as the
	// console preconnection-blob to open the VM's VMConnect console over RDP.
	VMID string `json:"vmId,omitempty"`

	// GuestOS is the guest operating system name reported by the integration
	// services KVP exchange (e.g. "Windows Server 2025 Datacenter"). Empty until
	// an OS is installed and integration services are running.
	GuestOS string `json:"guestOS,omitempty"`

	// IPAddress is the guest's IP address(es) as reported by Hyper-V (comma-
	// separated when more than one). Empty until the guest has integration
	// services and an address.
	IPAddress string `json:"ipAddress,omitempty"`

	// GuestFQDN is the guest's fully-qualified domain name from the integration-
	// services KVP exchange — "host.domain" when domain-joined, just "host" in a
	// workgroup. The UI derives domain membership from it.
	GuestFQDN string `json:"guestFQDN,omitempty"`

	// AssignedMemoryBytes is the memory currently assigned (meaningful under
	// dynamic memory). Best effort; zero when not observed.
	AssignedMemoryBytes uint64 `json:"assignedMemoryBytes,omitempty"`

	// CPUUsagePercent is the VM's host-CPU load. Best effort; zero when not
	// observed.
	CPUUsagePercent int `json:"cpuUsagePercent,omitempty"`

	// UptimeSeconds is how long the VM has been running. Best effort.
	UptimeSeconds int64 `json:"uptimeSeconds,omitempty"`

	// ScreenPNG is a small PNG snapshot of the VM's console (the Hyper-V
	// thumbnail), present only when the VM is running. It is delivered with
	// status but stripped from list/get responses to keep them small; the REST
	// screen endpoint serves it. Read-only — there is no interactive console yet.
	ScreenPNG []byte `json:"screenPng,omitempty"`

	// Checkpoints is the VM's current set of Hyper-V checkpoints (snapshots) as
	// reported by the owning agent. Read-only here; created/applied/removed via
	// the checkpoint jobs.
	Checkpoints []VMCheckpoint `json:"checkpoints,omitempty"`

	// Observed is the VM's actual configuration (CPU/memory/disks/adapters/
	// generation) read from the host. It lets the centre show and adopt a VM's
	// real config — in particular where its VHDX(s) live — for VMs Ballast did
	// not create. Read-only.
	Observed *VMObserved `json:"observed,omitempty"`

	Conditions []Condition `json:"conditions,omitempty"`
}

// VMObserved is a VM's actual configuration as read from the host, used to show
// and adopt VMs created outside Ballast. Disks reuse VMDiskSpec with SizeBytes
// left zero so an adopt attaches the existing VHDX rather than recreating it.
type VMObserved struct {
	ProcessorCount     int                    `json:"processorCount,omitempty"`
	MemoryStartupBytes uint64                 `json:"memoryStartupBytes,omitempty"`
	DynamicMemory      bool                   `json:"dynamicMemory,omitempty"`
	MinBytes           uint64                 `json:"minBytes,omitempty"`
	MaxBytes           uint64                 `json:"maxBytes,omitempty"`
	Generation         int                    `json:"generation,omitempty"`
	Disks              []VMDiskSpec           `json:"disks,omitempty"`
	NetworkAdapters    []VMNetworkAdapterSpec `json:"networkAdapters,omitempty"`
}

// VMCheckpoint is one Hyper-V checkpoint (snapshot) of a VM. Checkpoints form a
// tree — ParentName links a child to its parent ("" for a root). IsCurrent marks
// the checkpoint the VM's running state currently derives from.
type VMCheckpoint struct {
	Name       string    `json:"name"`
	ParentName string    `json:"parentName,omitempty"`
	Type       string    `json:"type,omitempty"` // Standard or Production
	CreatedAt  time.Time `json:"createdAt,omitempty"`
	IsCurrent  bool      `json:"isCurrent,omitempty"`
}
