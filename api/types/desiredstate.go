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
	"fmt"
	"strconv"
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

// Condition reasons that carry a specific remedy. Most reasons describe what
// happened; these tell the console what to OFFER, so they are part of the
// contract with the UI and must not be reworded casually.
const (
	// ReasonSavedStateIncompatible: the VM cannot start because its saved memory
	// image was captured on a host with a different CPU feature set. Retrying
	// never helps. The single remedy — discard the image and cold boot — is
	// destructive, so the console offers it explicitly and nothing performs it
	// automatically. See docs/console-completeness-2026-08-05.md.
	ReasonSavedStateIncompatible = "SavedStateIncompatible"

	// ReasonNotAttempted: the reconcile pass ran out of time (or was cancelled)
	// before this operation ran. Nothing was read and nothing was changed, so the
	// object's real state is unknown rather than bad. The console must not show
	// it as a failure of the object — the fault is whatever earlier step consumed
	// the pass, and that is where the operator should be sent.
	ReasonNotAttempted = "NotAttempted"

	// ReasonHyperVUnavailable: Hyper-V's Virtual Machine Management service is
	// stopped, crashed or restarting, so switches and vNICs could not be observed.
	// Ballast deliberately changes nothing in this state — an unreadable
	// observation must never be mistaken for an absent object — so the remedy is
	// to get VMMS running, after which the next pass reconciles normally.
	ReasonHyperVUnavailable = "HyperVUnavailable"
)

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

	/* BMC is the host's baseboard management controller — iLO, iDRAC, XCC — so
	   a host that is switched OFF can be switched on from the centre.

	   The one thing here that no agent can do. Every other host action goes
	   through an agent running on that host; this exists precisely for the case
	   where there is nothing running to ask, and without it a powered-off host
	   cannot be recovered from the console at all. "Find the iLO address and log
	   in" is a runbook step, which is the thing this product treats as a defect.

	   The centre makes the call, not a peer host: the BMC is on a management
	   network and the question of who can reach it is a routing question, not a
	   Hyper-V one. */
	BMC *BMCSpec `json:"bmc,omitempty"`

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

	// WindowsLicence declares the Windows EDITION this host should run.
	//
	// Desired state like the domain join and the Hyper-V role: declared once,
	// applied by the agent, restart governed by RebootPolicy. It does NOT activate
	// the host — a conversion and an activation are different acts, one consuming
	// nothing and the other consuming a key, so they are declared separately.
	//
	// Nil means Ballast does not manage the edition, which is not the same as
	// declaring the current one: an absent declaration never converts anything.
	WindowsLicence *WindowsLicenceSpec `json:"windowsLicence,omitempty"`

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

	// ReplicaServer, when set, configures this host to ACCEPT Hyper-V Replica
	// traffic (Set-VMReplicationServer + the replica firewall rule) so VMs from
	// elsewhere can replicate to it. For a cluster member this is fanned from
	// ClusterSpec.ReplicaBroker and applies per node; replication then targets
	// the broker's client access point, not an individual node.
	ReplicaServer *ReplicaServerSpec `json:"replicaServer,omitempty"`

	// Maintenance takes the host out of service for planned work. It is DESIRED
	// STATE, not a job, and that is the whole point: drain and resume already
	// existed as one-shot jobs, but nothing remembered the intent afterwards, so a
	// deliberately drained host was indistinguishable from a broken one. As a spec
	// field the agent re-asserts it every pass (a node that reboots and rejoins
	// comes back paused if that is still the intent), it survives the centre going
	// offline, and the centre can act on it — placement skips the host and its
	// alarms stop shouting about a machine somebody is deliberately working on.
	Maintenance *MaintenanceSpec `json:"maintenance,omitempty"`

	// ISOLibrary is an SMB share this STANDALONE host mounts boot media from.
	//
	// It is declared per host and is never inherited: a cluster member takes its
	// library from ClusterSpec.ISOLibrary instead, read straight from the cluster
	// assignment the agent already receives. Nothing is fanned into member specs,
	// because a fanned copy is a second source of truth that outlives its origin —
	// which is precisely how a deleted CSV left dangling paths on three hosts.
	// With no copy, eviction removes the library by construction.
	ISOLibrary *ISOLibrarySpec `json:"isoLibrary,omitempty"`
}

// ISOLibrarySpec is an SMB share holding boot media, referenced in place rather
// than copied.
//
// Today a cluster's ISOs are FETCHED onto its CSV — every cluster keeping its own
// copy of every image on the most expensive storage it has, and a new node
// waiting on a multi-gigabyte transfer before it can boot anything. A share is
// referenced directly by the VM's ISOPath, so there is nothing to copy and
// nothing to keep in step.
//
// THE ACCESS MODEL IS THE WHOLE DIFFICULTY, and it is the same one the file-share
// witness taught: Hyper-V attaches an ISO from the VMMS process, which runs as
// LocalSystem, so it reaches the share as the node's COMPUTER ACCOUNT — not as
// the operator, and not as the agent's service account. A share that the agent
// can read may still be unreadable to Hyper-V, and the reverse. Granting
// "everyone" on a standalone NAS does not help, because the computer account
// cannot authenticate to it at all.
//
// So the share must grant read to each node's computer account (or a group
// holding them), and the NAS must be domain-joined. That is outside what Ballast
// administers, so the console names the step rather than failing obscurely.
type ISOLibrarySpec struct {
	// Path is the UNC share, e.g. \\nas.lab.local\isos.
	//
	// Use the FQDN, not an IP: Kerberos needs an SPN to find, and an IP forces a
	// fallback to NTLM which the computer account often cannot complete.
	Path string `json:"path"`

	// CredentialSecret optionally names a stored credential to reach the share
	// with, for a share that cannot grant the computer accounts directly.
	//
	// It is a fallback, not the default. A credential only helps the agent's own
	// reads (listing the library); it does NOT change how Hyper-V attaches an
	// ISO, which is always as the computer account. So a library configured this
	// way can list correctly and still fail to boot a VM — the status says so
	// rather than letting the list imply it works.
	CredentialSecret string `json:"credentialSecret,omitempty"`
}

// ISOLibraryStatus is one host's view of its declared ISO library.
//
// The two reachability figures are separate on purpose, because they answer
// different questions and can disagree. The agent reads the share as its own
// service account; Hyper-V attaches an ISO as the node's COMPUTER ACCOUNT. A
// library that lists perfectly and cannot boot a VM is the failure this
// distinction exists to make visible — and it is the normal outcome of a share
// granted to a user rather than to the machines.
// WindowsLicenceSpec declares the Windows edition a host should run.
//
// Converting is IRREVERSIBLE — there is no way back to an evaluation edition, and
// no way down from a higher edition — so this is applied only when the declared
// edition differs from the running one AND Windows itself offers it as a valid
// target. Ballast asks rather than reasoning about which conversions are legal:
// the servicing stack knows, and a rule written here would be a guess that ages.
type WindowsLicenceSpec struct {
	// Edition is the target edition ID as Windows names it — ServerDatacenter,
	// ServerStandard. Not the friendly caption, which is localised.
	Edition string `json:"edition"`

	// ProductKeySecret names a stored secret holding the product key the
	// conversion needs.
	//
	// A reference, never the key. A key inline in the spec would sit in Postgres,
	// in every spec-history row, and in any diff an operator pastes into a ticket —
	// and unlike a password it cannot be rotated once it has been used.
	ProductKeySecret string `json:"productKeySecret"`

	// Activation is how this host activates: MAK, KMS, or empty for neither.
	//
	// Separate from the edition because they are different acts on different
	// schedules: a conversion happens once and consumes nothing, an activation
	// consumes a seat from a key pool and may be repeated after hardware changes.
	// A host can also be converted and left unactivated on purpose, during a
	// staged rollout.
	Activation string `json:"activation,omitempty"`

	// ActivationKeySecret names the key to activate WITH — a MAK, or the public
	// GVLK for a KMS client.
	//
	// Supplied rather than derived. GVLKs are public and per edition and release,
	// so Ballast could carry a table of them — and that table would be a rule that
	// ages every time Microsoft ships a version, which is the same trap as deciding
	// for ourselves which edition conversions are legal. Empty leaves the key the
	// host already has, which is right for a host that only needs pointing at a
	// KMS server.
	ActivationKeySecret string `json:"activationKeySecret,omitempty"`

	// KMSServer is the KMS host to activate against, optionally with :port. Empty
	// uses whatever DNS auto-discovery finds, which is how most KMS estates are
	// meant to work — setting it explicitly is for the ones that are not.
	KMSServer string `json:"kmsServer,omitempty"`

	// TargetEditions is what THIS installation may convert to, as Windows itself
	// reports it. Per-installation and not derivable: a Standard Core evaluation
	// converts only to Datacenter Core, under the name ServerDatacenterCor.
	//
	// Empty means NOT REPORTED. It must never be read as "no conversion is
	// possible" — a list Ballast could not gather is not a refusal, and a console
	// that offered an empty menu would block a conversion Windows would allow.
	TargetEditions []string `json:"targetEditions,omitempty"`
}

// Windows activation methods.
const (
	// ActivationMAK is a Multiple Activation Key: one key, a pool of activations,
	// each host consuming a seat and needing to reach Microsoft once.
	ActivationMAK = "MAK"
	// ActivationKMS points the host at a Key Management Service host, which
	// reactivates it every 180 days and needs no outbound internet.
	ActivationKMS = "KMS"
)

// WindowsLicenceStatus is the host's observed Windows edition and activation.
//
// Read-only, and deliberately shipped before anything that changes it. An
// evaluation edition cannot be activated at all — it has to be converted first,
// irreversibly, with a reboot — so the fleet's real position is worth seeing
// before Ballast is given the power to alter it. A 120-day clock nobody can see
// is the failure this prevents.
type WindowsLicenceStatus struct {
	// Edition is the installed edition ID (ServerDatacenter, ServerStandard…) as
	// DISM reports it, and Description is what a person recognises.
	Edition     string `json:"edition,omitempty"`
	Description string `json:"description,omitempty"`

	// Evaluation marks an edition that CANNOT be activated. It is the single most
	// consequential fact here: an operator who does not know it will try to
	// activate, fail, and have no idea why — the remedy is a DISM edition change,
	// not a key.
	Evaluation bool `json:"evaluation,omitempty"`

	// Status is the SoftwareLicensingProduct LicenseStatus in words — Licensed,
	// InitialGrace, OutOfTolerance, Notification, Unlicensed. The numeric form is
	// not carried: nothing downstream should have to know that 1 means licensed.
	Status string `json:"status,omitempty"`

	// GraceDaysRemaining is how long an unactivated or evaluation host has before
	// Windows starts objecting. Zero when licensed, and zero when unknown — the
	// two are told apart by Status, because a countdown is only meaningful
	// alongside what it is counting down to.
	GraceDaysRemaining int `json:"graceDaysRemaining,omitempty"`

	// Channel is Retail, Volume:MAK, Volume:GVLK or Evaluation, which is what
	// decides HOW a host activates. PartialProductKey is the last five characters
	// Windows exposes: enough to tell two keys apart, and never the key itself.
	Channel           string `json:"channel,omitempty"`
	PartialProductKey string `json:"partialProductKey,omitempty"`

	// KMSServer is where a volume-licensed host activates, when it has one.
	KMSServer string `json:"kmsServer,omitempty"`

	// TargetEditions is what THIS installation may convert to, as Windows itself
	// reports it. Per-installation and not derivable: a Standard Core evaluation
	// converts only to Datacenter Core, under the name ServerDatacenterCor.
	//
	// Empty means NOT REPORTED. It must never be read as "no conversion is
	// possible" — a list Ballast could not gather is not a refusal, and a console
	// that offered an empty menu would block a conversion Windows would allow.
	TargetEditions []string `json:"targetEditions,omitempty"`

	// Message explains a state the fields cannot, in the operator's terms.
	Message string `json:"message,omitempty"`
}

type ISOLibraryStatus struct {
	// Path echoes the declared share, so a status is readable without the spec.
	Path string `json:"path,omitempty"`

	// Readable is whether the AGENT could list the share. This is what populates
	// ISOs, and on its own it proves nothing about booting.
	Readable bool `json:"readable"`

	// MachineReadable is whether the share is reachable AS THE COMPUTER ACCOUNT —
	// the way Hyper-V will actually attach the media. This is the one that
	// decides whether a VM can boot from the library.
	//
	// Verified the way the witness had to be: from a LocalSystem context, because
	// a probe run as the agent's own account answers a different question and
	// answers it confidently. Nil means it has not been established, which is not
	// the same as false and must not be shown as a failure.
	MachineReadable *bool `json:"machineReadable,omitempty"`

	// Message explains a failure in the operator's terms, naming the remedy where
	// it is outside Ballast's boundary — a share on a NAS is not something Ballast
	// administers, so it says which grant is missing rather than failing obscurely.
	Message string `json:"message,omitempty"`

	// ISOs are the .iso files found, newest first. Names only; the full path is
	// Path + name.
	ISOs []string `json:"isos,omitempty"`

	// CheckedAt is when the host last probed the share.
	CheckedAt time.Time `json:"checkedAt,omitempty"`
}

