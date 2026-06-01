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

import "time"

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
	// (for example, "rack=R12", "role=storage").
	Labels map[string]string `json:"labels,omitempty"`

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

	// RebootRequired is true when spec cannot be fully honoured until reboot
	// and RebootPolicy forbids the agent doing it autonomously.
	RebootRequired bool `json:"rebootRequired"`

	// Autonomous is true when the agent is currently running on its
	// last-honoured cached state because the control plane is unreachable.
	Autonomous bool `json:"autonomous"`

	// LastContact is when the agent last reached the control plane.
	LastContact time.Time `json:"lastContact"`

	// Inventory is observed hardware the control plane uses for placement
	// decisions (physical adapters, disks, memory, CPU).
	Inventory HostInventory `json:"inventory,omitempty"`

	Conditions []Condition `json:"conditions,omitempty"`
}

type HostInventory struct {
	PhysicalAdapters []PhysicalAdapter `json:"physicalAdapters,omitempty"`
	PhysicalDisks    []PhysicalDisk    `json:"physicalDisks,omitempty"`
	TotalMemoryBytes uint64            `json:"totalMemoryBytes,omitempty"`
	LogicalCPUs      int               `json:"logicalCPUs,omitempty"`
}

type PhysicalAdapter struct {
	Name         string `json:"name"` // OS-visible name
	MAC          string `json:"mac"`
	LinkSpeedBps uint64 `json:"linkSpeedBps,omitempty"`
	Up           bool   `json:"up"`
}

type PhysicalDisk struct {
	DeviceID  string `json:"deviceId"`
	SizeBytes uint64 `json:"sizeBytes"`
	MediaType string `json:"mediaType,omitempty"` // SSD/HDD/SCM
	CanPool   bool   `json:"canPool,omitempty"`
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
	Phase              Phase       `json:"phase"`
	ObservedGeneration int64       `json:"observedGeneration"`
	FormedMembers      []string    `json:"formedMembers,omitempty"`
	S2DEnabled         bool        `json:"s2dEnabled,omitempty"`
	Conditions         []Condition `json:"conditions,omitempty"`
}
