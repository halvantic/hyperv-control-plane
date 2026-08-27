package types

import "time"

/* Migrating a VM off VMware, warm.

   A cold migration is a copy: power the VM off, move the disks, build it here.
   A warm one is a CONVERGENCE — copy the disks while the guest runs, then keep
   copying only what changed until the outstanding delta is small enough that a
   final pass fits inside a maintenance window. Downtime stops being "as long as
   500GB takes" and becomes "as long as the last few hundred megabytes take".

   That is the whole reason this is a state machine and not a job. A warm
   migration runs for hours, over many passes, and has to survive the centre
   restarting in the middle. Jobs are one-shot and short by design (see
   JobVMHealthProbe for the same reasoning applied to a runbook gate); the
   migration OBJECT holds the position and enqueues one job at a time.

   THREE THINGS THIS DESIGN COMMITS TO, each of which is expensive to change:

   1. IT MODIFIES THE SOURCE. Changed Block Tracking has to be enabled on the
      VM, and every pass takes a VMware snapshot to read from. Ballast is not
      read-only on infrastructure it does not manage, and the console says so
      before the first pass rather than after.

   2. THE DESTINATION IS A FIXED VHDX, WRITTEN BY SEEK. A delta is a list of
      byte ranges; applying one means writing at offsets inside a disk that
      already exists. Streaming through a converter into a dynamic VHDX would
      make every pass a full rewrite, which is the thing warm migration exists
      to avoid.

   3. CUTOVER IS THE OPERATOR'S. Ballast converges and then WAITS. Powering off
      a production workload is not a decision a control plane makes because a
      delta got small enough. */

// MigrationSource is a vCenter or ESXi endpoint Ballast can read VMs from.
//
// Registered once and reused: an operator migrating twenty VMs should give
// their credentials once, and the credential lives in the vault like every
// other, never on the object.
type MigrationSource struct {
	Meta   ObjectMeta            `json:"meta"`
	Spec   MigrationSourceSpec   `json:"spec"`
	Status MigrationSourceStatus `json:"status"`
}

type MigrationSourceSpec struct {
	// Address is the hostname or IP of vCenter, or of a standalone ESXi host.
	// Scheme and path are added by the client; an operator types what they type
	// into vSphere Client.
	Address string `json:"address"`

	// Kind is "vCenter" or "ESXi". It changes nothing about the protocol — both
	// speak the same API — but it changes what the console can promise: a
	// vCenter sees every host it manages, a single ESXi sees only itself.
	Kind string `json:"kind,omitempty"`

	// CredentialSecret names a vault secret of type VMwareCredential.
	CredentialSecret string `json:"credentialSecret"`

	// InsecureTLS skips certificate verification.
	//
	// Defaulting this to true would be the easy thing: a great many vCenters
	// still carry their self-signed installation certificate, and the first
	// connection attempt fails without it. It defaults to FALSE anyway, because
	// a control plane that silently accepts any certificate on the network path
	// carrying its credentials is not one to hand somebody else. The console
	// offers the tick, names what it costs, and shows the certificate's subject
	// and fingerprint so the choice is informed rather than reflexive.
	InsecureTLS bool `json:"insecureTLS,omitempty"`

	// Datacenter narrows a vCenter with more than one. Empty means every one.
	Datacenter string `json:"datacenter,omitempty"`
}

type MigrationSourceStatus struct {
	// Reachable and Message are the last connection attempt. Absent is not
	// "unreachable" — it is "never tried" — so both the flag and its knownness
	// travel, the same distinction ContentsKnown draws for a LUN.
	Reachable      bool      `json:"reachable,omitempty"`
	ReachableKnown bool      `json:"reachableKnown,omitempty"`
	Message        string    `json:"message,omitempty"`
	CheckedAt      time.Time `json:"checkedAt,omitempty"`

	// Product and Version are what answered, so an operator can tell a vCenter
	// from an ESXi that was registered as one by mistake.
	Product string `json:"product,omitempty"`
	Version string `json:"version,omitempty"`

	// CertSubject and CertFingerprint describe the certificate presented, so a
	// decision to skip verification can be made against something.
	CertSubject     string `json:"certSubject,omitempty"`
	CertFingerprint string `json:"certFingerprint,omitempty"`
}

