package types

import "time"

/*
One VM's journey off VMware.

	A Migration is an OBJECT with a position, not a job. A warm migration runs
	for hours — a base copy of half a terabyte, then delta passes until the
	outstanding change is small enough to close in a maintenance window — and it
	has to survive the centre restarting in the middle. Jobs in Ballast are
	one-shot and short by design; this holds the position and enqueues one job at
	a time, the same shape the runbook controller uses for the same reason.

	THE PHASES, and why each exists:

	  Pending      authored, nothing done. The source is untouched.
	  Preparing    Changed Block Tracking is being turned on. This is the first
	               point at which Ballast modifies somebody else's VM, and on a
	               running guest it cannot take effect until a power cycle — so
	               a VM that needs it is refused at authoring time rather than
	               discovered here.
	  BaseCopy     the full disk, read from a VMware snapshot so the guest keeps
	               running. The long one.
	  Syncing      repeated delta passes. Each reads what changed since the last
	               pass's changeId and writes only those ranges. Converging is
	               not guaranteed: a guest writing faster than the link can carry
	               will never close, and the console has to show that rather than
	               spinning for ever.
	  ReadyToCutOver  the delta is small and Ballast is WAITING. Deliberately a
	               resting state with no timer: powering off a production
	               workload is not a decision a control plane makes because a
	               number got small.
	  CuttingOver  the operator said go. The source is powered off, a final delta
	               runs against a guest that can no longer change, and the disks
	               are finalised.
	  Importing    desired state is authored and the ordinary reconciler builds
	               the VM — the same path a deployed template takes, so a
	               migrated VM is indistinguishable from any other from the
	               moment it exists.
	  Done / Failed / Cancelled.

	WHAT IS DELIBERATELY NOT HERE: nothing deletes or unregisters the source VM.
	It is left powered off and intact, which is the rollback.
*/
type Migration struct {
	Meta   ObjectMeta      `json:"meta"`
	Spec   MigrationSpec   `json:"spec"`
	Status MigrationStatus `json:"status"`
}

type MigrationSpec struct {
	// SourceName is the registered MigrationSource, and MoRef is VMware's own
	// identity for the VM. The NAME is a label an operator can change under us
	// and two VMs may share one, so it is carried for display and never used to
	// find anything.
	SourceName string `json:"sourceName"`
	MoRef      string `json:"moRef"`
	SourceVM   string `json:"sourceVm,omitempty"`

	// TargetHost is the Hyper-V host that does the work and ends up owning the
	// VM. It pulls from VMware itself — one hop, no staging at the centre — so
	// it needs a route to the source.
	TargetHost string `json:"targetHost"`
	// TargetCluster, when set, is the cluster the finished VM is placed in. The
	// copy still runs on TargetHost; this decides where the VM lives afterwards.
	TargetCluster string `json:"targetCluster,omitempty"`

	// DestinationPath is where the VHDXs are written, e.g. a CSV mount.
	DestinationPath string `json:"destinationPath"`

	// NewName renames the VM on arrival. Empty keeps the source's name — which
	// is usually right, and is a collision when the source is still registered
	// somewhere Ballast also manages.
	NewName string `json:"newName,omitempty"`

	// NetworkMap points each source portgroup at a Hyper-V switch or
	// distributed port. A source NIC with no mapping arrives DISCONNECTED
	// rather than guessing: a VM silently attached to the wrong network is
	// worse than one that obviously has none.
	NetworkMap []MigrationNIC `json:"networkMap,omitempty"`

	// KeepMAC carries the source's MAC addresses over. Off by default: the
	// original VM still exists, and two machines with one address is a fault
	// that presents as intermittent and takes a day to find.
	KeepMAC bool `json:"keepMac,omitempty"`

	// Warm selects the converging path. False copies once, which requires the
	// source to be off already.
	Warm bool `json:"warm,omitempty"`

	// StartAfterCutover powers the migrated VM on once it is built. Off by
	// default — an operator migrating at 02:00 may want to check it before it
	// starts serving.
	StartAfterCutover bool `json:"startAfterCutover,omitempty"`
}

// MigrationNIC maps one source network to a destination.
type MigrationNIC struct {
	// SourceNetwork is the portgroup name as VMware reported it.
	SourceNetwork string `json:"sourceNetwork"`
	// Switch and Dvport are the Hyper-V side. Both empty leaves the adapter
	// disconnected, which is the honest default for an unmapped network.
	Switch string `json:"switch,omitempty"`
	Dvport string `json:"dvport,omitempty"`
	VLANID int    `json:"vlanId,omitempty"`
}