// ReplicaServerSpec makes a host a Hyper-V Replica target.
type ReplicaServerSpec struct {
	// Enabled turns the replica server on. False (with the spec present)
	// declares it off.
	Enabled bool `json:"enabled"`

	// AuthenticationType is Kerberos (integrated, both ends domain-joined) or
	// Certificate. Empty defaults to Kerberos.
	AuthenticationType string `json:"authenticationType,omitempty"`

	// Port is the listener port. Zero defaults to 80 for Kerberos (443 for
	// certificate auth).
	Port int `json:"port,omitempty"`

	// DefaultStorageLocation is where inbound replica VHDs land. On a cluster
	// member this should be a CSV path so the replica VM can fail over. Empty
	// defaults to C:\Hyper-V\Replica (host) — set explicitly for clusters.
	DefaultStorageLocation string `json:"defaultStorageLocation,omitempty"`
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

/*
BMCSpec is how to reach a host's management controller.

	Redfish only, and power-ON only. Redfish is the standard every current
	controller speaks — iLO 5+, iDRAC 8+, Lenovo XCC, Supermicro X11+ — and the
	older ones that need IPMI are told so plainly rather than failing obscurely.

	Powering a host ON is safe: a machine that is off has nothing running to
	disturb. The destructive directions — force off, reset — are deliberately NOT
	here. They pull power from running workloads, and a control plane that can do
	that from a right-click menu is one misclick from an outage.
*/
type BMCSpec struct {
	// Address is the controller's hostname or IP, e.g. "hv01-ilo.lab.local".
	// A bare address is reached over HTTPS; a scheme may be given explicitly.
	Address string `json:"address"`

	// CredentialSecret names the stored Secret holding the controller's
	// username and password. A reference, never the credential itself: a BMC
	// account is a keys-to-the-building credential and it does not belong in
	// desired state that is read by everything.
	CredentialSecret string `json:"credentialSecret"`

	/* InsecureTLS skips certificate verification.

	   Off by default, and it has to be an explicit choice, because almost every
	   BMC ships with a self-signed certificate — which means the honest default
	   produces a failure the first time somebody uses this, and the alternative
	   is a control plane that silently trusts anything answering on the
	   management network. The failure names this field. */
	InsecureTLS bool `json:"insecureTls,omitempty"`
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

	// ClusterNode is this host's OWN membership state, as its cluster sees it:
	// Up, Paused, Quarantined, Isolated, Down. Empty when the host is not
	// clustered, or when its cluster service could not answer.
	//
	// Reported per host rather than read off the cluster, because the cluster's
	// node list comes from ONE member and is exactly what is missing when that
	// member is the broken one. A host can always answer for itself.
	ClusterNode string `json:"clusterNode,omitempty"`

	// ClusterService is the state of this host's Failover Clustering service
	// (Running, Stopped, StartPending...). It is what a node can still report when
	// it cannot answer anything else: quarantine works by STOPPING this service, so
	// a stopped one is both the reason a node reports no membership and the signal
	// that it must not be asked to speak for the cluster.
	ClusterService string `json:"clusterService,omitempty"`

	// ClusterNodeOf is WHICH cluster this host is a member of, as the host itself
	// reports it — the question ClusterNode and ClusterService both leave unsaid.
	//
	// They answer "how is its membership" and never "of what", so a host still
	// joined to a cluster somebody believed they had removed reports Up, Running
	// and healthy, and nothing in Ballast disagrees. Compared against the cluster
	// the centre has AUTHORED for it, this is the difference between "joined to
	// what I asked for" and "joined to something else entirely" — which no other
	// field could tell apart.
	//
	// Seen 2026-08-16: three wiped hosts authored into a new cluster reported
	// clusterNode=Up with no failing conditions while the authored cluster never
	// formed. Every reading was true and about a different cluster.
	ClusterNodeOf string `json:"clusterNodeOf,omitempty"`

	// RebootRequired is true when spec cannot be fully honoured until reboot
	// and RebootPolicy forbids the agent doing it autonomously.
	RebootRequired bool `json:"rebootRequired"`

	// InMaintenance is true when the host is actually out of service — for a
	// cluster member, its node is paused. Observed, not echoed: the declared
	// intent is in the spec, and a host that has been TOLD to drain but has not
	// finished doing so is not yet drained.
	InMaintenance bool `json:"inMaintenance,omitempty"`

	// Autonomous is true when the agent is currently running on its
	// last-honoured cached state because the control plane is unreachable.
	Autonomous bool `json:"autonomous"`

	// ISOLibrary is what this host actually found at its declared library share,
	// whether the cluster's or its own. Nil when none is declared.
	ISOLibrary *ISOLibraryStatus `json:"isoLibrary,omitempty"`

	// WindowsLicence is the host's Windows edition and activation state.
	//
	// Named WindowsLicence, never just Licence: centre/license is BALLAST's own
	// product licensing, a signed vendor token the agent is deliberately never
	// aware of. These are unrelated concerns that would be a genuine hazard to
	// confuse — one gates Ballast features, the other is Microsoft's and gates
	// nothing Ballast does.
	//
	// Observed. What is DECLARED lives on HostSpec.WindowsLicence — the edition
	// the host should run — and the two are compared to decide whether a
	// conversion is owed.
	WindowsLicence *WindowsLicenceStatus `json:"windowsLicence,omitempty"`

	// ISCSI is this host's own array connection, for a standalone host that
	// declares one. Nil for a cluster member — a member's iSCSI state is reported
	// per node on the CLUSTER, where the members can be compared against each
	// other, because that comparison is the whole point: iSCSI fails one node at a
	// time and a single-node view is exactly what hides it.
	ISCSI *ISCSIStatus `json:"iscsi,omitempty"`

	// ImportableVMs are virtual machines whose configuration files sit on this
	// host's storage but which no host has registered.
	//
	// Adopting a LUN that already holds data brings the volume in and then says
	// nothing about the VMs on it. The operator can see 800GB used and no VMs,
	// and the only way to find out what is there is a PowerShell session and a
	// directory listing — which the brief calls a defect, not a runbook step.
	//
	// Reported rather than acted on. Importing is a decision with real hazards
	// (see ImportableVM), so the agent's job is to say what is there and what is
	// wrong with it; the operator chooses.
	ImportableVMs []ImportableVM `json:"importableVMs,omitempty"`

	// ImportScan records when the scan last ran and what it cost, so a stale or
	// skipped scan is legible as such instead of reading as "nothing found".
	// Absent is not empty — the trap this codebase pays for most often.
	ImportScan *ImportScanStatus `json:"importScan,omitempty"`

	// MigrationPasses is the result of the last VMware copy pass this host ran
	// for each migration in flight on it, most importantly the per-disk change
	// markers the next pass resumes from.
	//
	// In status rather than in the job's result message because the centre has
	// to act on the numbers, and parsing them back out of a sentence written for
	// a person is how a pass ends up resuming from a marker nobody checked.
	MigrationPasses []MigrationPassResult `json:"migrationPasses,omitempty"`

	// AgentVersion is the reporting agent's build version, for the UI/diagnostics.
	AgentVersion string `json:"agentVersion,omitempty"`

	// NetworkProfile is the weakest Windows network-location category across the
	// host's connection profiles ("Public" / "Private" / "DomainAuthenticated").
	// A management NIC off the domain profile breaks cross-node WMI/clustering, so
	// the UI surfaces it as a coloured pill with a one-click repair.
	NetworkProfile string `json:"networkProfile,omitempty"`

	// LastContact is when the agent last reached the control plane.
	LastContact time.Time `json:"lastContact"`

	// ObservedAt is when the host was last actually LOOKED AT — the end of a
	// reconcile pass that read it — as distinct from LastContact, which is only
	// when its agent last spoke.
	//
	// The two came apart when the agent gained a keepalive: it resends the last
	// built status every 25s so a slow pass cannot make a working host read
	// offline, which means LastContact refreshes while the metrics, inventory,
	// network profile and licence inside are exactly as old as the last completed
	// pass. A host whose reconcile has wedged on slow WMI goes on reporting every
	// 25s, stays green, and holds its readings frozen with nothing saying so.
	//
	// Stamped by the CENTRE, and only on a report that was not a keepalive, for
	// the same reason as ClusterStatus.ObservedAt: one clock decides freshness,
	// so a host with a skewed clock cannot make a stale reading look current.
	// Not carried on the wire and an agent cannot set it.
	ObservedAt time.Time `json:"observedAt,omitempty"`

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

	// ObservedVMs is every VM present on the host, whether or not Ballast
	// manages it. It is how the centre discovers VMs when Ballast is added to
	// existing infrastructure: the UI lists unmanaged ones under the host and
	// offers to adopt them into desired state. Observed each cycle; a pure read.
	ObservedVMs []ObservedVM `json:"observedVMs,omitempty"`

	Conditions []Condition `json:"conditions,omitempty"`
}

// ObservedVM is a VM discovered on a host — enough to display it and adopt it
// into desired state. Config carries the actual CPU/memory/disks/adapters so an
// adopt reproduces the VM without recreating its VHDXs.
type ObservedVM struct {
	Name       string       `json:"name"`
	VMID       string       `json:"vmId,omitempty"`
	PowerState VMPowerState `json:"powerState,omitempty"`
	// Managed is true when this VM is already a Ballast control-plane object
	// (in the host's desired VM set), so the UI shows it as managed, not as a
	// candidate to adopt.
	Managed bool `json:"managed,omitempty"`
	// Clustered is true when the VM is a highly-available cluster role (adopt
	// places it on the cluster, not the single host).
	Clustered   bool                 `json:"clustered,omitempty"`
	GuestOS     string               `json:"guestOS,omitempty"`
	IPAddress   string               `json:"ipAddress,omitempty"`
	Config      *VMObserved          `json:"config,omitempty"`
	Replication *VMReplicationStatus `json:"replication,omitempty"`
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
	// ManagementVNICs are the observed management-OS vNICs (vEthernet adapters):
	// their switch, VLAN, DNS, network profile, and IP addresses with kind. Drives
	// the networking topology view and lets the operator spot/remove strays.
	ManagementVNICs []ManagementVNICInfo `json:"managementVNICs,omitempty"`
}

// ManagementVNICInfo is an observed management-OS vNIC.
type ManagementVNICInfo struct {
	Name       string   `json:"name"`
	SwitchName string   `json:"switchName,omitempty"`
	VlanID     int      `json:"vlanID,omitempty"`
	DNSServers []string `json:"dnsServers,omitempty"`
	// Profile is the Windows network category on this vNIC (Public/Private/
	// DomainAuthenticated).
	Profile   string        `json:"profile,omitempty"`
	Addresses []VNICAddress `json:"addresses,omitempty"`
	// Gateway is the IPv4 default-route next hop on this vNIC, empty when it has
	// none. The presence of a gateway is what separates a routable management
	// vNIC from an isolated fabric one (storage, live migration), so anything
	// reconstructing intent from observation needs it: a management vNIC written
	// without its gateway loses the host's default route.
	Gateway string `json:"gateway,omitempty"`

	/* LSOEnabled is Large Send Offload on this vNIC, which defeats jumbo frames
	   and is a SEPARATE setting from the uplinks' below it.

	   Read straight off the rig 2026-09-07. On HVNEW04, after the uplinks had
	   been disabled, every physical adapter read LSO off and every vEthernet
	   read it on — and jumbo still did not work. On HVNEW06, where it does,
	   both halves are off. So reporting only the uplinks would have taken the
	   warning away while the fault was still there, which is worse than never
	   having warned.

	   The vNIC is where the host's own traffic is segmented, so this is the
	   half that actually decides what leaves the management OS. */
	LSOEnabled bool `json:"lsoEnabled,omitempty"`

	// MTUBytes is the MTU this interface will actually send at (NlMtu).
	//
	// Observed rather than taken from the spec, because it is reset by anything
	// that re-creates the interface — including the IP reconcile's own
	// remove-and-re-add. A vNIC can therefore sit at 1500 having been set to
	// 9000 an hour earlier, settled and wrong, with nothing else reporting it.
	MTUBytes int `json:"mtuBytes,omitempty"`
}

// VNICAddress is one IPv4 address on a vNIC with its classification.
type VNICAddress struct {
	Address string `json:"address"` // CIDR
	// Kind is host (manual static), cluster (a cluster VIP), dhcp, or apipa.
	Kind string `json:"kind,omitempty"`
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

	/* Shared is true for a Cluster Shared Volume: every member reaches it, and
	   a VM placed there can fail over.

	   A member's LOCAL volumes are reported too, and the difference decides what
	   is safe to put on one. A clustered VM whose disk sits on a local volume
	   cannot fail over to any other node — the role comes online nowhere — so a
	   console that could not tell the two apart could only ever offer one kind
	   or refuse to answer. It used to do the latter: a host with CSVs reported
	   only its CSVs, and a drive somebody had formatted for Hyper-V was
	   invisible everywhere. */
	Shared bool `json:"shared,omitempty"`

	/* Unlettered marks a volume with no drive letter.

	   Stated this way round on purpose: false is "has a letter", which is what
	   every volume Ballast could make had until it learned to format without
	   one. So an older agent, which reports nothing here, is read correctly
	   rather than having every volume it reports look letter-less.

	   A letter-less volume is a deliberate arrangement — a disk to be mounted
	   into a folder, or handed to a cluster — reached by its GUID path instead.
	   It was invisible until now: the inventory only collected volumes that had
	   a letter, which made the option that creates one a way to produce
	   something the inventory then dropped. */
	Unlettered bool `json:"unlettered,omitempty"`
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
	// PrefixLength is the IPv4 prefix of the address in IPv4 (e.g. 24), so the
	// UI can prefill an exact CIDR when re-homing the address onto a management
	// vNIC. Zero when IPv4 is empty.
	PrefixLength int `json:"prefixLength,omitempty"`

	// MTUBytes is the payload MTU this adapter's IP interface currently carries
	// (NlMtu). This is the number that governs, whatever the driver's own jumbo
	// setting claims, so it is what gets compared against desired state.
	MTUBytes int `json:"mtuBytes,omitempty"`

	// JumboKeyword is the driver's advanced-property keyword for jumbo frames,
	// empty when the adapter exposes none.
	//
	// Reported because "this adapter cannot do jumbo frames" and "Ballast has not
	// looked" are different answers and were previously indistinguishable. An
	// adapter with no keyword is a fact about the hardware, not a failure.
	JumboKeyword string `json:"jumboKeyword,omitempty"`

	// JumboValues are the values this driver will actually accept, verbatim.
	//
	// Vendors disagree about what the number means: Intel's 9014 and Mellanox's
	// 9614 both carry a 9000-byte payload, and some drivers offer only "Disabled"
	// and a couple of fixed sizes. Writing a bare 9000 is therefore wrong on most
	// cards. These are carried up so the choice can be explained rather than
	// guessed, and so the console can say what a card will and will not do.
	JumboValues []string `json:"jumboValues,omitempty"`

	// JumboSetting is the value currently selected in the driver, verbatim.
	JumboSetting string `json:"jumboSetting,omitempty"`

	/* RSCEnabled and LSOEnabled are the offloads that re-segment traffic in the
	   NIC, and the third thing that has to agree for jumbo frames.

	   They are the only one of the three that no configuration anywhere shows
	   is wrong: the adapter reports 9000, the IP interface reports 9000, the
	   switch reports 9000, and large frames do not arrive. Found on the rig
	   2026-09-07, where jumbo worked only after both were disabled by hand on
	   every host.

	   Reported always, not only where jumbo is declared — on a 1500 fabric they
	   are doing their job, and it is the reader who decides whether they matter. */
	RSCEnabled bool `json:"rscEnabled,omitempty"`
	LSOEnabled bool `json:"lsoEnabled,omitempty"`
}

type PhysicalDisk struct {
	// DeviceID is unique per BUS, not per host: a local SSD and an iSCSI LUN both
	// report DeviceId 2. It stays because it is short and familiar, but nothing
	// may identify a disk by it alone — see UniqueID.
	DeviceID string `json:"deviceId"`
	// UniqueID identifies the disk on this host without ambiguity, and is what a
	// destructive operation must select on. Keying anything on DeviceID attributed
	// one disk's facts to another: HVNEW04 reported its iSCSI LUN as holding drive
	// F, which belongs to the local SSD sharing its DeviceId.
	UniqueID string `json:"uniqueId,omitempty"`
	// BusType is how the disk is attached (SAS, SATA, iSCSI, …), which is what
	// makes two disks with the same DeviceID tellable apart in the console.
	BusType   string `json:"busType,omitempty"`
	SizeBytes uint64 `json:"sizeBytes"`
	MediaType string `json:"mediaType,omitempty"` // SSD/HDD/SCM
	CanPool   bool   `json:"canPool,omitempty"`
	// IsOSDisk is true for the disk backing the host's boot/system volume, so the
	// UI can exclude it from the data disks available for S2D.
	//
	// It means "no WHOLE-DISK operation", not "invisible". A storage pool needs
	// the whole disk and can never have this one, so excluding it from a pool
	// picker is right. Unallocated space on it is a different question — see
	// LargestFreeExtentBytes.
	IsOSDisk bool `json:"isOSDisk,omitempty"`

	/* What the disk's partition table says, for the space that is NOT spoken
	   for.

	   A 6TB disk whose OS volume was shrunk to 600GB has 5.4TB an operator can
	   carve a Hyper-V volume out of, and Ballast could not express that: a disk
	   was either raw, and claimed whole, or in use and filtered out of every
	   picker. The capacity was not hidden by the OS-disk rule — it was never
	   read.

	   Standalone only, deliberately. A pool owns whole disks, so free space
	   inside a partition table is not something an S2D member has any use for. */

	// PartitionStyle is "GPT", "MBR" or "RAW", as Windows reports it. Worth
	// carrying on its own: an MBR disk cannot address anything past 2TB, so free
	// space beyond that is unreachable until the disk is converted, which is
	// destructive and is a different conversation from creating a volume.
	PartitionStyle string `json:"partitionStyle,omitempty"`
	// AllocatedBytes is how much of the disk existing partitions occupy.
	AllocatedBytes uint64 `json:"allocatedBytes,omitempty"`
	/* LargestFreeExtentBytes is the biggest CONTIGUOUS run of unallocated space,
	   which is what a new partition can actually use.

	   Not Size - AllocatedBytes. Windows commonly parks a recovery partition
	   behind the OS volume, so what is free and what is usable in one piece are
	   different numbers, and only the second one can be offered to somebody. */
	LargestFreeExtentBytes uint64 `json:"largestFreeExtentBytes,omitempty"`
	/* LayoutKnown says the three fields above were READ, rather than defaulted.

	   A pooled disk has no Disk object by design — Storage Spaces owns it — so
	   the layout is genuinely unknown there, and zero free space and unknown free
	   space must not read the same. Absent is not zero. */
	LayoutKnown bool `json:"layoutKnown,omitempty"`
	// DriveLetter is the Windows drive letter assigned to this disk's primary
	// partition (e.g. "E"), empty when the disk is raw or pooled.
	DriveLetter string `json:"driveLetter,omitempty"`
	// PoolName is the storage pool that holds this disk, empty when it is in
	// none. What a disk is CLAIMED BY is the question an operator is asking when
	// they look at a host's disks, and CanPool cannot answer it: it is false for
	// a disk in a pool, a disk holding a volume, one that is offline, removable
	// or too small alike. Reporting "in use" from that alone told an operator a
	// disk was in S2D on hosts with no S2D at all.
	PoolName string `json:"poolName,omitempty"`
	// CannotPoolReason is Windows' own reason the disk cannot join a pool ("In a
	// Pool", "Insufficient Capacity", "Removable Media", …). Empty when it can be
	// pooled, and empty from an agent too old to report it — which is why the
	// console still needs a fallback for a disk that says nothing.
	CannotPoolReason string `json:"cannotPoolReason,omitempty"`

	// Usage is what the pool will DO with this disk — Windows' own
	// PhysicalDisk.Usage: "Auto-Select", "Retired", "Journal", "Hot Spare",
	// "Manual-Select".
	//
	// It is independent of health, and that is the whole point. A RETIRED disk
	// reports HealthStatus Healthy and sits in the pool contributing nothing:
	// Storage Spaces will place no new data on it and will not repair onto it.
	// Health answers "is this disk all right"; this answers "will the pool use
	// it", and only the second one explains a pool that cannot create a volume.
	//
	// Found on bcluster2 2026-08-14 at the end of a two-day incident. Four of
	// twelve disks were retired — one node's worth — after their VMware serials
	// changed and Storage Spaces could no longer identify them. They came back
	// healthy on a power cycle and STAYED retired, which left the pool two
	// allocatable fault domains instead of three, so a three-way mirror could not
	// be created and New-Volume said only "Not Supported". Ballast reported the
	// pool as "Healthy / OK, 12 disks, 0 unhealthy" throughout, because it
	// collected health and never collected this. Every number it showed was true
	// and the one that mattered was missing.
	Usage string `json:"usage,omitempty"`
}

// DiskRetired reports a disk the pool will not allocate to. Kept beside the
// field so every consumer asks the same question the same way; Windows has
// spelled it both "Retired" and "Auto-Select" with and without the hyphen
// across builds, so the comparison is deliberately loose.
func (d PhysicalDisk) DiskRetired() bool {
	return strings.EqualFold(strings.ReplaceAll(strings.TrimSpace(d.Usage), "-", ""), "retired")
}

// MaintenanceSpec declares that a host is out of service for planned work.
//
// For a CLUSTER MEMBER this means the node is paused and its roles moved off —
// pausing without draining would leave VMs running on a node about to be
// rebooted, which is not what anyone means by maintenance. For a standalone host
// there is nothing to drain, so it is purely a centre-side fact: no new
// placements, and its alarms are stood down.
type MaintenanceSpec struct {
	Enabled bool `json:"enabled"`

	// Reason is shown wherever the state is: the next operator to look at a
	// drained host should not have to ask why it is drained.
	Reason string `json:"reason,omitempty"`
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

	// MTUBytes is the payload MTU every uplink in this switch's team must carry.
	// Zero leaves the adapters exactly as the driver has them.
	//
	// It belongs on the SWITCH, not on a vNIC, because that is where the setting
	// physically lives. Jumbo frames on Windows are a driver advanced property
	// (*JumboPacket) on the PHYSICAL adapter; the vSwitch and every vNIC on it
	// inherit whatever the uplinks will carry. Declaring 9000 on a storage vNIC
	// whose uplink is at 1500 configures nothing and reports success.
	//
	// One number for the whole team, deliberately. SET spreads traffic across
	// uplinks and moves it on failure, so a team whose members disagree about MTU
	// works until it fails over onto the member that cannot carry the frame.
	//
	// The value is the payload (9000), not the on-the-wire frame. Drivers count
	// this differently — Intel offers 9014, others 9216 or 9614, each including a
	// different amount of header — so the agent reads what the driver actually
	// offers and picks the smallest value that carries this payload, rather than
	// writing a number that means something else on the next vendor's card.
	MTUBytes int `json:"mtuBytes,omitempty"`
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

	// Purpose is what this vNIC carries. Empty means Management.
	//
	// It exists because storage traffic has requirements no other traffic has,
	// and Ballast had no way to say which vNIC carried it. See VNICPurpose.
	Purpose VNICPurpose `json:"purpose,omitempty"`

	// TeamMemberAdapter pins this vNIC to ONE physical adapter in the switch's
	// SET team (Set-VMNetworkAdapterTeamMapping).
	//
	// This is the field that makes redundancy real, and its absence is why a
	// fleet can look multipathed and not be. Without a mapping, SET places every
	// vNIC on whichever uplink it likes — so two storage vNICs, two subnets, MPIO
	// or SMB Multichannel dutifully running over both, can share a single
	// physical port. One cable then takes the lot, and nothing anywhere reports
	// that the second path was never a second path.
	//
	// Observed 2026-08-24: three hosts with three declared portals reporting one
	// path each, MPIO "in effect", nothing to fail over to.
	//
	// Empty leaves placement to SET, which is correct for management and VM
	// traffic and wrong for storage.
	TeamMemberAdapter string `json:"teamMemberAdapter,omitempty"`

	// MTUBytes is the payload MTU this vNIC's IP interface must carry. Zero
	// means whatever the switch gives it, which is the right default for
	// management and VM traffic.
	//
	// Distinct from the switch's MTU and NOT a substitute for it. The uplink
	// decides what can physically cross the wire; this decides what the IP stack
	// on this interface will put on it. Setting it above the uplink's MTU is the
	// silent failure worth refusing: everything reports configured, and every
	// frame over 1500 is dropped somewhere the host cannot see.
	//
	// Declared per vNIC rather than inherited because a converged switch commonly
	// carries jumbo storage alongside management that must stay at 1500 — a
	// management interface at 9000 talking to a 1500 router is a slow, sporadic
	// fault that looks like anything but MTU.
	MTUBytes int `json:"mtuBytes,omitempty"`

	// RDMA enables RDMA on this vNIC (Enable-NetAdapterRdma).
	//
	// S2D's east-west traffic is SMB Direct, and on RDMA-capable hardware that is
	// the difference between a pool that performs and one that does not. Nil
	// means off: RDMA needs capable adapters AND, for RoCE, DCB/PFC configured
	// end to end including the switches — which Ballast does not administer. It
	// is declared rather than inferred so a fleet that cannot do it says so
	// instead of half-configuring it.
	RDMA *bool `json:"rdma,omitempty"`
}

// VNICPurpose says what a management OS vNIC carries, because the answer
// changes what has to be true of it.
//
// Ballast modelled every vNIC as management. That was survivable while the only
// question was "does it have an address", and stopped being survivable once
// storage was involved: a storage vNIC needs its own subnet, its own physical
// uplink, and — for S2D — RDMA, none of which are preferences. A model that
// cannot distinguish them cannot check any of it.
type VNICPurpose string

const (
	// VNICManagement carries host management. The default, and the one vNIC that
	// must never be torn down casually: it is how the agent is reachable.
	VNICManagement VNICPurpose = ""
	// VNICCluster carries cluster heartbeat and CSV redirected I/O.
	VNICCluster VNICPurpose = "Cluster"
	// VNICLiveMigration carries live migration traffic.
	VNICLiveMigration VNICPurpose = "LiveMigration"
	// VNICStorage carries storage: SMB Direct to an S2D pool, or iSCSI to an
	// array. Two or more, on separate subnets and separate uplinks, is the
	// arrangement both models need — SMB Multichannel and MPIO alike multiplex
	// across interfaces, and cannot manufacture a second one.
	VNICStorage VNICPurpose = "Storage"
)

// IsStorage reports whether this vNIC carries storage traffic.
func (p VNICPurpose) IsStorage() bool { return p == VNICStorage }

// StorageVNICs returns the host's storage-purpose vNICs, which are the
// interfaces an iSCSI session or an SMB Direct connection should be made from.
func (n HostNetworkingSpec) StorageVNICs() []ManagementVNICSpec {
	var out []ManagementVNICSpec
	for _, v := range n.ManagementVNICs {
		if v.Purpose.IsStorage() {
			out = append(out, v)
		}
	}
	return out
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

	// ISCSI connects THIS host to an iSCSI array on its own account, for a
	// standalone host backing its VMs with LUNs from a NAS or SAN.
	//
	// The initiator side was always host-shaped — the service, the portals, the
	// logins and MPIO are per-node settings, and a cluster's spec is that same
	// configuration fanned to every member. Only the reachability differed: it
	// could be declared on a cluster and nowhere else, so a standalone host had no
	// way to reach an array at all.
	//
	// Scoped like the ISO library: declared for ONE of cluster or host and never
	// inherited across. A cluster member takes its initiator configuration from
	// the cluster, because two authorities over one initiator is how a node ends
	// up logged in to targets nobody declared. Setting both is refused with the
	// reason rather than resolved by precedence.
	//
	// What happens to the LUN differs, which is why this is not simply the cluster
	// spec moved. A cluster adopts it as a Cluster Shared Volume; a standalone host
	// has nothing to share it with, so the disk appears in the host's inventory and
	// is provisioned like any other local disk — formatted and given a drive letter.
	ISCSI *ISCSIStorageSpec `json:"iscsi,omitempty"`
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

// VMFolder is a named grouping of VMs for the console's inventory tree.
//
// A folder used to be nothing but the "folder" label on the VMs inside it, so it
// existed only while at least one VM carried it. That makes two ordinary things
// impossible: creating the structure BEFORE the VMs that go in it, and having a
// folder survive its last VM being moved or deleted. An operator who emptied a
// folder to refill it watched it disappear.
//
// So a folder is now declared. A folder EXISTS if it is declared here OR any VM
// in its scope still carries its label — the union, which is what lets every
// folder that predates this record keep working with no migration behind it.
// Membership is still the label: this record says the folder exists, not who is
// in it.
//
// Centre-only, like Dvport and the ISO library: no agent has any use for how an
// operator files their VMs, so it never reaches one and carries no generation.
type VMFolder struct {
	Name string `json:"name"`

	// ClusterName and HostName are the scope, mutually exclusive, exactly as
	// Dvport scopes a port. A folder groups VMs WITHIN one cluster or standalone
	// host, so the same name may be used in both without collision — and a
	// folder cannot span them, because the tree it draws does not.
	ClusterName string `json:"clusterName,omitempty"`
	HostName    string `json:"hostName,omitempty"`
}

// Scope returns the folder's scope, and whether it has one at all. A folder with
// no scope belongs to no tree and is not resolvable; the REST layer refuses to
// store one rather than leaving it to be puzzled over later.
func (f VMFolder) Scope() (kind, name string, scoped bool) {
	switch {
	case f.ClusterName != "":
		return "cluster", f.ClusterName, true
	case f.HostName != "":
		return "host", f.HostName, true
	}
	return "", "", false
}

// SameScope reports whether two folders live in the same scope, which is what
// makes their names collide.
func (f VMFolder) SameScope(o VMFolder) bool {
	return f.ClusterName == o.ClusterName && f.HostName == o.HostName
}

// Dvport is a distributed virtual port: a user-named pairing of a vSwitch and a
// VLAN, giving operators a stable abstraction to attach VM NICs to instead of
// juggling raw switch names and VLAN IDs. It is centre-only metadata; a VM NIC
// references it by name and the centre resolves it to the NIC's SwitchName and
// VLANID, which the existing per-VM reconciler applies. SwitchName is immutable
// after creation.
//
// SCOPED to the thing that owns the switch — a cluster, or a standalone host —
// and unique only within it. A name has to mean one switch, and a vSwitch is only
// real on the hosts that have it: a fleet-wide port would assert that its switch
// exists everywhere, so authoring a VM on another cluster against it resolves
// cleanly at the centre and then fails on the host with a switch that was never
// there. Scoping is what makes the resolution correct; being able to call the
// port VLAN100 on both clusters is the consequence, not the reason.
//
// Never inherited, the same rule as the ISO library and host iSCSI: a cluster
// member takes its ports from the cluster, never from its own host scope, because
// two authorities over one VM's networking is how a NIC ends up on a switch
// nobody declared.
type Dvport struct {
	Name string `json:"name"`

	// ClusterName and HostName are the scope, mutually exclusive, mirroring
	// VMPlacementSpec — which is what the resolution keys off, so the two must not
	// drift apart.
	//
	// Both empty means UNSCOPED: a port authored before scoping existed, whose
	// scope could not be inferred. It is not a fleet-wide port and does not resolve
	// — see DvportUnscopedMessage.
	ClusterName string `json:"clusterName,omitempty"`
	HostName    string `json:"hostName,omitempty"`

	SwitchName  string `json:"switchName"`
	VLANID      int    `json:"vlanId"`
	Description string `json:"description,omitempty"`
}

// Scope returns the dvport's scope, and whether it has one at all.
func (d Dvport) Scope() (kind, name string, scoped bool) {
	switch {
	case d.ClusterName != "":
		return "cluster", d.ClusterName, true
	case d.HostName != "":
		return "host", d.HostName, true
	}
	return "", "", false
}

// SameScope reports whether two dvports live in the same scope, which is what
// makes their names collide.
func (d Dvport) SameScope(o Dvport) bool {
	return d.ClusterName == o.ClusterName && d.HostName == o.HostName
}

// DvportUnscopedMessage explains a port that has no scope, in the terms an
// operator can act on.
//
// It refuses to resolve rather than falling back to matching any scope. Resolving
// it fleet-wide is precisely the behaviour scoping exists to remove, and it would
// bind a NIC to a switch that may not exist on the target — the failure would then
// surface on a host, as a switch-not-found, a long way from the cause.
const DvportUnscopedMessage = "this port has no cluster or host, so Ballast cannot tell which switch it means. " +
	"It was created before ports were scoped and its scope could not be worked out from the switch name. " +
	"Set its cluster or host and it will resolve again; VMs already using it are unaffected, " +
	"because a VM's adapter carries the switch and VLAN it was given at the time."

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
	//
	// DEPRECATED in favour of Storage.Kind, and kept because clusters already
	// exist that set it. Read it through StorageKind(), never directly: a cluster
	// authored before Storage existed has only this, and one authored after has
	// only that. Nothing should have to know which era it came from.
	EnableS2D bool `json:"enableS2D,omitempty"`

	// Storage selects how this cluster's shared storage is provided. Nil means
	// fall back to EnableS2D; see StorageKind().
	//
	// It is a DISCRIMINATOR rather than more optional fields because the models do
	// not overlap. S2D pools local disks and Ballast decides resiliency and
	// capacity; an iSCSI array owns both, and a cluster backed by one has no pool
	// at all — so pool health, capacity, disk counts, repair and rebuild are not
	// "empty" for it, they are meaningless. Bolting iSCSI onto the S2D fields
	// would make half the console assert things about storage that does not work
	// that way, which is what CLAUDE.md means by "future kinds, not retrofits".
	Storage *ClusterStorageSpec `json:"storage,omitempty"`

	// Volumes are the cluster's Cluster Shared Volumes. What a volume MEANS
	// depends on the storage kind: under S2D it is provisioned from the pool to
	// the size and resiliency declared here, while under iSCSI the array already
	// owns the LUN and Ballast only adopts it. See CSVSpec.
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

	// ReplicaBroker, when set, provisions the Hyper-V Replica Broker role on
	// this cluster — required for a cluster to send or receive Hyper-V Replica
	// traffic (replication targets the broker's client access point, and the
	// broker follows VM ownership as roles move between nodes). The centre also
	// fans a ReplicaServerSpec into every member host so each node accepts
	// replica traffic.
	ReplicaBroker *ReplicaBrokerSpec `json:"replicaBroker,omitempty"`

	// ISOLibrary is an SMB share every member mounts boot media from. Declared
	// once here and read by each member from its cluster assignment — never fanned
	// into member HostSpecs, so a host that leaves the cluster loses the library
	// with it and there is no copy to go stale. See ISOLibrarySpec.
	ISOLibrary *ISOLibrarySpec `json:"isoLibrary,omitempty"`
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

	// MTUBytes is the payload MTU every uplink of this switch must carry, on
	// every member. Zero leaves the adapters as their drivers have them.
	//
	// One number for the whole cluster, not per host. A converged fabric where
	// one node's uplinks are at 1500 and the rest are at 9000 is worse than one
	// where none of them are: SMB Multichannel and MPIO both spread across
	// members, so the fabric works until traffic lands on the node that cannot
	// carry the frame. Members disagreeing about MTU is the failure this field
	// exists to make impossible to express.
	//
	// Fanned into every member's VirtualSwitchSpec.MTUBytes. Whether each host's
	// adapters can actually reach it is a fact about that hardware, reported per
	// host rather than assumed here.
	MTUBytes int `json:"mtuBytes,omitempty"`

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

	// Purpose says what the vNIC carries — management, cluster, live migration or
	// storage. Fanned into every member so the storage-network rules apply on the
	// host that has to honour them; see ValidateStorageNetwork.
	Purpose VNICPurpose `json:"purpose,omitempty"`

	// HostAdapters maps a member host to the physical adapter inside this
	// switch's SET team that the vNIC is pinned to on that host.
	//
	// Per host, not one value, because adapter names are a property of the
	// machine and members do not have to agree on them. A host with no entry is
	// left to SET's own placement — which for a storage vNIC means the second
	// path may share a cable with the first.
	HostAdapters map[string]string `json:"hostAdapters,omitempty"`

	// RDMA enables or disables RDMA on the vNIC. Nil leaves it alone: enabling
	// it where the fabric cannot carry it produces storage that works until it is
	// loaded, so it is declared rather than inferred.
	RDMA *bool `json:"rdma,omitempty"`

	// MTUBytes is this vNIC's IPv4 interface MTU on every member. Zero means
	// whatever the switch gives it, which is right for management traffic.
	//
	// Cluster-wide like the switch's, and for the same reason: a storage vNIC at
	// 9000 on two nodes and 1500 on a third is a fabric that works until the
	// third one is asked to carry a full frame.
	MTUBytes int `json:"mtuBytes,omitempty"`

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

// ReplicaBrokerSpec provisions the Hyper-V Replica Broker cluster role: a
// client access point (name + optional static IP) plus the broker resource.
// Replication to or from the cluster addresses the broker's name, never an
// individual node.
type ReplicaBrokerSpec struct {
	// Name is the broker's client access point (computer object) name, e.g.
	// "Newer-Broker". Required, and at most MaxNetBIOSName characters — see
	// ValidateNetBIOSName for why a longer one fails in a way nothing reports.
	Name string `json:"name"`

	// StaticIP optionally assigns the client access point a static address;
	// empty uses DHCP (fails on static-only networks — set it there).
	StaticIP string `json:"staticIP,omitempty"`

	// StoragePath is where inbound replica VHDs land on the members — a CSV
	// path so a replica VM can fail over. Fanned into each member host's
	// ReplicaServerSpec.DefaultStorageLocation.
	StoragePath string `json:"storagePath,omitempty"`
}

// WitnessSpec declares the cluster's quorum witness.
//
// A witness is a vote, not storage. With an even number of votes a cluster can
// split evenly and stop; with three nodes and no witness, losing one leaves the
// remaining two holding quorum by a single vote and the next loss stops the
// cluster. The witness supplies the extra vote.
//
// Setting this is coordinating with Failover Clustering, not replacing it:
// Set-ClusterQuorum is the supported cmdlet and the cluster continues to own
// every quorum decision. Ballast declares which witness should be configured
// and reconciles to it, exactly as it does with New-Cluster.
//
// Only FileShare is enforced today. Cloud is observed and reported but not
// applied — it needs an Azure account key, which is a credential path that has
// to reach the agent without landing in its on-disk cache in the clear.
// Disk is not usable with Storage Spaces Direct at all (it requires shared
// block storage, which S2D has none of) and is refused rather than attempted.
type WitnessSpec struct {
	Type WitnessType `json:"type"`
	// FileSharePath is the UNC path for a FileShare witness, e.g.
	// \\fileserver\bcluster2-witness. The share is reached as the cluster
	// computer object (the CNO), which must have change permission on it, so no
	// credential belongs in this spec.
	FileSharePath string `json:"fileSharePath,omitempty"`
	// CloudAccount / endpoint for cloud witnesses (secret handled out of band).
	CloudAccount string `json:"cloudAccount,omitempty"`

	// Disk identifies the LUN to use for a Disk witness, by the same means a CSV
	// identifies its LUN — a serial number is what every node sees for the same
	// disk, whereas a disk number is per-node and a LUN number is per-target.
	//
	// The witness disk is adopted as a CLUSTERED DISK and never as a CSV. A CSV is
	// mounted on every node at once so all of them can write to it, which is the
	// opposite of what a witness is for: the witness is owned by one node and its
	// ownership is part of how quorum is arbitrated. Adopting it as a CSV would
	// produce a cluster that looked configured and had no working witness.
	//
	// Small is correct. A witness holds a few kilobytes of cluster state, so the
	// 512MB minimum is the real floor and anything above it is unused capacity.
	Disk *CSVSourceSpec `json:"disk,omitempty"`
}

type WitnessType string

const (
	// WitnessNone is node majority with no witness. Declaring it explicitly
	// removes a configured witness; leaving Type empty means "not declared" and
	// changes nothing, so an unmanaged cluster is never quietly reconfigured.
	WitnessNone      WitnessType = "None"
	WitnessFileShare WitnessType = "FileShare"
	WitnessCloud     WitnessType = "Cloud"
	WitnessDisk      WitnessType = "Disk"
)

// ClusterWitnessStatus is the quorum configuration as observed on the cluster,
// which is a different question from what was asked for. Reported by the former.
//
// The console previously stated the witness was "Owned by Failover Clustering"
// and synthesised a quorum state from the member count. That was not an
// observation — a cluster with no witness at all read the same as a healthy one.
type ClusterWitnessStatus struct {
	// Type is what is actually configured: None, FileShare, Cloud or Disk.
	Type WitnessType `json:"type,omitempty"`
	// Path identifies the witness — the UNC share for FileShare, the storage
	// account for Cloud. Empty for None.
	Path string `json:"path,omitempty"`
	// State is the witness cluster resource's state (typically Online). A
	// witness that is configured but Offline is not voting, and the difference
	// only shows up when a node is lost, so it is worth reporting on its own.
	State string `json:"state,omitempty"`
	// QuorumType is the cluster's quorum model as Failover Clustering names it
	// (e.g. "NodeMajority", "NodeAndFileShareMajority").
	QuorumType string `json:"quorumType,omitempty"`
}

// ISCSIStatus is one node's observed iSCSI connection state.
//
// It is per-NODE even though it lives on the cluster, because that is how iSCSI
// fails: one member loses a path or a login while the others are fine, and the
// cluster keeps working until that member is asked to own a disk. A single
// cluster-wide "connected" would hide exactly the condition worth reporting, so
// the reporting node is named and the console compares members.
type ISCSIStatus struct {
	// Node is the member this snapshot came from.
	Node string `json:"node,omitempty"`

	// InitiatorIQN is this node's iSCSI initiator name.
	//
	// Reported because no array can be configured without it, and it is the first
	// thing anyone setting up iSCSI needs: the LUN is granted TO these names. It
	// is knowable only on the host, so an operator who cannot see it here has to
	// open a session on every node to collect them — which CLAUDE.md counts as a
	// defect in Ballast rather than a step in a runbook.
	InitiatorIQN string `json:"initiatorIQN,omitempty"`

	// ServiceRunning is whether the Microsoft iSCSI Initiator service is up. It
	// is set to start on demand by default on Windows Server, so a node that has
	// never had a target configured reports false — which is a state to fix, not
	// a fault to alarm on.
	ServiceRunning bool `json:"serviceRunning,omitempty"`

	// Portals are the discovery addresses this node has registered.
	Portals []string `json:"portals,omitempty"`

	// Sessions are the target logins this node currently holds.
	Sessions []ISCSISession `json:"sessions,omitempty"`

	// MPIOInstalled reports the Multipath-IO feature's presence. With more than
	// one path and no MPIO, Windows presents the same LUN as several disks, and a
	// cluster writing to two of them corrupts data — so this is reported even
	// when everything else looks healthy.
	MPIOInstalled bool `json:"mpioInstalled,omitempty"`

	// MPIOEffective reports that multipath is actually protecting this node —
	// installed, claiming iSCSI devices, and restarted into.
	//
	// It is separate from MPIOInstalled because installed is not protection. The
	// feature asks for a restart, and between installing it and restarting, a LUN
	// reached by several paths is still presented as several disks. Anything that
	// decides whether multipath storage is safe to use must read THIS, not the
	// presence of the feature.
	MPIOEffective bool `json:"mpioEffective,omitempty"`

	// Disks are the block devices this node sees over iSCSI, keyed by the serial
	// a CSVSourceSpec binds to.
	Disks []ISCSIDisk `json:"disks,omitempty"`

	// Message explains a connection failure in the operator's terms, naming the
	// step where the remedy is on the array rather than on the host — Ballast
	// administers the initiator, not the target.
	Message string `json:"message,omitempty"`
}

// ISCSISession is one initiator-to-target login.
type ISCSISession struct {
	TargetIQN string `json:"targetIQN"`
	// Connected distinguishes a target that is known from one that is logged in.
	Connected bool `json:"connected,omitempty"`
	/* Persistent means the login is restored at boot. A non-persistent session
	   works perfectly until the node reboots and then silently does not come
	   back, which on a cluster member means its disks simply do not arrive.

	   NOT omitempty, and that is the point. With it, false and "this agent never
	   reported the field" are the same JSON — which is what made a stored status
	   unreadable when one had to be diagnosed by hand.

	   A companion "was this established at all" flag was written and then
	   removed. It would have to cross the gRPC wire to reach the centre, a proto
	   bool is false when absent, and this machine has protoc-gen-go without
	   protoc — so it would have arrived false for every session and reported
	   every one as unestablished. Worse than the ambiguity it was fixing.

	   It is also less needed than it looks: an agent reporting a session at all
	   ran the script that sets this, so absent means an agent old enough that
	   AgentOutdated is already saying so. */
	Persistent bool `json:"persistent"`
	// Paths is how many connections back this session — more than one only when
	// MPIO is doing its job.
	Paths int `json:"paths,omitempty"`
}

// ISCSIDisk is a block device presented over iSCSI as one node sees it.
type ISCSIDisk struct {
	// SerialNumber is the cluster-wide identity of the LUN; disk numbers are
	// per-node and change across reboots, so they identify nothing shared.
	SerialNumber string `json:"serialNumber,omitempty"`
	Number       int    `json:"number,omitempty"`
	SizeBytes    uint64 `json:"sizeBytes,omitempty"`
	// TargetIQN and LUN say where it came from, for authoring a volume against a
	// disk that has no serial recorded yet.
	TargetIQN string `json:"targetIQN,omitempty"`
	LUN       int    `json:"lun,omitempty"`
	// Clustered is whether this disk is already a cluster resource.
	Clustered bool `json:"clustered,omitempty"`
	// Offline reports a disk present but not online on this node. That is NORMAL
	// for a clustered disk on a non-owner and a problem on the owner, so it is
	// reported rather than judged here.
	Offline bool `json:"offline,omitempty"`
	// Contents describes what is already on the LUN — "a ReFS volume labelled
	// \"Cluster Disk 1\", 7.6GB used of 999.9GB" — so the console can say what
	// adopting it would destroy BEFORE an operator chooses, instead of after the
	// reconcile has refused it.
	//
	// ContentsKnown separates "the disk is blank" from "nobody looked". They read
	// identically as an empty string and have opposite consequences for a wipe,
	// which is the absent-is-not-zero trap in the one place it destroys data.
	Contents      string `json:"contents,omitempty"`
	ContentsKnown bool   `json:"contentsKnown,omitempty"`
}

// ImportableVM is a virtual machine configuration found on storage that no host
// has registered — what an operator gets after adopting a LUN that already held
// somebody else's VMs, or after a cluster was torn down around its storage.
//
// Everything here is OBSERVED. Ballast does not import anything on its own: an
// import is a decision with consequences the agent cannot weigh, and three of
// them destroy or corrupt data if guessed wrong. See RegisteredElsewhere,
// InUseElsewhere and SavedState.
type ImportableVM struct {
	// Name and ID come from the configuration file itself, not from the folder
	// it sits in. A VM's folder is named for the VM at CREATION and never
	// renamed after, so the directory is a stale label, not an identity.
	Name string `json:"name,omitempty"`
	ID   string `json:"id,omitempty"`

	// ConfigPath is the .vmcx, which is what Import-VM and Compare-VM take. It
	// is also the identity the console groups by: every member of a cluster sees
	// the same CSV, so the same VM is reported once per node.
	ConfigPath string `json:"configPath,omitempty"`

	// Volume is the CSV or drive the configuration was found on, so the console
	// can offer the import from the storage the operator is looking at.
	Volume string `json:"volume,omitempty"`

	Generation         int32 `json:"generation,omitempty"`
	ProcessorCount     int32 `json:"processorCount,omitempty"`
	MemoryStartupBytes int64 `json:"memoryStartupBytes,omitempty"`
	// SizeBytes is the whole VM folder, so the console can say what a COPY would
	// cost before an operator chooses one over registering in place.
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// SavedState reports a saved (not shut down) VM. Saved state is captured
	// against the exact processor it ran on, so restoring it on different
	// silicon fails with an error naming neither cause nor remedy. Ballast can
	// establish this before the import rather than after.
	SavedState bool `json:"savedState,omitempty"`

	// RegisteredElsewhere means this VM's ID is already registered on a host
	// Ballast knows about. Importing it a second time gives two hosts a live
	// claim on one set of VHDXs, which corrupts them — this is the single most
	// destructive thing this feature could do, so it is reported by the agent
	// AND refused by the import.
	RegisteredElsewhere bool   `json:"registeredElsewhere,omitempty"`
	RegisteredOn        string `json:"registeredOn,omitempty"`

	// InUseElsewhere means a file in this VM's folder is open — something outside
	// this host is running it. Adopted storage very often came from a cluster
	// that is still up, and importing into a running VM's files is the same
	// corruption by a different route.
	InUseElsewhere bool `json:"inUseElsewhere,omitempty"`

	// Incompatibilities are Compare-VM's findings: the vSwitch this VM wants
	// does not exist here, its ISO is gone, its processor features differ. This
	// is the substance of the feature. Windows returns a code and a sentence;
	// Ballast adds what to do about it, because a code and a sentence is exactly
	// the "raw error passed through" the brief refuses.
	Incompatibilities []VMIncompatibility `json:"incompatibilities,omitempty"`

	// Compatible is Compare-VM's verdict, and CompatKnown separates "it checked
	// and found nothing" from "it never ran". They read identically as an empty
	// list and mean opposite things when the next click is Import.
	Compatible  bool `json:"compatible,omitempty"`
	CompatKnown bool `json:"compatKnown,omitempty"`

	// Error is why this configuration could not be read at all, which is itself
	// worth reporting: a .vmcx that cannot be parsed is a fact about the storage,
	// not a reason to omit the row and imply the disk is empty.
	Error string `json:"error,omitempty"`
}

// VMIncompatibility is one finding from Compare-VM, with Ballast's reading of
// it. Windows gives Code and Message; Kind and Remedy are ours.
type VMIncompatibility struct {
	Code    int32  `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	// Kind buckets the finding so the console can treat it consistently:
	// "Switch", "ISO", "SavedState", "Processor", "Storage", "Other".
	Kind string `json:"kind,omitempty"`
	// Remedy is what closes it, in the operator's terms. Empty means Ballast has
	// nothing useful to add beyond the message, which is honest; inventing a
	// remedy for a finding nobody has seen would be worse than silence.
	Remedy string `json:"remedy,omitempty"`
	// Fixable means the import can proceed once the remedy is applied, as
	// against a finding that only blocks. Compare-VM's own report distinguishes
	// these by whether the incompatibility can be resolved on the report object.
	Fixable bool `json:"fixable,omitempty"`
}

// ImportScanStatus records the scan itself, so the console can tell an empty
// result from a scan that never ran, was skipped for cost, or was cut short.
//
// The alternative is a page that says "no VMs found" for a host whose scan
// timed out, which is this codebase's most expensive bug class: a stale or
// absent reading presented as current fact.
type ImportScanStatus struct {
	// Roots are the paths that were walked, so "nothing found" can be read
	// against where it looked.
	Roots []string `json:"roots,omitempty"`
	// ScannedAt is when the walk completed. Zero means it has never run.
	ScannedAt time.Time `json:"scannedAt,omitempty"`
	// ElapsedMs is what it cost, because a scan is the one part of a pass whose
	// cost scales with somebody else's data.
	ElapsedMs int64 `json:"elapsedMs,omitempty"`
	// Truncated means the walk hit its cap and there may be more. Reported so
	// the count is never presented as complete when it is not.
	Truncated bool `json:"truncated,omitempty"`
	// Message is why the scan did not run or did not finish.
	Message string `json:"message,omitempty"`
}

// ClusterReplicaBrokerStatus is the observed Hyper-V Replica Broker: the role a
// cluster needs before it can send or receive Hyper-V Replica traffic.
type ClusterReplicaBrokerStatus struct {
	// Name is the broker's client access point — the address a primary actually
	// replicates to, not the cluster resource's own name.
	Name string `json:"name,omitempty"`
	// State is the broker resource's cluster state (Online when it can serve).
	State string `json:"state,omitempty"`
	// StorageLocation is where an incoming replica's VHDs land, read from the
	// replication authorization entry rather than the broker resource — the entry
	// is what actually decides it.
	//
	// This is reported because it is the setting that silently breaks: on a
	// cluster it is a CSV path, so deleting the volume leaves every relationship
	// pointing at a directory that is not there, and nothing says so until a
	// relationship is enabled and Hyper-V answers "failed to enable replication".
	StorageLocation string `json:"storageLocation,omitempty"`
}

// ClusterStorageKind names how a cluster's shared storage is provided.
type ClusterStorageKind string

const (
	// StorageKindS2D is Storage Spaces Direct: local disks pooled by the cluster,
	// with Ballast declaring capacity and resiliency.
	StorageKindS2D ClusterStorageKind = "S2D"
	// StorageKindISCSI is shared block storage from an iSCSI array. The array
	// owns the LUNs, their size and their redundancy; Ballast connects the nodes
	// to it and adopts what it presents. There is no pool to report on.
	StorageKindISCSI ClusterStorageKind = "iSCSI"
)

// ClusterStorageSpec selects and configures the cluster's storage model.
type ClusterStorageSpec struct {
	Kind ClusterStorageKind `json:"kind"`
	// ISCSI is required when Kind is iSCSI and ignored otherwise.
	ISCSI *ISCSIStorageSpec `json:"iscsi,omitempty"`
}

// StorageKind is the cluster's storage model, tolerating specs authored before
// ClusterStorageSpec existed. Every consumer should use this rather than reading
// either field, so an old cluster and a new one are indistinguishable to it.
func (s ClusterSpec) StorageKind() ClusterStorageKind {
	if s.Storage != nil && s.Storage.Kind != "" {
		return s.Storage.Kind
	}
	if s.EnableS2D {
		return StorageKindS2D
	}
	return ""
}

// CHAPScope selects which iSCSI sessions carry the CHAP credential.
type CHAPScope string

const (
	// CHAPAuto tries discovery without CHAP and falls back to it if the array
	// refuses. The default, and correct without knowing what the array wants.
	CHAPAuto CHAPScope = ""
	// CHAPTargetOnly never sends CHAP on discovery. Synology and most arrays.
	CHAPTargetOnly CHAPScope = "TargetOnly"
	// CHAPDiscoveryAndTarget authenticates the discovery session too, from the
	// first attempt — for an array that requires it, or a policy that mandates it.
	CHAPDiscoveryAndTarget CHAPScope = "DiscoveryAndTarget"
)

// AuthenticatesDiscovery reports whether the discovery session should carry the
// credential on its FIRST attempt. Auto does not: it earns the credential by
// being refused without one.
func (c CHAPScope) AuthenticatesDiscovery() bool { return c == CHAPDiscoveryAndTarget }

// MayRetryDiscoveryWithCHAP reports whether a refused unauthenticated discovery
// should be retried with the credential. Only Auto does — an operator who has
// said TargetOnly has stated the array does not want it, and retrying anyway
// would be the console overruling them.
func (c CHAPScope) MayRetryDiscoveryWithCHAP() bool { return c == CHAPAuto }

// ISCSIStorageSpec connects every cluster member to an iSCSI array.
//
// Ballast's job here is ONLY the initiator side: start the service, register the
// portals, log the node in, and keep those logins persistent across reboots. It
// does not create LUNs, set their size, or configure the array's redundancy —
// that is the array's, and it is outside what Ballast administers. Where a LUN
// is missing or too small the console says so and names the step, rather than
// failing obscurely.
type ISCSIStorageSpec struct {
	// Portals are the array's discovery addresses ("10.0.70.10" or
	// "10.0.70.10:3260"). More than one is normal and is what MPIO needs: two
	// portals on separate fabrics is the usual reason an iSCSI cluster survives a
	// switch failure.
	Portals []string `json:"portals"`

	// Targets are the target IQNs each node should log in to. Empty means log in
	// to every target the portals advertise, which is convenient for a dedicated
	// array and wrong for a shared one — so it is a deliberate choice, not a
	// default anybody falls into.
	Targets []string `json:"targets,omitempty"`

	// CredentialSecret names a stored credential holding the CHAP username and
	// secret. Empty means no CHAP.
	//
	// It is a secret reference rather than a value because the spec is stored in
	// Postgres and delivered to every member: an inline CHAP secret would sit in
	// the desired state of each node and in every spec-history row of the cluster.
	CredentialSecret string `json:"credentialSecret,omitempty"`

	// MutualCHAP requires the target to authenticate back to the initiator. It
	// needs the same credential configured on the array, and it is off by default
	// because a mismatch presents as a login failure with no indication which
	// direction failed.
	MutualCHAP bool `json:"mutualCHAP,omitempty"`

	// CHAPScope says WHERE the credential is presented.
	//
	// iSCSI has two session types and they authenticate independently: the
	// discovery (SendTargets) session and the normal session that logs in to a
	// target. An array can require CHAP on one, both or neither, and most —
	// Synology, and TrueNAS by default — put it on the target and mask discovery
	// by allowed-initiator list instead.
	//
	// Empty (CHAPAuto) is the default and needs no knowledge of the array: try
	// discovery unauthenticated, and offer the credential only if that is
	// refused. Both kinds of array then work and neither is sent something it
	// will reject.
	//
	// The explicit values exist because "just try it" is not always free. Some
	// arrays log an unauthenticated discovery attempt as an intrusion, or lock
	// the initiator out after a few; and a shop whose policy mandates CHAP
	// everywhere wants that DECLARED rather than inferred at runtime.
	CHAPScope CHAPScope `json:"chapScope,omitempty"`

	// EnableMPIO installs and configures Multipath I/O.
	//
	// With several portals and no MPIO, Windows sees the SAME LUN once per path
	// as separate disks. Clustering will then happily use one path's disk while
	// another node uses a different path to the same blocks, which is a data
	// corruption, not a performance problem. So this defaults ON when more than
	// one portal is declared, and turning it off with multiple portals is refused
	// rather than honoured.
	EnableMPIO *bool `json:"enableMPIO,omitempty"`
}

// MPIORequired reports whether MPIO must be configured for this spec, and
// whether the operator's setting was overridden.
//
// Multiple paths without MPIO means Windows presents one LUN as several disks,
// and a cluster that writes to two of them is corrupting data rather than
// performing badly. That is not a preference to honour.
func (s ISCSIStorageSpec) MPIORequired() (required bool, overridden bool) {
	multipath := len(s.Portals) > 1
	if s.EnableMPIO == nil {
		return multipath, false
	}
	if multipath && !*s.EnableMPIO {
		return true, true
	}
	return *s.EnableMPIO, false
}

// CSVSpec is one Cluster Shared Volume. Which fields apply depends on the
// cluster's storage kind, and the ones that do not apply are not merely unset —
// they have no meaning.
type CSVSpec struct {
	Name string `json:"name"`

	// SizeBytes, ResiliencyType and NumberOfCopies are S2D ONLY: they are what
	// Ballast asks the pool to provision. Under iSCSI the array already decided
	// all three before Ballast saw the LUN, so a value here would be a wish with
	// nothing to enforce it — the console does not offer them, and the reconciler
	// must not read them.
	SizeBytes      uint64 `json:"sizeBytes,omitempty"`
	ResiliencyType string `json:"resiliencyType,omitempty"` // Mirror/Parity
	NumberOfCopies int    `json:"numberOfCopies,omitempty"`

	// Source identifies an EXISTING disk to adopt as this volume, for storage
	// kinds where Ballast does not create it. Required under iSCSI.
	Source *CSVSourceSpec `json:"source,omitempty"`
}

// CSVSourceSpec identifies a disk the array presents, so a CSV can be bound to
// the right LUN on every node.
//
// Identification matters more here than it looks. A LUN number is per-target and
// a disk number is per-node and changes across reboots, so neither identifies the
// same storage cluster-wide. The serial number does, and it is what every node
// sees for the same LUN — which is exactly what a clustered disk needs to be.
type CSVSourceSpec struct {
	// SerialNumber is the disk's unique serial as Windows reports it
	// (Get-Disk .SerialNumber). The reliable cluster-wide identifier.
	SerialNumber string `json:"serialNumber,omitempty"`
	// TargetIQN and LUN narrow the search when a serial is not known yet — for
	// instance when authoring a volume before the array has presented it.
	TargetIQN string `json:"targetIQN,omitempty"`
	LUN       *int   `json:"lun,omitempty"`
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

	// Witness is the observed quorum configuration. Nil means the former has not
	// reported it yet — which is not the same as "no witness", and the console
	// must not render it as such.
	Witness *ClusterWitnessStatus `json:"witness,omitempty"`

	// ISCSI is the observed iSCSI connection state, ONE ENTRY PER MEMBER.
	//
	// A list rather than a single value because every member reports its own
	// cluster status: a single field would be overwritten by whichever node
	// reported last, leaving the console showing one arbitrary member's view and
	// calling it the cluster's. That is precisely the failure iSCSI produces — one
	// node loses a path while the others are fine — so the shape that hides it is
	// the wrong shape.
	//
	// The centre MERGES on receipt, replacing the reporting node's entry and
	// keeping the rest; an agent only ever sends its own.
	//
	// It exists rather than being folded into Pool because an iSCSI cluster has
	// no pool: capacity, resiliency, disk health and repair all belong to the
	// array, and reporting empty pool fields would say Ballast looked and found
	// nothing when in fact there was never anything of that shape to look at.
	ISCSI []ISCSIStatus `json:"iscsi,omitempty"`

	// ReplicaBroker is the observed Hyper-V Replica Broker. Nil means none was
	// found (or none reported yet); a non-nil value while ClusterSpec.ReplicaBroker
	// is nil means the cluster is running a replication endpoint Ballast does not
	// manage, which is the state the centre adopts rather than ignores.
	ReplicaBroker *ClusterReplicaBrokerStatus `json:"replicaBroker,omitempty"`

	// FunctionalLevel is the cluster's operating mode as Failover Clustering
	// reports it (9 = Server 2016, 10 = 2019, 11 = 2022, 12 = 2025). Zero means
	// it has not been reported.
	//
	// It matters because a rolling OS upgrade does NOT raise it. Take a cluster to
	// a newer Windows node by node — which is what cluster-aware updating does —
	// and when the last node returns the cluster still runs at the old level: the
	// new OS's features stay unavailable and the upgrade is not finished until
	// Update-ClusterFunctionalLevel is run. That step is deliberately manual
	// because it cannot be undone, which is exactly why something has to say it is
	// outstanding.
	FunctionalLevel int `json:"functionalLevel,omitempty"`

	// NodeOSBuild is the Windows build of the node that reported, which bounds the
	// level the cluster could run at. Together with FunctionalLevel it says
	// whether an upgrade was completed or merely performed.
	NodeOSBuild int `json:"nodeOSBuild,omitempty"`

	// ObservedAt is when this snapshot was taken, stamped by the CENTRE when the
	// report arrives. It is not carried on the wire and an agent cannot set it:
	// one clock decides, so the value is comparable with the centre's own
	// timestamps and immune to host clock skew.
	//
	// Receipt time is the observation time here because the former only sends a
	// cluster report on a cycle where it has just run the cluster sweep (see
	// clusterReconcileEvery); it does not resend a cached one each heartbeat.
	//
	// This exists because "healthy" is a claim about a moment. A cluster status
	// from before a node was drained says nothing about the cluster after it, and
	// a rolling update that reads one as current will happily take a second node
	// out while the first one's disks are still rebuilding. Anything gating an
	// action on cluster health must check this is newer than whatever it did
	// last, not merely that the numbers look good.
	ObservedAt time.Time `json:"observedAt,omitempty"`

	// StateUnreadable means the reporting member could not read the cluster at all
	// this pass — its cluster service was stopped, or it has been ejected from
	// membership — so every observed field below is EMPTY because nothing was
	// seen, not because nothing is there.
	//
	// Every member reports, and a report replaces the last one, so without this a
	// single blind member overwrites the readings of every healthy one. On the rig
	// bcluster2's first member was quarantined — which stops its cluster service —
	// and the whole cluster went dark to the centre: no nodes, no volumes, three
	// of seven steps, while two healthy members could see it perfectly.
	StateUnreadable bool `json:"stateUnreadable,omitempty"`

	/* NoMemberReporting means not one member of this cluster has an agent
	   checking in, so nothing here is being refreshed by anybody.

	   StateUnreadable is a member that TRIED and could not see the cluster. This
	   is the case where no member is left to try, and it has to be derived
	   rather than reported for exactly that reason: the flag that would say so
	   is set by an agent running a cluster pass, and a powered-off host runs no
	   passes. Nothing sets it, the last good reading stays on record, and the
	   console goes on drawing it.

	   That is not hypothetical. Ballast shut down all three members of Primary1
	   through its own ShutdownHost job, recorded all three as succeeded, watched
	   every agent go silent — and forty hours later still showed the cluster
	   Ready with three nodes Up. Every fact needed to know better was in the
	   store; nothing put them together.

	   Derived by the CENTRE at read time from host liveness, like ObservedAt it
	   is not carried on the wire and an agent cannot set it. It is not persisted
	   either: it describes this moment, and a stored copy would be one more
	   reading going stale on the shelf. */
	NoMemberReporting bool `json:"noMemberReporting,omitempty"`

	// LastMemberContact is when the most recently heard-from member last checked
	// in, so "nobody is reporting" can say since when. Zero when no member has
	// ever reported, which is a cluster that has not come up rather than one
	// that has gone away. Centre-derived at read time, like NoMemberReporting.
	LastMemberContact time.Time `json:"lastMemberContact,omitempty"`

	// MemberCount and MembersOnline are the liveness behind the two fields
	// above, kept so the console can say "0 of 3 members reporting" rather than
	// recount from a host list it may not have loaded. Centre-derived.
	MemberCount   int `json:"memberCount,omitempty"`
	MembersOnline int `json:"membersOnline,omitempty"`

	/* ShutDownFromBallast means every silent member went quiet because Ballast
	   shut it down, and the cluster is therefore OFF rather than LOST.

	   The distinction is the whole point of reporting it. Both look identical
	   from the centre — no member answering — and they call for opposite
	   responses: one is somebody's Tuesday evening and should raise nothing, the
	   other is an outage. A console that cannot tell them apart either alarms
	   through every planned shutdown until its alarms mean nothing, or stays
	   calm through a real one.

	   Derived from JOB HISTORY, not from a flag set when the button was pressed.
	   The jobs are already durable, already attributed to whoever asked, and
	   already correlated with the silence they explain by the same rule the host
	   alarms use — so there is one account of why a host is quiet rather than
	   two that can disagree. A stored flag would also have to be got right when
	   a host is shut down some other way, and it would be wrong for every
	   cluster shut down before the flag existed.

	   Requires ALL of them: a cluster where two members were shut down and a
	   third simply vanished is not a planned shutdown, and calling it one hides
	   the member that matters. Centre-derived at read time. */
	ShutDownFromBallast bool `json:"shutDownFromBallast,omitempty"`

	// ShutDownAt and ShutDownBy are when the LAST member was shut down and who
	// asked for it, so the console can say whose shutdown this was rather than
	// only that it was one. Set only alongside ShutDownFromBallast.
	ShutDownAt time.Time `json:"shutDownAt,omitempty"`
	ShutDownBy string    `json:"shutDownBy,omitempty"`
}

// ClusterNetworkStatus is one cluster network: subnet (CIDR), role
// (None/Cluster/ClusterAndClient) and state (Up/Down/Partitioned/Unavailable).
// The console uses it to pick a live-migration network and flag a bad one.
type ClusterNetworkStatus struct {
	Name  string `json:"name"`
	CIDR  string `json:"cidr,omitempty"`
	Role  string `json:"role,omitempty"`
	State string `json:"state,omitempty"`

	// Metric is what actually decides where cluster and CSV/SMB traffic goes:
	// among the networks enabled for cluster use, the LOWEST metric wins.
	// Windows assigns it automatically, preferring networks with no gateway, so
	// an operator cannot infer it from the role — and when a storage network
	// misbehaves, "which network is storage actually on" is the first question
	// and the console could not answer it.
	Metric int `json:"metric,omitempty"`
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
	//
	// It EXCLUDES disks in storage maintenance mode, which are counted by
	// DisksInMaintenance instead. A disk in maintenance reports HealthStatus
	// Warning, so counting on health alone folded a planned drain into the
	// failure count: the console showed "4 of 12 disks unhealthy · Repair pool"
	// beside a pool Windows called Healthy, and offered a repair for a state the
	// operator had asked for. The two have opposite remedies — one needs a disk
	// replaced, the other needs the node resumed — so they cannot share a number.
	UnhealthyDisks int `json:"unhealthyDisks,omitempty"`
	// DisksInMaintenance is the number of physical disks the cluster has taken
	// into storage maintenance mode. Failover Clustering does this itself as part
	// of Suspend-ClusterNode -Drain and reverses it on resume, so a non-zero value
	// during a drain is expected and needs no action; a non-zero value with no
	// node drained is disks stranded by a half-finished operation, which holds
	// every space degraded until cleared.
	DisksInMaintenance int `json:"disksInMaintenance,omitempty"`
	// TotalDisks is the pool's physical-disk count, for context.
	TotalDisks int `json:"totalDisks,omitempty"`

	// Resyncing reports that Storage Spaces Direct is actively rebuilding data —
	// a repair or regeneration job is running. This must be distinguished from a
	// pool that is degraded and stuck: a resync is normal, self-resolving work
	// (it follows volume creation, a disk replacement or a node returning), and
	// it makes the pool and its disks report non-Healthy transiently. Telling an
	// operator to "repair the pool" while it is already repairing is wrong, and
	// running a Repair or Rebuild on top of a live resync is actively harmful, so
	// the observed job is reported rather than inferred from health alone.
	Resyncing bool `json:"resyncing,omitempty"`
	// ResyncPercent is the running job's completion (0-100), and ResyncJob names
	// what is running (e.g. "Repair", "Regeneration") so the console can say what
	// is happening rather than that something is wrong.
	ResyncPercent int `json:"resyncPercent,omitempty"`

	// ResyncRemainingBytes is the work still outstanding across the running jobs.
	// It exists because the PERCENTAGE cannot be trusted as overall progress: a
	// finished job vanishes from Get-StorageJob, so when the next one starts the
	// percentage drops back toward zero. Observed live going 21% -> 84% -> 0.
	//
	// There is no knowable total to measure the whole rebuild against, so rather
	// than invent continuity across jobs that cannot be correlated, report the
	// figure that IS comparable across them. It also separates the two causes of
	// a reset: across phases the remaining bytes trend down (the rebuild is
	// converging), whereas a repair that keeps restarting returns to the same
	// value and retains nothing — a fault that otherwise looks like slow progress.
	ResyncRemainingBytes uint64 `json:"resyncRemainingBytes,omitempty"`
	ResyncJob            string `json:"resyncJob,omitempty"`
}

// ClusterNodeStatus is a cluster node and its observed state — Up, Paused (the
// node is drained / in maintenance), or Down. The UI greys a drained/down node.
type ClusterNodeStatus struct {
	Name  string `json:"name"`
	State string `json:"state,omitempty"`

	// StatusInformation is what Failover Clustering says ABOUT that state, from
	// Get-ClusterNode .StatusInformation — "Quarantined", "Isolated", "Normal",
	// or empty.
	//
	// It exists because State alone cannot tell the two most important cases
	// apart. A QUARANTINED node reports State=Down, exactly like a node that is
	// switched off, and quarantine is not an outage: it is the cluster refusing
	// to readmit a node that left and rejoined three times in an hour, it stops
	// the cluster service deliberately, and it clears with
	// Start-ClusterNode -ClearQuarantine rather than by fixing anything on the
	// node. An operator shown "Down" goes looking for a dead machine.
	//
	// Found on the rig 2026-08-13. bcluster2's guests flapped ~13 times each —
	// the hypervisor beneath them was starved — and HVNEW01 and HVNEW02 ended
	// with their cluster service stopped and State=Down. Ballast could not say
	// why, while shipping a ClusterClearQuarantine job whose precondition it had
	// no way to observe: the console offered a remedy it could not tell you was
	// the right one.
	StatusInformation string `json:"statusInformation,omitempty"`
}

// NodeQuarantined reports whether a node is quarantined — held out of the
// cluster by Failover Clustering rather than absent. Kept here beside the field
// so every consumer asks the same question the same way; Windows has used more
// than one spelling of the word across builds, so matching on a prefix is
// deliberate.
func (n ClusterNodeStatus) NodeQuarantined() bool {
	return strings.HasPrefix(strings.ToLower(n.StatusInformation), "quarantine")
}

// NodeIsolated reports whether a node is isolated: still a member, but out of
// communication with the cluster. Unlike quarantine this usually resolves
// itself when communication returns, so it is a different thing to show and a
// different thing to act on.
func (n ClusterNodeStatus) NodeIsolated() bool {
	return strings.EqualFold(strings.TrimSpace(n.StatusInformation), "isolated")
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

	// Resources are the group's own resources, collected when the group is not
	// Online. A group state names the group and not the fault: "SDDC Group is
	// PartialOnline" says some of it came up and some did not, and which is the
	// whole question.
	//
	// Seen on S2DCluster 2026-08-16. The console reported PartialOnline with no
	// detail, and the group is one Failover Cluster Manager does not display at
	// all — so the operator was sent to look in a GUI that never shows it. The
	// answer took one Get-ClusterResource: of three resources the Health Service
	// and the performance-history disk were Online and only SDDC Management was
	// Offline, which is a management resource in neither the storage nor the VM
	// data path. That is a fact the agent can establish every pass.
	//
	// Empty for a group that is Online, where there is nothing to explain.
	Resources []ClusterResourceStatus `json:"resources,omitempty"`
}

// ClusterResourceStatus is one resource inside a cluster group, reported so a
// group that is not Online can name what is holding it down rather than leaving
// the operator to go and look.
type ClusterResourceStatus struct {
	Name string `json:"name"`
	// Type is the cluster resource type as Windows names it, e.g. "IP Address",
	// "Physical Disk", "SDDC Management". It is what distinguishes a resource
	// that carries data from one that only manages.
	Type  string `json:"type,omitempty"`
	State string `json:"state,omitempty"`
}

// CSVStatus is one Cluster Shared Volume and its current owner node.
type CSVStatus struct {
	Name      string `json:"name"`
	OwnerNode string `json:"ownerNode,omitempty"`
	State     string `json:"state,omitempty"`
	/* SerialNumber is the serial of the DISK this volume sits on — the same
	   identifier CSVSpec.Source uses, and the only thing tying an observed
	   volume to the LUN carrying it.

	   Without it the two lists cannot be joined. The console showed a LUN
	   holding DS1 as "unclaimed" and had no way to offer to declare it: the CSV
	   said only its name, the disk said only its serial, and nothing connected
	   them. Reported for display and for that join; the DECLARED source is
	   still what the reconcile binds on. */
	SerialNumber string `json:"serialNumber,omitempty"`

	// Health and Operational are the BACKING VIRTUAL DISK's status, observed
	// separately from the pool's. A pool and its volumes fail independently: a
	// volume can be detached or degraded while every disk and the pool itself are
	// Healthy. Without this the console had only pool health to go on and blamed
	// the pool for a volume's problem — telling the operator to repair storage
	// that was fine. Empty when the volume could not be matched to the CSV.
	Health      string `json:"health,omitempty"`
	Operational string `json:"operational,omitempty"`
	// DetachedReason explains an unattached volume. "By Policy" is the normal
	// resting state for a clustered volume (the cluster owns attachment), so it
	// must not be read as a fault.
	DetachedReason string `json:"detachedReason,omitempty"`

	// SizeBytes and FreeBytes are the VOLUME's observed capacity — what the
	// filesystem actually offers — not the backing virtual disk's allocation.
	//
	// They exist so a CSV's declared size (CSVSpec.SizeBytes) can be compared
	// against reality. EnsureCSV creates a volume and then, on every later pass,
	// returns as soon as it finds one with that name: the declared size is never
	// looked at again. So changing it today bumps the generation, settles, and
	// reports converged while nothing happened — the same shape as a broker's
	// StaticIP being applied only at creation.
	//
	// Reporting the volume rather than the virtual disk is deliberate. Growing a
	// CSV is two steps (grow the virtual disk, then extend the partition into
	// it), and a half-done grow leaves the virtual disk larger with the usable
	// space unchanged. Reading the virtual disk would confirm a resize that never
	// reached the filesystem.
	//
	// Zero means the size was not observed, never that the volume is empty.
	SizeBytes uint64 `json:"sizeBytes,omitempty"`
	FreeBytes uint64 `json:"freeBytes,omitempty"`
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
	ID       string            `json:"id"`
	HostName string            `json:"hostName"` // target host whose agent runs it
	Kind     string            `json:"kind"`
	Params   map[string]string `json:"params,omitempty"`
	State    JobState          `json:"state"`
	Message  string            `json:"message,omitempty"` // result detail / error
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
	JobVMStart       = "VMStart"            // params: vm
	JobVMStop        = "VMStop"             // params: vm
	JobVMRestart     = "VMRestart"          // params: vm — one-shot guest restart
	JobVMCheckpoint  = "VMCheckpoint"       // params: vm, name
	JobVMApplyCheck  = "VMApplyCheckpoint"  // params: vm, name
	JobVMRemoveCheck = "VMRemoveCheckpoint" // params: vm, name
	JobVMExport      = "VMExport"           // params: vm, path
	JobVMClone       = "VMClone"            // params: vm (source), name (new), folder (target) — copy an Off VM into an independent new one

	// JobVMDiscardSavedState throws away a saved VM's memory image so it becomes
	// Off. A clustered VM lands in Saved rather than Off whenever its role goes
	// offline, because AutomaticStopAction defaults to Save — and a saved VM
	// cannot be captured, cloned, or have its disk moved, because the disk holds
	// writes that were still in memory. Discarding is the way out and, unlike
	// starting the VM, it does not disturb a generalised image.
	JobVMDiscardSavedState = "VMDiscardSavedState" // params: vm
	// JobVMDiscardSavedStateAndStart discards the memory image and then starts
	// the VM, because "get it running again" is one intention and splitting it
	// into two operator steps invites stopping half way — leaving a VM Off that
	// somebody wanted Running.
	JobVMDiscardSavedStateAndStart = "VMDiscardSavedStateAndStart" // params: vm
	JobClusterAddNode              = "ClusterAddNode"              // params: node
	JobClusterEvict                = "ClusterEvict"                // params: node
	JobNodeDrain                   = "NodeDrain"                   // params: node — pause + move roles off (maintenance)
	JobNodeResume                  = "NodeResume"                  // params: node — resume into the cluster

	// JobClusterUpdateFunctionalLevel raises the cluster's operating mode to what
	// its nodes now support, after a rolling OS upgrade has taken every node to a
	// newer Windows. Run on the former. IRREVERSIBLE — a cluster cannot be taken
	// back down a functional level, and a node running the older OS can no longer
	// join afterwards — which is why it is an explicit action and never something
	// the reconcile loop does on its own.
	JobClusterUpdateFunctionalLevel = "ClusterUpdateFunctionalLevel" // no params

	// JobClusterStartCoreGroup brings the cluster's own resources back online: the
	// core group first, then the storage that could not come online behind it.
	//
	// A job rather than reconcile state. Whether a cluster resource should be
	// online is the CLUSTER's decision, made continuously by its own service, and
	// a reconcile loop asserting it would fight the cluster every pass — including
	// while it is deliberately moving a group between nodes. This is an operator
	// saying "try again now", which is what the situation actually calls for.
	JobClusterStartCoreGroup = "ClusterStartCoreGroup" // no params

	// JobClusterClearQuarantine readmits a node the cluster has quarantined.
	//
	// MUST be enqueued on a different member. Quarantine works by stopping the
	// cluster service on the node it applies to, so that node cannot act for the
	// cluster — asking it to readmit itself asks the one machine that has been cut
	// off, which is also how Ballast came to be blind to a whole cluster whose
	// designated former was the quarantined node.
	JobClusterClearQuarantine = "ClusterClearQuarantine" // params: node

	JobClusterMoveGroup = "ClusterMoveGroup" // params: group, node — move/fail over a clustered role to node
	JobClusterMoveCSV   = "ClusterMoveCSV"   // params: volume, node — move CSV ownership to node

	/* JobClusterVolumeOnline asks the cluster to bring one volume online.

	   An offline CSV is the cluster's decision and it usually has a reason, but
	   an operator whose storage is down had to open Failover Cluster Manager and
	   right-click — a recovery reachable only from outside the product, which
	   CLAUDE.md counts as a defect rather than a runbook step. It bites hardest
	   exactly here: after something has already gone wrong.

	   Coordination, not a decision. The cluster still owns whether the resource
	   may come online, and its refusal is reported in its own words. */
	JobClusterVolumeOnline = "ClusterVolumeOnline" // params: volume
	JobClusterValidate     = "ClusterValidate"     // params: nodes (optional, comma list), include (optional) — Test-Cluster
	JobClusterMoveVM       = "ClusterMoveVM"       // params: vm, node — live-migrate a clustered VM role to node
	/* JobCopyVM moves a VM by EXPORTING it to the destination's storage and
	   importing it there, then removing the original. params: vm, destHost,
	   destPath, sourceCluster/targetCluster (optional), networkMap (optional
	   JSON). Runs on the source host.

	   The heavy path, and the one that asks least of the two hosts. Move-VM
	   needs live migration configured at both ends, Kerberos delegation between
	   the computer accounts and a compatible destination; this needs the guest
	   stopped and somewhere to write. Which is the right trade depends entirely
	   on whether the two hosts were built to know about each other, and a new
	   environment by definition was not. */
	JobCopyVM = "CopyVM"

	/* JobClearVMExport removes an export CopyVM left on a destination.
	   params: vm, destHost, destPath. Runs on the source host, which is the one
	   that can already reach the destination.

	   A copy that fails after the export has written refuses to start again,
	   because writing a second export over a first is how a half-copy becomes an
	   unreadable one. That refusal used to end at "remove it, or import it if it
	   is complete" — an instruction to open a session on the destination, which
	   the brief calls a defect in Ballast rather than a runbook step. It is
	   worst exactly here: the operator is mid-evacuation, something has already
	   gone wrong, and the leftover is Ballast's own. */
	JobClearVMExport = "ClearVMExport"

	JobMigrateVM = "MigrateVM" // params: vm, destHost, destPath, sourceCluster/targetCluster (optional, the cluster halves of a cross-boundary move), networkMap (optional JSON [{sourceSwitch,targetSwitch,vlanId}]) — shared-nothing live migration to another host (run on the source host)

	// JobVMMoveStorage relocates a VM's files to another datastore WITHOUT moving
	// the VM itself — Hyper-V storage migration, which runs live. The centre
	// dictates the destination path of every file rather than letting Hyper-V
	// choose a layout, so it can author the VM's new desired disk paths when the
	// job succeeds. params: vm, dest (datastore root; files land in <dest>\<vm>).
	JobVMMoveStorage       = "VMMoveStorage"
	JobClusterLog          = "ClusterLog"          // params: span (minutes), filter (optional substring) — Get-ClusterLog, relevant lines
	JobMigrationDelegation = "MigrationDelegation" // run on the former: params: nodes (optional comma list) — set Kerberos constrained delegation for live migration

	// Hyper-V Replica failover. All run on the REPLICA host (the target the VM
	// replicates to), resolved by the centre from the VM's replication spec.
	// Planned/unplanned failover invert the relationship; the centre rewrites the
	// VM's desired state (placement + replication direction) when the job
	// succeeds so the reconcile loop honours the new topology — and keeps honouring
	// it if the centre goes offline. fromKind/fromName record the VM's placement at
	// enqueue so the rewrite applies exactly once.
	JobVMTestFailover       = "VMTestFailover"       // params: vm — non-disruptive test failover (temporary test VM on an isolated network); primary keeps running
	JobVMStopTestFailover   = "VMStopTestFailover"   // params: vm — tear down a test failover
	JobVMPlannedFailover    = "VMPlannedFailover"    // params: vm, primaryHost, fromKind, fromName — zero-data-loss planned failover + reverse replication
	JobVMFailover           = "VMFailover"           // params: vm, recoveryPoint (optional), fromKind, fromName — unplanned failover after the primary is lost
	JobVMCancelFailover     = "VMCancelFailover"     // params: vm — cancel a test or unplanned failover (Stop-VMFailover)
	JobVMReverseReplication = "VMReverseReplication" // params: vm, fromKind, fromName — reverse replication so the new primary replicates back to the old primary
	JobVMRemoveReplica      = "VMRemoveReplica"      // params: vm — run on the replica host: remove the replica relationship and delete the orphaned replica copy (+ its VHDs)

	// Runbook orchestration. These run on the DR-side host, enqueued by the
	// centre's runbook controller rather than by an operator — but they are
	// ordinary jobs, visible in the activity feed and runnable on their own, so
	// there is no hidden channel the console cannot show.
	//
	// JobVMHealthProbe is ONE SHOT and short: it probes once and reports. A gate
	// that needs twenty minutes is twenty minutes of probes, not one job holding
	// an agent worker open — so a centre restart mid-gate resumes the gate, and a
	// guest that never boots does not tie up the host that has to run the rest of
	// the recovery.
	JobVMHealthProbe = "VMHealthProbe" // params: vm, check (Heartbeat|TCP|ICMP|Script), address (optional), port, script, expectExit — one probe, reports healthy or why not
	// JobEnsureTestSwitch creates the private, uplink-less switch a test failover
	// runs inside. It REFUSES if a switch of that name exists and is not private,
	// because reconfiguring somebody's external switch to isolate a test is how a
	// DR test takes production down.
	JobEnsureTestSwitch = "EnsureTestSwitch" // params: switch — ensure a private (isolated) virtual switch exists on this host
	JobRemoveTestSwitch = "RemoveTestSwitch" // params: switch — remove a private test switch; refuses if it is not private or still has VMs attached

	JobRemoveSwitch   = "RemoveSwitch"   // params: switch — delete a virtual switch from the host
	JobRemoveMgmtVNIC = "RemoveMgmtVNIC" // params: vnic — remove a management-OS vNIC from the host
	JobRemoveVM       = "RemoveVM"       // params: vm — stop and delete a VM from the host (hard delete)
	JobRemoveCSV      = "RemoveCSV"      // run on the former: params: volume — delete a Cluster Shared Volume from the S2D pool (destructive)
	JobRepairPool     = "RepairPool"     // run on a member: retire and remove unhealthy disks from the S2D pool so it returns to Healthy
	// JobRepairISCSIPortals re-registers discovery portals pinned to a source
	// address the host no longer has. A pinned portal returns nothing from
	// discovery, which then fails every login with "the target name is not
	// found" and points at the array, where nothing is wrong. The reconcile
	// detects and reports it but cannot fix it — fixing means REMOVING a portal
	// entry, and the iSCSI reconcile is strictly additive.
	JobRepairISCSIPortals = "RepairISCSIPortals" // params: none — re-register discovery portals bound to an address this host no longer has

	/* JobISCSIRediscover clears every discovery portal on a host so the reconcile
	   rebuilds them from the spec. params: none.

	   Windows keeps its own discovered-target database, and a stale entry makes
	   Connect-IscsiTarget refuse with "The target name is not found or is marked
	   as hidden from login" — a message that reads like the array refusing the
	   node and is not. Clearing the portals fixed exactly that on the rig on
	   2026-09-01, from iscsicpl, by hand, on the host: the defect CLAUDE.md names.

	   Wider than JobRepairISCSIPortals, which touches only portals bound to an
	   absent address because it runs inside the reconcile loop. Narrower than
	   JobResetISCSIInitiator, which drops sessions and persistent logins; this
	   drops neither, so the node keeps its disks while it runs. */
	JobISCSIRediscover = "ISCSIRediscover"
	// JobPruneISCSIPortals removes discovery portals the spec does not declare.
	// The reconcile is strictly additive — it cannot tell a portal an operator
	// retired from one something else on the host depends on — so a portal added
	// by mistake or left from an earlier configuration stays for ever, inflating
	// the reported path count and advertising targets nobody wants. Before this
	// the only ways out were iscsicpl on the host, or Reset initiator, which is
	// nuclear and refuses while disks carry partitions.
	//
	// The agent REFUSES to remove an undeclared portal that is carrying a live
	// session, and says so: that is either a spec missing a portal or a session
	// nobody declared, and both are for a person to resolve.
	// JobAdoptISCSIDisk adopts a LUN that already has contents, which the
	// reconcile refuses on purpose: formatting a disk with data on it is a
	// decision an operator makes once, about one disk, with the contents named to
	// them — not a field in a spec that re-applies on every pass.
	//
	// Two modes, and the difference is everything. "keep" brings the EXISTING
	// volume into the cluster untouched, which is what moving a CSV between
	// clusters needs. "wipe" formats first, for contents that are finished with.
	//
	// The refusal message named "Wipe and adopt" long before any such action
	// existed, so it sent operators looking for a button that was never built.
	JobAdoptISCSIDisk = "AdoptISCSIDisk" // params: cluster, volume, mode (keep|wipe)

	/* The VMware migration jobs. One pass each, never the whole migration.

	   A warm migration runs for hours and the centre may restart in the middle
	   of it, so the position lives in the Migration object and each job is one
	   short, resumable step. The alternative — one job that runs to completion —
	   would be a nine-hour job with no position anybody could read, and a centre
	   restart would lose all of it.

	   MigrationPass covers the base copy, every delta, and the final pass after
	   the source is stopped. They are one code path on purpose: a separate
	   "final pass" that had drifted from the delta path would be the one nobody
	   had run a hundred times. */
	JobMigrationEnableCBT = "MigrationEnableCBT" // params: migration, source creds (address/username/password/insecure), moRef
	JobMigrationPass      = "MigrationPass"      // params: as above plus destDir, markers (JSON key->changeId), final (true on cutover)
	// JobMigrationCleanup removes the VMware snapshot Ballast created. It runs
	// on the way out of EVERY terminal phase, failure included: a snapshot left
	// on somebody else's VM grows until their datastore fills, which is the
	// worst thing this feature could leave behind on a system it does not own.
	JobMigrationCleanup = "MigrationCleanup" // params: source creds, moRef, migration

	JobPruneISCSIPortals = "PruneISCSIPortals" // params: portals — comma-separated declared portals to keep

	/* JobClearISCSIFavourites removes stale persistent logins — the favourites in
	   iscsicpl — for the declared targets, leaving every session connected.

	   Ballast makes a session persistent by registering the existing one, and
	   that call fails against login state left by an earlier configuration. A
	   session made fresh by Connect-IscsiTarget never needs it. So a host
	   carrying leftovers never becomes persistent however many passes run: it
	   works until it reboots and then does not come back, and the only way out
	   was the iSCSI Initiator control panel on the host.

	   Nothing is disconnected. A persistent entry is what happens at BOOT, and
	   removing it does not touch what is connected now — measured on HVNEW01,
	   2026-09-06, where the session stayed up throughout and the next reconcile
	   registered it properly. The job reports the live session count back as
	   proof rather than asserting it.

	   Only DECLARED targets. An entry for something nobody declared may belong to
	   another product, and removing it would be Ballast reaching outside what it
	   was asked to manage. */
	JobClearISCSIFavourites = "ClearISCSIFavourites" // params: targets — comma-separated declared target IQNs

	/* JobTestJumboPath sends a don't-fragment ping at the declared payload from
	   each storage vNIC to the addresses that matter, and reports what crossed.

	   It exists because configuring MTU correctly on every host is not the same
	   as jumbo frames working, and nothing on a host can tell the difference.
	   Three things have to agree — the uplink, the IP interface, and the physical
	   switch between the hosts — and Ballast owns the first two. The third it has
	   no presence on and cannot read.

	   So the only way to establish the truth is to send a frame that size and see
	   whether it arrives. Don't-fragment is the whole point: without it the stack
	   silently splits an oversized packet and the test passes on a 1500 path.

	   The result names the culprit rather than reporting a failure. Both ends at
	   9000 with the ping failing is a switch that has not been configured, and
	   that is a sentence the console can print — the one step the operator has to
	   take somewhere Ballast does not run.

	   params: mtu (payload bytes; default 9000), targets (comma-separated
	   addresses; empty means the host tests every peer it can see from its own
	   storage subnets). */
	JobTestJumboPath = "TestJumboPath" // params: mtu, targets

	/* JobDisableLSO turns off Large Send Offload on named adapters.

	   The third thing that has to agree for jumbo frames, and the only one with
	   no configuration anywhere that shows it is wrong. LSO re-segments traffic
	   in the NIC, and on some drivers that defeats a 9000-byte MTU behind a
	   Hyper-V vSwitch: every layer reports 9000 and large frames do not arrive.

	   Found on the rig 2026-09-07 — HPE 631FLR-SFP28 on Server 2025 — where
	   jumbo started working only after LSO was disabled by hand on every host.
	   RSC was left ENABLED on those same hosts and jumbo works, so it is not
	   the blocker here and nothing offers to change it.

	   A job rather than desired state, deliberately. LSO earns its keep on a
	   1500 fabric and turning it off costs throughput, so it is a decision an
	   operator makes for a reason, not something a reconcile does on their
	   behalf because an MTU was declared.

	   params: adapters, switches — comma-separated */
	JobDisableLSO = "DisableLSO"

	// JobScanImportableVMs walks this host's storage for VM configurations no
	// host has registered and reports them in status.
	//
	// A job rather than an unconditional part of every pass, because the cost
	// scales with somebody else's data: Compare-VM has to load each
	// configuration, and a volume adopted from a busy cluster can hold hundreds.
	// The reconcile does the cheap half on its own — a directory walk that
	// notices configurations exist — and this job does the expensive half when
	// an operator is actually looking.
	JobScanImportableVMs = "ScanImportableVMs" // params: path — optional single volume to scan; empty scans every storage root

	// JobImportVM registers a VM whose files already sit on this host's storage.
	//
	// Two modes, and choosing wrong is expensive in opposite directions:
	//
	//   "register"  Import-VM -Register. The files stay exactly where they are
	//               and the host takes ownership of them. This is the right
	//               choice for a VM already on the CSV it will run from, which
	//               is every VM this feature exists for.
	//   "copy"      Import-VM -Copy -GenerateNewId. The files are DUPLICATED
	//               into the host's VM path and the copy gets a new identity.
	//               Correct for cloning a template; on a 500GB VM already on the
	//               right volume it silently writes 500GB to the same disk.
	//
	// The import REFUSES a VM whose ID is already registered anywhere Ballast
	// knows about, and one whose files are open. Two live claims on one set of
	// VHDXs corrupts them, and that is not a warning to click through.
	JobImportVM = "ImportVM" // params: path — the .vmcx; mode — register|copy; cluster — add a cluster role; applyFixes — resolve the resolvable Compare-VM findings; discardSavedState
	// JobDisconnectISCSITarget logs a host out of one target and clears its
	// persistent entry. The reconcile is additive and will never do this: it
	// cannot tell a target the operator retired from one something else on the
	// host still depends on, so retiring a login is a decision, not a cadence.
	// The agent refuses while the target's disks are clustered or online.
	JobDisconnectISCSITarget = "DisconnectISCSITarget" // params: target — log out of one iSCSI target and forget it

	// JobResetISCSIInitiator returns a host's iSCSI initiator to a clean slate —
	// every session, every persistent login, every discovery portal — so the
	// reconcile rebuilds it from the declared spec on its next pass.
	//
	// The reconcile is strictly additive and never prunes, so iSCSI state
	// accumulates: dead favourite targets a host retries once a minute for ever,
	// portals pinned to addresses it no longer has, disk objects for LUNs it
	// cannot reach. Starting again is a decision, not a cadence, so it is a job.
	// It REFUSES while any iSCSI disk is clustered or online.
	JobResetISCSIInitiator = "ResetISCSIInitiator" // no params — DESTRUCTIVE: clear all iSCSI sessions, persistent logins and portals
	JobRebuildPool         = "RebuildPool"         // run on a member: DESTRUCTIVE — destroy the S2D pool and its volumes, then re-enable S2D fresh (for a stale/degraded pool from a torn-down cluster)

	// JobDestroyS2D tears an S2D pool down for good — CSVs, virtual disks, the
	// pool, and Storage Spaces Direct itself — with an optional wipe of the
	// disks back to raw. Unlike RebuildPool it does NOT re-enable S2D; it is what
	// a cluster moving to an array needs.
	//
	// Without it, switching a cluster from S2D to iSCSI left the old pool behind
	// reporting Unknown / Read-only for ever, indistinguishable from a failed
	// one, with every disk still claimed — and the only way out was a PowerShell
	// session on a member. It refuses while any clustered VM role still exists.
	JobDestroyS2D = "DestroyS2D" // run on a member: DESTRUCTIVE — params: wipeDisks ("true" to return the disks to raw)

	JobFormatDisk      = "FormatDisk"      // params: deviceId — wipe a physical disk back to a poolable raw state (destructive)
	JobFormatDiskDrive = "FormatDiskDrive" // params: deviceId, driveLetter (optional — empty leaves the volume with no letter) — initialise, partition, format NTFS
	/* JobCreateVolumeInFreeSpace carves a volume out of a disk's UNALLOCATED
	   space, leaving every existing partition alone.

	   params: deviceId, driveLetter (required), sizeBytes (optional — 0 takes
	   the whole free extent), label (optional).

	   It is the one disk job that may run on the OS disk, and the reason is what
	   it does NOT do: no Clear-Disk, no Initialize-Disk, nothing that touches a
	   partition that already exists. It allocates space nothing has claimed. A
	   host with one 6TB disk carrying a 600GB Windows is otherwise a host with
	   no room for a VM, which is not true of the hardware.

	   Standalone hosts only. A pool takes whole disks, so there is no cluster
	   form of this. */
	JobCreateVolumeInFreeSpace = "CreateVolumeInFreeSpace"

	JobRepairHostDNS  = "RepairHostDNS"  // no params — point non-management NICs' DNS at the DC and stop them registering in DNS
	JobResetPoolDisks = "ResetPoolDisks" // no params — wipe local non-OS, non-pooled disks so S2D can claim them (adding a node's capacity)

	// JobReleasePoolDisks is the inverse of ResetPoolDisks: it releases disks a
	// storage pool still CLAIMS, on a host that is no longer a cluster member.
	// ResetPoolDisks deliberately skips pool members, so a torn-down cluster left
	// every data disk stuck "In a Pool" with no way back short of a PowerShell
	// session on the host. params: deviceId — one disk by unique id (or device id);
	// omit it to release every non-OS local disk. Destructive.
	JobReleasePoolDisks = "ReleasePoolDisks"

	JobRepairNetworkProfile = "RepairNetworkProfile" // no params — set any host NIC on the Public network profile to Private (Public breaks WinRM/clustering); Domain NICs are left as-is

	JobRebootHost = "RebootHost" // no params — restart the host now (Restart-Computer -Force)

	JobShutdownHost = "ShutdownHost" // no params — power the host off now (Stop-Computer -Force)

	JobEnableRDP = "EnableRDP" // no params — enable Remote Desktop (clear fDenyTSConnections, enable the RDP firewall group)

	JobClusterDestroy = "ClusterDestroy" // run on the former: remove VM roles, disable S2D, Remove-Cluster -CleanupAD (destructive)

	// JobRemoveReplicaBroker removes the Hyper-V Replica Broker cluster role and
	// its client access point (network name + IP). Run on the former. Clear
	// Cluster.Spec.ReplicaBroker first or the reconcile recreates it.
	JobRemoveReplicaBroker = "RemoveReplicaBroker" // run on the former: params: group (optional) — delete the Replica Broker role and its CAP (destructive)

	// JobResync forces an immediate full reconcile on the host. It does no work
	// itself: every completed job already triggers a nudge, which pulls desired
	// state in full (ignoring the generation short-circuit), re-collects the
	// throttled inventory/resource/VM observations and runs the cluster pass
	// regardless of its edge trigger. Read-only and safe to run at any time.
	JobResync = "Resync" // no params — force an immediate full reconcile on this host

	JobFetchISO = "FetchISO" // params: url, dest, name — download an ISO from the centre's library to dest (a CSV's ISOs folder), agent-local

	// JobGuestActivateAVMA installs an Automatic Virtual Machine Activation key
	// inside a guest so it activates against its Hyper-V host.
	//
	// A JOB rather than desired state, for the same reason the guest domain join
	// is one: it runs INSIDE the guest over PowerShell Direct, so it needs the VM
	// running and an administrator account within it. As desired state a VM that
	// is legitimately powered off would report unmet intent for as long as it
	// stayed off, which is not drift and not something to fix.
	JobGuestActivateAVMA = "GuestActivateAVMA" // params: vm, avmaKey, guestUser, guestPass

	JobGuestJoinDomain = "GuestJoinDomain" // params: vm, domain, ou, newName (optional), guestUser, guestPass, domainUser, domainPass — join the guest OS to the domain via PowerShell Direct, renaming it in the same reboot when newName is set (reboots the guest)
	JobGuestSetIP      = "GuestSetIP"      // params: vm, interface, address (CIDR), gateway, dns, guestUser, guestPass — set a static IP in the guest via PowerShell Direct

	// VM templates. Both are pure disk work: capture copies a VM's VHDX into the
	// library, deploy copies it back out and injects the guest's unattend. The
	// deploy deliberately does NOT create the VM — the centre authors the VM's
	// desired state when the job succeeds and the ordinary reconciler builds it,
	// so a deployed VM is indistinguishable from any other from then on.
	JobVMCaptureTemplate    = "VMCaptureTemplate"    // params: vm, dest, template, generalise, guestUser, guestPass — sysprep (optional) then copy the VM's first VHDX into the library
	JobVMDeployFromTemplate = "VMDeployFromTemplate" // params: source, dest, vm, unattend, vmspec — copy the template VHDX to dest and inject the unattend
)

// sensitiveJobParams are Job.Params keys whose values are credential material.
// They are stripped from every UI/REST view of a job and scrubbed from the
// stored job once it reaches a terminal state, so secrets do not linger at rest
// in the job history. The agent receives the real values over the gRPC job
// channel before the job completes, so scrubbing afterwards costs it nothing.
// "unattend" is here because a generated unattend.xml embeds the guest's local
// administrator password and the domain-join credential in the clear. It has to
// reach the agent to be written into the image, but it must not sit in the job
// history afterwards or appear in any view of the job.
var sensitiveJobParams = map[string]bool{
	"guestuser": true, "guestpass": true, "domainuser": true, "domainpass": true,
	"username": true, "password": true, "pass": true, "pw": true, "secret": true,
	"unattend": true,
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

	// SecretCHAPCredential carries an iSCSI CHAP username and secret, in the same
	// "username" + "password" keys.
	//
	// It is its own type rather than a domain credential because the two are
	// offered in different places and confusing them is a real hazard: a domain
	// credential reaches hosts over WinRM, and putting one forward as a CHAP
	// secret would send an account password to a storage array. The dialogs that
	// pick a credential filter by type, so the separation is what keeps that from
	// being one wrong selection away.
	SecretCHAPCredential = "CHAPCredential"

	// SecretVMwareCredential authenticates to vCenter or ESXi, in the same
	// "username" + "password" keys.
	//
	// Its own type for the reason CHAP has one: these credentials leave the
	// fabric. A vCenter SSO account offered in a picker beside domain
	// administrators is one wrong click from sending a domain password to
	// somebody else's management plane — and the reverse, a domain account sent
	// to vCenter, fails in a way that looks like Ballast being broken.
	//
	// It is also a different SHAPE of account in practice: vCenter wants an SSO
	// principal (administrator@vsphere.local), a standalone ESXi wants a local
	// one (root). Neither authenticates against the other, which the connect
	// error says explicitly.
	SecretVMwareCredential = "VMwareCredential"

	/* SecretBMCCredential authenticates to a host's management controller —
	   iLO, iDRAC, XCC — in the same "username" + "password" keys.

	   Its own type for the reason CHAP and vCenter have one: it leaves the
	   fabric, and it is the most privileged credential in the building. A BMC
	   account offered in a picker beside domain administrators is one wrong
	   click from sending a domain password to a device on the management
	   network — and a controller account has power over the machine itself, not
	   merely over Windows on it.

	   It is also a different SHAPE of account: local to the controller
	   (Administrator on iLO, root on iDRAC), authenticating against nothing
	   else. */
	SecretBMCCredential = "BMCCredential"

	// SecretLocalCredential is a LOCAL administrator on a host, in the same
	// "username" + "password" keys.
	//
	// It exists because a host is reachable by a domain account only once it is
	// domain-joined, and Ballast's job includes onboarding hosts that are not yet.
	// With only a domain type on offer, a workgroup host could not be given any
	// valid credential at all — so the operator had to join it to the domain by
	// hand before Ballast would touch it, which is the step Ballast performs.
	//
	// It is a separate type rather than a flag on the domain one because the
	// pickers filter by type, and that filtering is what stops a guest account
	// being offered as a WinRM target.
	SecretLocalCredential = "LocalCredential"

	// SecretProductKey is a Windows product key, in the key "productKey".
	//
	// Its own type because it is not a credential and the pickers filter by type:
	// offering a domain account where a product key belongs, or the reverse, is a
	// mistake one wrong selection away. A product key also differs from every
	// other secret here in that it CANNOT be rotated once used — which is why it
	// is a stored reference rather than a value in a spec.
	SecretProductKey = "ProductKey"
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
	// Role governs what the user may do: RoleAdmin (everything), RoleOperator
	// (day-to-day fabric and VM operations, but not accounts, centre settings,
	// or the credential vault), or RoleReadOnly (view only).
	Role string `json:"role"`
	// Source is UserSourceLocal for built-in accounts; AD/LDAP users (a later
	// phase) will carry a different source.
	Source    string    `json:"source"`
	UpdatedAt time.Time `json:"updatedAt"`
}

const (
	RoleAdmin       = "admin"
	RoleOperator    = "operator"
	RoleReadOnly    = "readonly"
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

/*
VMIntegrationServices is the six Hyper-V guest services, each tri-state.

	A nil field is UNMANAGED and left exactly as it is. The distinction matters
	more here than in most places: these have real consequences and different
	Hyper-V defaults, so a missing value must never be read as "off". Shutdown
	off means a host drain has to hard-stop the guest; VSS off means no
	application-consistent backup; Time Synchronization on is wrong for a domain
	controller and right for nearly everything else.

	Named as Hyper-V names them, so what is ticked here and what Get-VMIntegrationService
	prints are recognisably the same thing.
*/
type VMIntegrationServices struct {
	// GuestServiceInterface is file copy into the guest. Ships DISABLED.
	GuestServiceInterface *bool `json:"guestServiceInterface,omitempty"`
	// Heartbeat is the liveness signal the console's health column reads.
	Heartbeat *bool `json:"heartbeat,omitempty"`
	// KeyValuePairExchange carries guest facts back to the host — the guest's
	// IP addresses and OS build reach Ballast through it.
	KeyValuePairExchange *bool `json:"keyValuePairExchange,omitempty"`
	// Shutdown is what lets a host drain stop a guest gracefully. Off means the
	// only remaining option is to pull the power.
	Shutdown *bool `json:"shutdown,omitempty"`
	// TimeSynchronisation syncs the guest clock to the host. Correct for nearly
	// everything and wrong for a domain controller, which must hold the
	// authoritative time itself.
	TimeSynchronisation *bool `json:"timeSynchronisation,omitempty"`
	// VSS is the volume shadow copy service: what makes an application-
	// consistent backup possible rather than a crash-consistent one.
	VSS *bool `json:"vss,omitempty"`
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

	// SecureBoot sets the Gen 2 UEFI Secure Boot policy. "" or "windows" uses the
	// default Microsoft Windows template; "linux" uses the Microsoft UEFI CA
	// template — required for most Linux guests, whose shim is signed under it, so
	// the Windows template rejects them ("the signed image's hash is not allowed").
	// "off" disables Secure Boot. Ignored for Gen 1. A firmware change needs the VM
	// stopped, so it settles on the next power-off (like processor/memory).
	SecureBoot string `json:"secureBoot,omitempty"`

	/* VideoResolution is the console resolution the VM's synthetic video adapter
	   presents, as "1920x1080". Empty leaves it as Hyper-V set it, which is
	   1024x768 and is why this field exists.

	   It is a property of the VM, not of the viewer. A basic console session
	   shows exactly what the guest's video adapter is driving, so asking the
	   browser for a bigger window scales a 1024x768 picture up rather than
	   showing more of anything — the only thing that changes it is the adapter.

	   Applied with Set-VMVideo, which requires the VM stopped, so like Secure
	   Boot and processor count it settles on the next power-off and reports
	   pending until then. Enhanced-session guests (Windows with integration
	   services) negotiate their own resolution and are unaffected.
	*/
	VideoResolution string `json:"videoResolution,omitempty"`

	// ComputerName is the guest OS hostname this VM should have. It is NOT the
	// VM's name: the Hyper-V object and the operating system inside it are named
	// separately, they drift the moment anyone renames either, and the one that
	// appears in DNS, in AD and in a support call is this one. Mirrors
	// HostSpec.ComputerName.
	//
	// Applied where a name can be set without disrupting a running workload: the
	// unattend at deploy (before the guest can register a wrong name anywhere)
	// and the domain-join job, which renames and joins in a single reboot.
	// Unlike a host, a running guest is NOT renamed to match — that is a reboot
	// of somebody's workload, and there is no guest RebootPolicy to govern it.
	// A difference is reported as drift and renamed by an explicit job.
	//
	// Empty means the name is unmanaged and no drift is reported.
	ComputerName string `json:"computerName,omitempty"`

	// BootOrder is the firmware boot priority, most-preferred first, expressed as
	// device categories rather than specific devices so it stays stable as disks
	// and NICs change: "Drive" (a virtual hard disk), "DVD" (an ISO/optical drive),
	// "Network" (PXE), and, for Generation 1 only, "Floppy". The agent orders the
	// VM's actual boot entries to match — a Gen 2 UEFI BootOrder or a Gen 1 BIOS
	// StartupOrder — and appends any device categories not listed. Empty means the
	// boot order is unmanaged (Hyper-V's default is left untouched). Like a
	// firmware change, it settles on the next power-off.
	BootOrder []string `json:"bootOrder,omitempty"`

	// ProcessorCount is the number of virtual processors assigned.
	ProcessorCount int `json:"processorCount"`

	// MemoryStartupBytes is the startup memory. With DynamicMemory unset this is
	// also the fixed assignment.
	MemoryStartupBytes uint64 `json:"memoryStartupBytes"`

	// DynamicMemory, when set, lets the VM's memory float between Min and Max.
	// Nil means a fixed assignment of MemoryStartupBytes. Cannot be combined
	// with NestedVirtualisation: Hyper-V refuses to start a nested-enabled VM
	// whose memory is dynamic.
	DynamicMemory *DynamicMemorySpec `json:"dynamicMemory,omitempty"`

	/* NestedVirtualisation exposes the host's virtualisation extensions to the
	   guest, so the guest can itself run Hyper-V.

	   ONE field for two settings, deliberately. It is Set-VMProcessor
	   -ExposeVirtualizationExtensions AND MAC address spoofing on every one of
	   the VM's adapters, because a nested guest's inner VMs have MAC addresses
	   the outer switch has never seen and the switch drops their traffic
	   otherwise. Split into two knobs, the reachable state "nested on, spoofing
	   off" is a guest that boots, runs VMs, and has no network — worse than
	   either setting alone, and diagnosed by nobody.

	   Extensions are exposed at power-on, so like the processor count and Secure
	   Boot this settles on the next power-off and reports pending until then.
	   Ballast does not restart somebody's VM to satisfy a checkbox. Spoofing is
	   a live setting and applies immediately.

	   The host must support it (Intel VT-x/EPT or AMD-V on Hyper-V 2016 or
	   later), and a nested guest cannot be live-migrated. */
	NestedVirtualisation bool `json:"nestedVirtualisation,omitempty"`

	/* TPM gives the VM a software-emulated TPM 2.0, which Windows 11 requires to
	   install and which BitLocker binds to.

	   Generation 2 only. A Gen 1 VM has no UEFI firmware to present one, so the
	   setting is refused there rather than accepted and silently ignored — a
	   checkbox that stays ticked and does nothing is how somebody spends an
	   afternoon on a Windows 11 installer that will not proceed.

	   IT NEEDS MORE THAN Enable-VMTPM. A vTPM is sealed to key protectors, and a
	   VM with no key protector cannot have one enabled: Hyper-V refuses with "A
	   key protector cannot be found". So the reconcile creates a local key
	   protector first when the VM has none. That is the step every runbook on
	   this subject spells out and no console does for you.

	   The consequence of the sealing is worth stating because it is not
	   reversible by wishing: the protector belongs to THIS host's guardian. A VM
	   with a vTPM cannot simply be exported and imported elsewhere — the
	   destination cannot unseal it — so moving one needs the key protector
	   carrying too, or the guest's BitLocker recovery key to hand. Ballast says
	   so rather than letting an operator discover it during a migration.

	   Settled at power-off like Secure Boot and the extensions: adding a TPM to a
	   running VM is refused by Hyper-V, and Ballast does not restart somebody's
	   VM to satisfy a checkbox. It reports pending until then. */
	TPM bool `json:"tpm,omitempty"`

	// Disks are the virtual hard disks attached to the VM, in attachment order.
	Disks []VMDiskSpec `json:"disks,omitempty"`

	// NetworkAdapters are the VM's vNICs, each bound to a named vSwitch that is
	// expected to exist on the placement host.
	NetworkAdapters []VMNetworkAdapterSpec `json:"networkAdapters,omitempty"`

	// ISOPath, when set, attaches a DVD drive backed by this ISO so the VM can
	// boot from it. Empty means no boot media (or one already attached is left
	// as-is). For a Generation 2 VM the agent also makes the DVD a boot entry.
	/* IntegrationServices are the Hyper-V guest services to enable or disable.

	   Nil means unmanaged, which is why each is a pointer. Hyper-V's own
	   defaults differ per service — Guest Service Interface ships off, the rest
	   ship on — and a struct of plain bools would read as "turn all of these
	   off" for every VM that had never been asked about, silently disabling
	   backup and shutdown across a fleet on the first save. Declare or leave
	   alone, the same rule as RDMA and MTU.

	   Worth managing rather than leaving to whoever built the VM: Shutdown is
	   what makes a graceful host drain work, VSS is what makes a consistent
	   backup possible, and both are per-VM settings that nothing in the console
	   could previously see or set. */
	IntegrationServices *VMIntegrationServices `json:"integrationServices,omitempty"`

	ISOPath string `json:"isoPath,omitempty"`

	// DesiredPowerState is the power state the agent should drive the VM to.
	DesiredPowerState VMPowerState `json:"desiredPowerState"`

	// AutomaticStartAction governs what the host does with the VM when the host
	// itself boots. Empty defaults to the Hyper-V default (StartIfRunning).
	AutomaticStartAction VMStartAction `json:"automaticStartAction,omitempty"`

	// Replication, when set, replicates this VM with Hyper-V Replica to a
	// standalone host or another cluster. The owning agent enables and keeps
	// the replication relationship; the target side must be a configured
	// replica server (HostSpec.ReplicaServer / ClusterSpec.ReplicaBroker —
	// the UI authors both sides together). Enabled=false with the spec present
	// removes an existing relationship.
	Replication *VMReplicationSpec `json:"replication,omitempty"`
}

// VMReplicationSpec declares Hyper-V Replica for one VM.
type VMReplicationSpec struct {
	Enabled bool `json:"enabled"`

	// TargetHost / TargetCluster record the operator's intent (one of the two)
	// for display and validation. TargetCluster means the replica lands as a
	// clustered replica behind that cluster's broker.
	TargetHost    string `json:"targetHost,omitempty"`
	TargetCluster string `json:"targetCluster,omitempty"`

	// ReplicaServer is the address replication actually points at: the
	// standalone host's FQDN, or the broker's client access point FQDN for a
	// cluster target. Authored by the UI when the spec is written.
	ReplicaServer string `json:"replicaServer,omitempty"`

	// FrequencySeconds is the replication interval: 30, 300 or 900. Zero
	// defaults to 300.
	FrequencySeconds int `json:"frequencySeconds,omitempty"`

	// AuthenticationType is Kerberos or Certificate; empty defaults to
	// Kerberos. Port zero defaults to 80 (Kerberos) / 443 (Certificate).
	AuthenticationType string `json:"authenticationType,omitempty"`
	Port               int    `json:"port,omitempty"`
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

	// VMPowerRoleOffline is a CLUSTERED VM whose role is offline. It is not a
	// Hyper-V power state and no agent reports it — the centre derives it from
	// the cluster, which is the only thing that knows.
	//
	// A clustered VM's registration IS a cluster resource: with the role offline
	// the VM is deregistered from Hyper-V on every node, so no agent can see it
	// and none can report its power. That absence used to leave the last observed
	// value standing, and a VM stopped hours ago went on reading "Running" while
	// its role sat Offline in the same database.
	//
	// It is deliberately not "Off". The VM is usually SAVED behind an offline
	// role (AutomaticStopAction defaults to Save), so the true power state is
	// genuinely unknown; what IS known is that the role is down and the VM is not
	// running. Saying "Off" would assert the part nobody can see.
	VMPowerRoleOffline VMPowerState = "Offline"
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

	// PowerObservedAt is when an agent last actually OBSERVED this VM's power,
	// stamped by the centre when a report arrives carrying a non-empty
	// PowerState. A report that carries an empty one did not observe it — the VM
	// was unreadable, or deregistered behind an offline cluster role — and does
	// not move this stamp, even though it does move LastReportedAt.
	//
	// The two are different questions. LastReportedAt answers "is an agent still
	// enforcing this VM"; this answers "how old is the power state being shown".
	// Deriving power from the cluster needs the second one: a cluster snapshot
	// older than a real observation must not overwrite it, or a running VM
	// flickers to "role offline" every time a stale sweep lands.
	PowerObservedAt time.Time `json:"powerObservedAt,omitempty"`

	// ReportedBy is the host whose agent last reported this VM, stamped by the
	// CENTRE from the report's own identity. Not on the wire and not settable by
	// an agent, for the same reason as ClusterStatus.ObservedAt: one authority.
	//
	// It is the fastest correct answer to "which node is this VM actually on".
	// A clustered VM is REGISTERED IN HYPER-V ONLY ON ITS OWNER — the role owns
	// the registration, and a non-owner cannot Get-VM it at all — so the agent
	// that can report it is the owner, by construction. The alternatives lag: a
	// clustered VM's Placement names the cluster rather than a node, the
	// cluster's own role list refreshes on the slow sweep, and a host's observed
	// VM inventory is throttled to every few cycles. All three are behind a
	// report that already carries the VMID the console needs.
	//
	// It is OBSERVED, not desired. After a live migration the new owner reports
	// next and this follows it; placement may still say something older.
	ReportedBy string `json:"reportedBy,omitempty"`

	// PowerState is the actual observed power state of the VM.
	PowerState VMPowerState `json:"powerState,omitempty"`

	// LastReportedAt is when an agent last reported this VM's status. Stamped
	// by the centre on receipt (never by agents, so it is not on the proto,
	// like the other centre-only metadata). Zero for a VM no agent has
	// reported since the centre gained stamping. It is what lets the centre
	// notice a VM that no agent is enforcing — the dangerous quiet failure
	// where the last reported status stays green forever.
	LastReportedAt time.Time `json:"lastReportedAt,omitempty"`

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

	// MemoryDemandBytes is what the guest is actually asking for. Assigned memory
	// equals startup memory on a static-memory VM, so "assigned vs configured" is
	// 100% by definition and measures nothing — demand is the figure that shows
	// real usage, and Hyper-V reports it for static and dynamic VMs alike.
	// Zero when the VM is off or its integration services are not reporting, in
	// which case usage is unknown and must not be shown as zero.
	MemoryDemandBytes uint64 `json:"memoryDemandBytes,omitempty"`

	// MemoryStatus is Hyper-V's own verdict on the VM's memory pressure:
	// "OK", "Low" (the guest wants more than it has) or "Warning".
	MemoryStatus string `json:"memoryStatus,omitempty"`

	// Heartbeat is the integration layer's own verdict on the guest, straight from
	// Hyper-V: "OkApplicationsHealthy" and "OkApplicationsUnknown" both mean the
	// heartbeat is arriving (the second simply means nothing inside is publishing
	// application health, which is the normal case for a server); "Lost",
	// "NoContact" and "Error" mean it is not.
	//
	// It exists because "the VM says Running" answers a question nobody was
	// asking. A guest can be Running, reporting memory demand, and completely
	// unreachable at its console — and without this the console has no way to say
	// whether the integration layer is healthy, so an operator is left inferring it
	// from indirect signals. Empty when the VM is off or has no integration
	// services, which is unknown, not a fault.
	Heartbeat string `json:"heartbeat,omitempty"`

	// ScreenBlank reports that the captured console thumbnail is entirely one
	// colour — in practice, that Windows has powered the display down on its
	// inactivity timer.
	//
	// It says the DISPLAY IS BLANK and nothing more. A blanked display looks
	// identical whether the session beneath it is signed in, locked, or at the
	// logon screen, so anything that claims to know which is inventing it. Waking
	// the console is what distinguishes them, and that is an operator's action to
	// take, never a background one.
	ScreenBlank bool `json:"screenBlank,omitempty"`

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

	// Replication is the VM's observed Hyper-V Replica state, present when the
	// VM has a replication relationship (as primary or replica).
	Replication *VMReplicationStatus `json:"replication,omitempty"`

	Conditions []Condition `json:"conditions,omitempty"`
}

// VMReplicationStatus is a VM's observed Hyper-V Replica state.
type VMReplicationStatus struct {
	// Mode is Primary or Replica (this copy's role in the relationship).
	Mode string `json:"mode,omitempty"`
	// State is the replication state (e.g. Replicating, InitialReplication,
	// Suspended, Error, ReadyForInitialReplication).
	State string `json:"state,omitempty"`
	// Health is Normal, Warning or Critical.
	Health string `json:"health,omitempty"`
	// PrimaryServer / ReplicaServer are the two ends as Hyper-V reports them.
	PrimaryServer string `json:"primaryServer,omitempty"`
	ReplicaServer string `json:"replicaServer,omitempty"`
	// LastReplicationTime is when the last replica cycle completed (RFC3339);
	// empty before initial replication finishes.
	LastReplicationTime string `json:"lastReplicationTime,omitempty"`
	// FrequencySeconds is the configured interval.
	FrequencySeconds int `json:"frequencySeconds,omitempty"`
}

// VMObserved is a VM's actual configuration as read from the host, used to show
// and adopt VMs created outside Ballast. Disks reuse VMDiskSpec with SizeBytes
// left zero so an adopt attaches the existing VHDX rather than recreating it.
// VMIntegrationServiceState is one guest service as the host reports it, named
// exactly as Hyper-V names it so the console and Get-VMIntegrationService are
// recognisably describing the same thing.
type VMIntegrationServiceState struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type VMObserved struct {
	ProcessorCount     int                    `json:"processorCount,omitempty"`
	MemoryStartupBytes uint64                 `json:"memoryStartupBytes,omitempty"`
	DynamicMemory      bool                   `json:"dynamicMemory,omitempty"`
	MinBytes           uint64                 `json:"minBytes,omitempty"`
	MaxBytes           uint64                 `json:"maxBytes,omitempty"`
	Generation         int                    `json:"generation,omitempty"`
	Disks              []VMDiskSpec           `json:"disks,omitempty"`
	NetworkAdapters    []VMNetworkAdapterSpec `json:"networkAdapters,omitempty"`
	/* IntegrationServices is what the guest services are ACTUALLY set to, as
	   the host reports them.

	   Observed rather than declared, and both are needed. Nearly every VM
	   declares none of them — that is the correct default — so a console
	   showing only the declaration says "left alone" six times and tells an
	   operator nothing about the machine in front of them. Whether Shutdown is
	   on right now is the question they came to answer.

	   Empty means NOT READ YET, which is not the same as "all off": a VM the
	   agent has not looked at must never be drawn as one with every service
	   disabled. */
	IntegrationServices []VMIntegrationServiceState `json:"integrationServices,omitempty"`
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

// ---------------------------------------------------------------------------
// VM templates
// ---------------------------------------------------------------------------

// VMTemplate is a reusable VM definition: a hardware profile, a guest
// customisation profile, and a generalised (sysprepped) VHDX held in the
// library.
//
// A template is deliberately NOT desired state. Nothing reconciles towards one,
// no agent is ever given one, and it has no Generation/ObservedGeneration
// relationship — deploying from a template AUTHORS a VM's desired state, and
// from that moment the ordinary per-VM reconciler owns the result. So this is
// centre-only metadata in the same category as Site and Dvport: it needs no
// proto and never crosses the agent wire, and deleting a template afterwards
// takes nothing with it.
type VMTemplate struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// SourceDiskPath is the generalised VHDX this template deploys from, and
	// SourceHost / SourceCluster say who can read it: a host-local volume is
	// readable only by that host, while a CSV path is readable by any member of
	// the cluster (which is what makes a template usable across a cluster).
	SourceDiskPath string `json:"sourceDiskPath"`
	SourceHost     string `json:"sourceHost,omitempty"`
	SourceCluster  string `json:"sourceCluster,omitempty"`

	// DiskSizeBytes is the captured VHDX's size on disk, observed at capture. It
	// is what a deploy needs to check free space against; zero means unknown.
	DiskSizeBytes uint64 `json:"diskSizeBytes,omitempty"`

	// Generalised records whether sysprep /generalize ran during capture. False
	// means the image still carries a machine identity (SID, computer name,
	// domain membership), so every VM deployed from it is a duplicate of the
	// original — legitimate when the operator generalised it themselves or the
	// image is a Linux golden disk, and worth warning about otherwise.
	Generalised bool `json:"generalised,omitempty"`

	// Hardware profile. These mirror the VMSpec fields a template fixes; a
	// deploy may override the sizing ones per VM.
	HyperVGeneration   int                    `json:"hyperVGeneration,omitempty"`
	SecureBoot         string                 `json:"secureBoot,omitempty"`
	ProcessorCount     int                    `json:"processorCount,omitempty"`
	MemoryStartupBytes uint64                 `json:"memoryStartupBytes,omitempty"`
	DynamicMemory      *DynamicMemorySpec     `json:"dynamicMemory,omitempty"`
	BootOrder          []string               `json:"bootOrder,omitempty"`
	NetworkAdapters    []VMNetworkAdapterSpec `json:"networkAdapters,omitempty"`

	// AdditionalDisks are blank data disks created alongside the deployed copy
	// of the template image. SizeBytes must be set — a zero-sized entry means
	// "attach an existing VHDX", which has no meaning for a new VM.
	AdditionalDisks []VMDiskSpec `json:"additionalDisks,omitempty"`

	// Guest customises the guest OS on first boot. Nil leaves the image exactly
	// as captured.
	Guest *GuestProfileSpec `json:"guest,omitempty"`

	CreatedAt time.Time `json:"createdAt,omitempty"`
	CreatedBy string    `json:"createdBy,omitempty"`

	Status VMTemplateStatus `json:"status,omitempty"`
}

// Template phases. A capture copies a multi-gigabyte VHDX and can run for many
// minutes, so a template exists in the library — visibly unfinished — for the
// whole of it, rather than appearing only once the copy lands.
const (
	TemplateCapturing = "Capturing"
	TemplateReady     = "Ready"
	TemplateFailed    = "Failed"
)

// VMTemplateStatus tracks a capture through to a usable image. Only Ready
// templates can be deployed.
type VMTemplateStatus struct {
	Phase string `json:"phase,omitempty"`

	// Message explains a Failed capture, or what a Capturing one is doing.
	Message string `json:"message,omitempty"`

	// JobID is the capture job, so the console can link the template to its
	// progress and the operator can read the failure where it happened.
	JobID string `json:"jobId,omitempty"`

	CapturedAt time.Time `json:"capturedAt,omitempty"`
}

// Deployable reports whether a template can be deployed from.
func (t VMTemplate) Deployable() bool {
	return t.Status.Phase == TemplateReady && t.SourceDiskPath != ""
}

// A capture has one fact to hand back that the centre stores rather than
// displays — the size of the image it produced. A job reports its outcome as a
// human-readable message and nothing else, so the size travels inside that
// message, and these two functions are the only place its shape is defined. A
// message the parse does not recognise yields zero, which the centre records as
// "size unknown"; it never guesses.
const capturedBytesMarker = " bytes)"

// FormatCaptureResult builds a capture job's success message. It reads as prose
// and parses exactly.
func FormatCaptureResult(template, dest string, sizeBytes uint64) string {
	return fmt.Sprintf("captured %s to %s (%d%s", template, dest, sizeBytes, capturedBytesMarker)
}

// ParseCapturedBytes recovers the image size from a capture job's message,
// returning 0 when the message does not carry one.
func ParseCapturedBytes(message string) uint64 {
	end := strings.LastIndex(message, capturedBytesMarker)
	if end < 0 {
		return 0
	}
	start := strings.LastIndex(message[:end], "(")
	if start < 0 {
		return 0
	}
	n, err := strconv.ParseUint(message[start+1:end], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// GuestProfileSpec is how a deployed guest customises itself on first boot.
//
// For a Windows guest this becomes an unattend.xml injected into the copied
// VHDX before the VM is ever created, so the specialise pass consumes it before
// the guest finishes booting. That matters: it needs no guest credentials (a
// generalised image has no account yet), no integration services, no network,
// and it sets the computer name before the machine can register a wrong one in
// DNS. The existing PowerShell Direct jobs (GuestJoinDomain, GuestSetIP) remain
// the right tool for day-2 changes to a VM that is already running; they are
// complements, not alternatives.
//
// The profile itself holds no secrets — it NAMES vault secrets, which the
// centre resolves only at deploy, when it generates the unattend.
type GuestProfileSpec struct {
	// OSFamily selects the customisation mechanism. "windows" generates an
	// unattend.xml. Empty means the guest is left exactly as the image has it.
	// Linux (cloud-init via a seed ISO) is a separate mechanism, not a variation
	// on this one, and is not implemented.
	OSFamily string `json:"osFamily,omitempty"`

	// TimeZone is a Windows time-zone id ("New Zealand Standard Time").
	TimeZone string `json:"timeZone,omitempty"`

	// Locale is a BCP-47 tag ("en-NZ") used for the UI language, input locale
	// and regional settings.
	Locale string `json:"locale,omitempty"`

	OrganisationName string `json:"organisationName,omitempty"`

	// AdminPasswordSecret names a vault secret whose "password" value becomes
	// the guest's local Administrator password. Empty leaves the account
	// disabled, which means nobody can log in until the guest is domain-joined
	// — legitimate, but rarely what is wanted.
	AdminPasswordSecret string `json:"adminPasswordSecret,omitempty"`

	// ProductKeySecret names a vault secret holding a "key" value. Empty relies
	// on KMS/AVMA activation, which is the norm on a licensed Hyper-V host.
	ProductKeySecret string `json:"productKeySecret,omitempty"`

	// DomainJoin joins the guest during the specialise pass — before first
	// logon, in the same reboot, rather than joining afterwards.
	DomainJoin *GuestDomainJoinSpec `json:"domainJoin,omitempty"`

	// Workgroup names the workgroup to join when DomainJoin is nil.
	Workgroup string `json:"workgroup,omitempty"`

	// RunOnce are commands run at first logon, in order.
	RunOnce []string `json:"runOnce,omitempty"`
}

// GuestDomainJoinSpec joins a deployed guest to Active Directory as part of its
// first boot. It mirrors HostSpec's DomainJoin: the credential is a vault secret
// name, never inline.
type GuestDomainJoinSpec struct {
	Domain string `json:"domain"`

	// OUPath is the LDAP DN of the OU to place the computer object in. Empty
	// uses the domain's default computers container.
	OUPath string `json:"ouPath,omitempty"`

	// CredentialSecret names a vault secret with "username" and "password"
	// values for an account permitted to join computers to the domain.
	CredentialSecret string `json:"credentialSecret,omitempty"`
}

// ---------------------------------------------------------------------------
// Cluster-aware updating
// ---------------------------------------------------------------------------

// ClusterUpdateRun is one pass over a cluster, rebooting the nodes that need it
// one at a time and keeping the cluster serving throughout.
//
// It is an ORCHESTRATOR, not an installer. Something else applies the patches —
// WSUS, Intune, Group Policy, a person — and the agent already sees the result:
// its role-state script reads both pending-reboot registry flags, including
// WindowsUpdate\Auto Update\RebootRequired, every cycle. Installing is the easy
// half. The half that can take a cluster down is the reboot: drain the roles,
// wait for them to actually move, reboot, wait for the node AND ITS STORAGE to
// come back, resume, verify, only then the next node.
//
// Like Site, Dvport and VMTemplate this is centre-only: no agent is ever given
// one, nothing reconciles towards it, and it needs no proto. What it drives is
// ordinary desired state — HostSpec.Maintenance — so the agent's behaviour is
// the same as an operator draining a node by hand.
//
// A run is durable because it outlives a centre restart. The sequencing is
// centre-side, so a centre outage PAUSES a run; it does not strand the cluster
// (maintenance is desired state and the agent keeps honouring it), but the node
// mid-run stays drained until the centre returns.
type ClusterUpdateRun struct {
	// ID is assigned by the centre. A cluster may have many runs over time and
	// the history is worth keeping, so runs are not keyed by cluster name.
	ID          string `json:"id"`
	ClusterName string `json:"clusterName"`

	State   string `json:"state"`
	Message string `json:"message,omitempty"`

	// Nodes is the plan, in the order it will be walked, each carrying its own
	// state. One node at a time: the list is the operator's view of where the
	// run got to and what it is waiting on.
	Nodes []ClusterUpdateNode `json:"nodes,omitempty"`

	// ForceRebootAll reboots every member even where Windows reports no reboot
	// pending — for firmware or driver work, where the reason to reboot is not
	// something Windows knows about.
	ForceRebootAll bool `json:"forceRebootAll,omitempty"`

	// A run does NOT consult HostSpec.RebootPolicy, and that is deliberate.
	//
	// RebootPolicy governs whether the AGENT reboots on its own to honour desired
	// state: RebootNever means "surface the requirement and wait for an operator"
	// (see reconcileHostRole). A run is that operator. Treating Never as "skip
	// this node" would make a run do nothing at all on the fleets most likely to
	// be set that way, which is the opposite of what starting one asks for — and
	// it is the same reason the RebootHost job does not consult the policy
	// either.
	//
	// To leave a node out, leave it out of the run. Repurposing a field about
	// agent autonomy to mean "exclude from maintenance" would conflate two
	// different questions and confuse both.

	// LastObserveNudge is when the run last asked the cluster's former member to
	// re-observe, so it can do so periodically without doing it every tick.
	//
	// The health gate reads cluster status, which is refreshed on the former's
	// slow sweep — minutes. Waiting for that means a node sits Resuming long
	// after its storage has actually come back, and it compounds once per node.
	// Asking makes the run wait on the cluster rather than on a cadence.
	LastObserveNudge time.Time `json:"lastObserveNudge,omitempty"`

	// LastDisturbance is when this run last did something that changes what the
	// cluster looks like — drained a node, rebooted one, or returned one to
	// service. Cluster status observed before it describes a cluster that no
	// longer exists, so every health gate requires a snapshot newer than this.
	//
	// Without it the run reasons about health from whatever report happened to be
	// in the store. On the rig a second run began 13 minutes after the first
	// finished and passed preflight on a snapshot predating the reboots, so it
	// started draining into a pool that was still rebuilding. The numbers it read
	// were true; they were just true of an earlier cluster.
	LastDisturbance time.Time `json:"lastDisturbance,omitempty"`

	CreatedBy string    `json:"createdBy,omitempty"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
}

// Run states.
const (
	// ClusterUpdatePending is authored but not yet picked up by the controller.
	ClusterUpdatePending = "Pending"
	ClusterUpdateRunning = "Running"
	// ClusterUpdatePaused is a run that stopped because something was not safe
	// to proceed through — a degraded pool, a member offline, a step that timed
	// out. It holds rather than skipping ahead, and Message says why.
	ClusterUpdatePaused    = "Paused"
	ClusterUpdateSucceeded = "Succeeded"
	ClusterUpdateFailed    = "Failed"
	ClusterUpdateCancelled = "Cancelled"
)

// ClusterUpdateNode is one node's place in a run.
type ClusterUpdateNode struct {
	Name  string `json:"name"`
	State string `json:"state"`

	// Message says what this node is waiting on, or why it was skipped or
	// failed — in the operator's terms, not the controller's.
	Message string `json:"message,omitempty"`

	// RebootJobID links to the reboot this node was given, so the console can
	// show it where it happened.
	RebootJobID string `json:"rebootJobId,omitempty"`

	// RebootAt is when the reboot was asked for. It is how the controller knows
	// the machine actually went down: once the host reports an uptime SHORTER
	// than the time since this, it must have restarted.
	//
	// The alternative — watching for the host to go offline — depends on catching
	// a window (StaleAfter is 90s) that a quick reboot could slip through, and a
	// missed window would leave the run waiting for a restart that already
	// happened. Uptime is a fact the host reports rather than an absence the
	// centre has to notice.
	RebootAt time.Time `json:"rebootAt,omitempty"`

	StartedAt time.Time `json:"startedAt,omitempty"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
}

// Per-node states, in the order a node passes through them.
const (
	ClusterUpdateNodePending = "Pending"
	// ClusterUpdateNodeSkipped is a node that needed nothing: no reboot pending,
	// or a RebootNever policy the run was not told to override.
	ClusterUpdateNodeSkipped = "Skipped"
	// ClusterUpdateNodeDraining covers declaring maintenance and waiting for the
	// roles to actually leave. Paused is not drained.
	ClusterUpdateNodeDraining  = "Draining"
	ClusterUpdateNodeRebooting = "Rebooting"
	// ClusterUpdateNodeRejoining is the node coming back: online, Ready, and no
	// longer reporting a pending reboot.
	ClusterUpdateNodeRejoining = "Rejoining"
	// ClusterUpdateNodeResuming returns it to service and waits for the cluster
	// AND THE POOL to be healthy again. This is the gate that matters: a node can
	// report Up while its disks are still out of the S2D pool, and draining the
	// next node then takes two nodes' worth of storage out at once.
	ClusterUpdateNodeResuming = "Resuming"
	ClusterUpdateNodeDone     = "Done"
	ClusterUpdateNodeFailed   = "Failed"
)

// Active reports whether the run is still the controller's business.
func (r ClusterUpdateRun) Active() bool {
	return r.State == ClusterUpdatePending || r.State == ClusterUpdateRunning
}

// Terminal reports whether the run has finished, one way or another.
func (r ClusterUpdateRun) Terminal() bool {
	switch r.State {
	case ClusterUpdateSucceeded, ClusterUpdateFailed, ClusterUpdateCancelled:
		return true
	}
	return false
}

// CurrentNode returns the node the run is working on, and whether there is one.
// A run works strictly one node at a time, so this is the first node that has
// neither finished nor been skipped.
func (r ClusterUpdateRun) CurrentNode() (int, bool) {
	for i, n := range r.Nodes {
		switch n.State {
		case ClusterUpdateNodeDone, ClusterUpdateNodeSkipped, ClusterUpdateNodeFailed:
			continue
		}
		return i, true
	}
	return 0, false
}