// VMwareVM is one VM as the source reports it — enough to decide whether and
// how to migrate it, and nothing more.
type VMwareVM struct {
	// MoRef is VMware's own identity ("vm-1234") and is what every later call
	// uses. The NAME is a label an operator can change under us, and two VMs in
	// different folders may share one.
	MoRef string `json:"moRef"`
	Name  string `json:"name"`

	PowerState string `json:"powerState,omitempty"` // poweredOn | poweredOff | suspended
	GuestID    string `json:"guestId,omitempty"`
	GuestOS    string `json:"guestOs,omitempty"`

	CPUCount int32 `json:"cpuCount,omitempty"`
	MemoryMB int32 `json:"memoryMb,omitempty"`
	// ProvisionedBytes is the sum of the disks' declared sizes; UsedBytes is
	// what they actually occupy. A warm migration copies the FORMER, so the
	// difference is what an operator needs to size the destination against.
	ProvisionedBytes int64 `json:"provisionedBytes,omitempty"`
	UsedBytes        int64 `json:"usedBytes,omitempty"`

	// Firmware decides the Hyper-V generation, and it is not a preference: a
	// BIOS guest cannot boot as Generation 2 and a UEFI one cannot boot as
	// Generation 1. Read, never asked.
	Firmware string `json:"firmware,omitempty"` // bios | efi

	// CBTEnabled is whether Changed Block Tracking is already on. Enabling it
	// needs a power cycle, so a running VM without it cannot be migrated warm
	// until somebody restarts it — which is a fact worth knowing BEFORE
	// choosing warm, not after the first pass.
	CBTEnabled bool `json:"cbtEnabled,omitempty"`

	// Disks are the VM's virtual disks, in controller order.
	Disks []VMwareDisk `json:"disks,omitempty"`
	// NICs carry the MAC and the portgroup, so the console can offer a mapping
	// rather than making the operator remember which network was which.
	NICs []VMwareNIC `json:"nics,omitempty"`

	// Host and Datastore say where it lives now, which is what decides whether
	// the destination Hyper-V host can reach it at all.
	Host      string `json:"host,omitempty"`
	Datastore string `json:"datastore,omitempty"`

	// Snapshots is how many the VM already has. A VM with an existing snapshot
	// chain is not refused, but the copy reads the flattened current state and
	// the chain does not come across — so it has to be said.
	Snapshots int `json:"snapshots,omitempty"`

	// Blockers are reasons this VM cannot be migrated as it stands, worked out
	// once at inventory time so the console can grey the row rather than
	// letting somebody select it and fail twenty minutes in.
	Blockers []string `json:"blockers,omitempty"`
}

type VMwareDisk struct {
	Key int32 `json:"key"`
	// Label is VMware's own ("Hard disk 1"), kept because it is what an
	// operator sees in vSphere Client and will look for here.
	Label     string `json:"label,omitempty"`
	Path      string `json:"path,omitempty"` // "[datastore1] Web01/Web01.vmdk"
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Thin      bool   `json:"thin,omitempty"`
	// Sharing and RDM disks cannot be copied this way and are named as blockers
	// rather than being silently skipped, which would migrate a VM missing a
	// disk and look like success.
	RDM     bool `json:"rdm,omitempty"`
	Sharing bool `json:"sharing,omitempty"`
}

type VMwareNIC struct {
	Key int32 `json:"key"`
	// MAC is recorded but deliberately NOT carried over by default. A migrated
	// VM keeping its MAC while the original still exists is two machines with
	// one address; the operator can ask for it, having been told.
	MAC       string `json:"mac,omitempty"`
	Network   string `json:"network,omitempty"`
	Type      string `json:"type,omitempty"` // vmxnet3 | e1000 | ...
	Connected bool   `json:"connected,omitempty"`
}