// Migration phases.
const (
	MigrationPending        = "Pending"
	MigrationPreparing      = "Preparing"
	MigrationBaseCopy       = "BaseCopy"
	MigrationSyncing        = "Syncing"
	MigrationReadyToCutOver = "ReadyToCutOver"
	MigrationCuttingOver    = "CuttingOver"
	MigrationImporting      = "Importing"
	MigrationDone           = "Done"
	MigrationFailed         = "Failed"
	MigrationCancelled      = "Cancelled"
)

// MigrationTerminal reports a phase nothing will move on from.
func MigrationTerminal(phase string) bool {
	return phase == MigrationDone || phase == MigrationFailed || phase == MigrationCancelled
}

type MigrationStatus struct {
	Phase   string `json:"phase"`
	Message string `json:"message,omitempty"`

	// JobID is the agent job this migration is currently waiting on, if any.
	JobID string `json:"jobId,omitempty"`

	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`

	// Disks carry per-disk progress. A VM with four disks copying at different
	// rates is normal, and one aggregate percentage hides a disk that has
	// stalled while the others finish.
	Disks []MigrationDisk `json:"disks,omitempty"`

	// Passes counts completed delta passes, and OutstandingBytes is what the
	// last one found still to copy.
	//
	// Both are shown because the pair is what says whether this will ever
	// finish. A delta that is not shrinking pass over pass means the guest is
	// writing faster than the link carries, and no amount of waiting fixes it —
	// the operator needs to cut over during a quiet period or accept a longer
	// outage, and they can only decide that if they can see it.
	Passes           int   `json:"passes,omitempty"`
	OutstandingBytes int64 `json:"outstandingBytes,omitempty"`
	// LastPassBytes is what the previous pass copied, so the trend is legible
	// without keeping a history.
	LastPassBytes int64     `json:"lastPassBytes,omitempty"`
	LastPassAt    time.Time `json:"lastPassAt,omitempty"`

	// Converging is whether the outstanding delta is falling. False with a
	// non-zero delta is the case above, and it is reported rather than inferred
	// by the console so both agree.
	Converging      bool `json:"converging,omitempty"`
	ConvergingKnown bool `json:"convergingKnown,omitempty"`

	// SnapshotRef is the VMware snapshot the current pass reads from. Recorded
	// so a failed migration can be cleaned up: a snapshot left behind on
	// somebody else's VM grows until their datastore fills, which is the worst
	// thing this feature could leave on a system it does not own.
	SnapshotRef string `json:"snapshotRef,omitempty"`

	/* Whether the snapshot actually came off, and what stopped it if not.

	   The cleanup job runs on the way out of every terminal phase, and it is
	   deliberately not waited on — a migration that is already Done or Failed
	   has no phase to move to when the job lands. But not waiting is not the
	   same as not looking: without these the migration reads "cancelled, the
	   source VM is untouched" while a Ballast snapshot sits on somebody else's
	   VM growing until their datastore fills, and nothing anywhere reports it.
	   The centre knew the job failed and said nothing, which is the failure the
	   brief calls a defect rather than a runbook step.

	   SnapshotRemoved is a POSITIVE confirmation and is only ever set from the
	   cleanup job succeeding. False means "not confirmed", which includes "the
	   job has not finished yet" — it is never read as "there is a snapshot
	   there", because absent is not the same as zero. */
	CleanupJobID    string `json:"cleanupJobId,omitempty"`
	SnapshotRemoved bool   `json:"snapshotRemoved,omitempty"`
	// SnapshotProblem is the agent's own words when the snapshot did not come
	// off. Present means a real snapshot is still on the source VM.
	SnapshotProblem string `json:"snapshotProblem,omitempty"`
	// SnapshotCleanupRequested is an operator asking to try again. A flag the
	// controller acts on rather than an action REST takes directly, because the
	// source credential and the job's parameters are the controller's to
	// resolve — the same shape as releasing a cutover.
	SnapshotCleanupRequested bool `json:"snapshotCleanupRequested,omitempty"`

	// CBTEnabledByBallast records that Ballast turned Changed Block Tracking on.
	// It is left on afterwards — turning it off needs another power cycle, and
	// doing that to somebody's VM to tidy up a setting is worse than leaving a
	// harmless one enabled — but the fact is recorded so nobody has to guess
	// later whether it was always like that.
	CBTEnabledByBallast bool `json:"cbtEnabledByBallast,omitempty"`

	// CancelRequested is a person asking to stop. Recorded rather than acted on
	// directly because the controller has one thing it must still do on the way
	// out — remove the snapshot from the source — and a cancel that skipped it
	// would leave the worst possible thing behind on a system Ballast does not
	// own.
	CancelRequested   bool      `json:"cancelRequested,omitempty"`
	CancelRequestedBy string    `json:"cancelRequestedBy,omitempty"`
	CancelRequestedAt time.Time `json:"cancelRequestedAt,omitempty"`

	// CutoverRequestedBy and At record who released the cutover, because it is
	// the moment a production workload stopped.
	CutoverReleased    bool      `json:"cutoverReleased,omitempty"`
	CutoverRequestedBy string    `json:"cutoverRequestedBy,omitempty"`
	CutoverRequestedAt time.Time `json:"cutoverRequestedAt,omitempty"`
}

type MigrationDisk struct {
	// Key is VMware's device key and is the identity across passes; Label is
	// what the operator sees in vSphere Client.
	Key   int32  `json:"key"`
	Label string `json:"label,omitempty"`
	// SourcePath is the VMDK, DestPath the VHDX being written.
	SourcePath string `json:"sourcePath,omitempty"`
	DestPath   string `json:"destPath,omitempty"`

	SizeBytes   int64 `json:"sizeBytes,omitempty"`
	CopiedBytes int64 `json:"copiedBytes,omitempty"`
	// ChangeID is VMware's marker for "everything up to here has been copied".
	// The next delta asks what changed since it.
	//
	// A RESET is the hazard: storage vMotion, a failed snapshot consolidation
	// and some power operations invalidate it, and VMware then reports every
	// block as changed. Detected rather than trusted, because the alternative is
	// copying the whole disk again while telling the operator it is a delta.
	ChangeID string `json:"changeId,omitempty"`

	Done    bool   `json:"done,omitempty"`
	Message string `json:"message,omitempty"`
}

/* MigrationPassResult is what one copy pass on a host actually did.

   Reported in HostStatus rather than returned in the job's message, because the
   centre needs the per-disk change markers to run the NEXT pass and a job
   message is prose for a person to read. Parsing numbers back out of a sentence
   is how a migration ends up resuming from a marker nobody checked.

   Kept only while the migration is live. A host does not carry the history of
   every VM it has ever pulled off VMware; the Migration object is where that
   lives. */
type MigrationPassResult struct {
	// Migration is the object this pass belongs to, and JobID the job that ran
	// it — so a result from a job the centre has already given up on is
	// recognisable as stale rather than applied on top of a newer one.
	Migration string `json:"migration"`
	JobID     string `json:"jobId,omitempty"`

	// Final marks the cutover pass, the one that ran with the source stopped.
	Final bool `json:"final,omitempty"`
	// SourcePoweredOff records that this pass stopped the guest. It is the
	// moment a production workload stopped, so it belongs in the record.
	SourcePoweredOff bool `json:"sourcePoweredOff,omitempty"`

	Disks       []MigrationPassDisk `json:"disks,omitempty"`
	CopiedBytes int64               `json:"copiedBytes,omitempty"`

	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`

	// Error is the agent's own words when the pass failed. Present with disks
	// already filled in is normal and useful: a four-disk VM that failed on the
	// third still copied two, and the markers for those are worth keeping.
	Error string `json:"error,omitempty"`
	// CBTReset says the failure was an invalidated change marker, which is
	// recoverable by reading in full rather than a reason to fail a migration
	// that is still perfectly possible.
	CBTReset bool `json:"cbtReset,omitempty"`
}

// MigrationPassDisk is one disk within a pass.
type MigrationPassDisk struct {
	Key         int32  `json:"key"`
	Label       string `json:"label,omitempty"`
	SourcePath  string `json:"sourcePath,omitempty"`
	DestPath    string `json:"destPath,omitempty"`
	SizeBytes   int64  `json:"sizeBytes,omitempty"`
	CopiedBytes int64  `json:"copiedBytes,omitempty"`
	// NextChangeID is the marker the following pass resumes from.
	NextChangeID string `json:"nextChangeId,omitempty"`
}
