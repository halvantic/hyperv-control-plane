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

	// NetworkMap points each source adapter at a Hyper-V distributed port. A
	// source NIC with no mapping arrives DISCONNECTED rather than guessing: a VM
	// silently attached to the wrong network is worse than one that obviously
	// has none.
	NetworkMap []MigrationNIC `json:"networkMap,omitempty"`

	/* ProcessorCount and MemoryStartupBytes resize the VM as it arrives. Zero
	   means "as the source is", read at import time rather than copied from the
	   plan — a VM given another 8GB in the hours since would otherwise arrive as
	   the machine it used to be.

	   Offered because migration is the one moment resizing is free: the VM is
	   being built from nothing and is not running, so a change that would
	   otherwise need a power cycle costs nothing at all. */
	ProcessorCount     int    `json:"processorCount,omitempty"`
	MemoryStartupBytes uint64 `json:"memoryStartupBytes,omitempty"`

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

// MigrationNIC maps one source adapter, or one source network, to a destination.
type MigrationNIC struct {
	// SourceNetwork is the portgroup name as VMware reported it.
	SourceNetwork string `json:"sourceNetwork"`

	/* DeviceKey binds this mapping to ONE source adapter, by VMware's own device
	   key. Zero means the mapping applies to every adapter on SourceNetwork,
	   which is what a batch of ten VMs off the same portgroup wants.

	   Both forms exist because both questions are real. Two adapters of one VM
	   on the same portgroup — a guest that does its own teaming, or one that
	   fronts two services — are two different destinations, and a mapping keyed
	   on the network alone cannot tell them apart. */
	DeviceKey int32 `json:"deviceKey,omitempty"`

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

	// RetriedAt and RetriedBy record that a finished migration was started
	// again, and by whom. Kept because a run that succeeded on its third attempt
	// is not the same story as one that succeeded outright, and the job list is
	// the only other place that says so.
	RetriedAt time.Time `json:"retriedAt,omitempty"`
	RetriedBy string    `json:"retriedBy,omitempty"`

	/* Copying is per-disk progress for the pass running RIGHT NOW.

	   Its own field rather than folded into Disks, because the two count
	   different things: Disks accumulates what every pass has copied, and this
	   is what the current one has moved so far. Adding a live figure into a
	   running total would make a delta pass appear to have copied the whole disk
	   again. Replaced on every report and cleared when the pass ends. */
	Copying []MigrationDisk `json:"copying,omitempty"`

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

	/* Archived takes a finished migration out of the operations list without
	   destroying what it knows.

	   "Remove" used to delete the record, and the record is the only thing that
	   says this VM was ever migrated: the source inventory reads it to mark a VM
	   as moved, so deleting it made an already-migrated VM look untouched and
	   invited copying half a terabyte a second time. Tidying the list and losing
	   the history are different intentions, and only one of them was on offer.

	   Archived records are still listed by the API — the per-VM history depends
	   on them — and are simply not part of what is in flight. */
	Archived   bool      `json:"archived,omitempty"`
	ArchivedAt time.Time `json:"archivedAt,omitempty"`
	ArchivedBy string    `json:"archivedBy,omitempty"`

	/* NoChangeTracking records that a pass had to read a disk in full because
	   the source could not answer a change-tracking query.

	   It is the reason a warm migration will not converge, and it is a different
	   reason from the one everybody assumes. Without it the console had one
	   explanation for a delta that does not shrink — the guest is writing faster
	   than the link — and offered it as fact on a VM that was powered off. */
	NoChangeTracking bool `json:"noChangeTracking,omitempty"`
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

/*
MigrationPassResult is what one copy pass on a host actually did.

	Reported in HostStatus rather than returned in the job's message, because the
	centre needs the per-disk change markers to run the NEXT pass and a job
	message is prose for a person to read. Parsing numbers back out of a sentence
	is how a migration ends up resuming from a marker nobody checked.

	Kept only while the migration is live. A host does not carry the history of
	every VM it has ever pulled off VMware; the Migration object is where that
	lives.
*/
type MigrationPassResult struct {
	// Migration is the object this pass belongs to, and JobID the job that ran
	// it — so a result from a job the centre has already given up on is
	// recognisable as stale rather than applied on top of a newer one.
	Migration string `json:"migration"`
	JobID     string `json:"jobId,omitempty"`

	/* InProgress marks a result reported WHILE the pass is still running.

	   A base copy of half a terabyte takes hours, and until this existed the
	   only per-disk numbers were written when the pass finished — so the console
	   showed "nothing copied yet" for the whole of it while the bytes were
	   plainly moving. The progress meter had nothing to draw.

	   It is display only, and the centre must treat it as such: a partial result
	   carries no change markers, because the marker for the next pass is not
	   known until this one has read to the end. Folding one in as though the
	   pass had finished would resume the next pass from a point never reached. */
	InProgress bool `json:"inProgress,omitempty"`

	// Final marks the cutover pass, the one that ran with the source stopped.
	Final bool `json:"final,omitempty"`
	// SourcePoweredOff records that this pass stopped the guest. It is the
	// moment a production workload stopped, so it belongs in the record.
	SourcePoweredOff bool `json:"sourcePoweredOff,omitempty"`

	/* FullRead marks a pass that had to read at least one disk END TO END
	   because the source could not answer a change-tracking query.

	   It reaches the centre because the centre is what decides whether more
	   passes are worth running, and without it a warm migration converges on
	   nothing: with no marker to resume from, every "delta" re-reads the whole
	   disk, the outstanding figure never falls, and the console blames the guest
	   for writing faster than the link — on a VM that was switched off. */
	FullRead bool `json:"fullRead,omitempty"`

	Disks       []MigrationPassDisk `json:"disks,omitempty"`
	CopiedBytes int64               `json:"copiedBytes,omitempty"`

	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`

	// RetriedAt and RetriedBy record that a finished migration was started
	// again, and by whom. Kept because a run that succeeded on its third attempt
	// is not the same story as one that succeeded outright, and the job list is
	// the only other place that says so.
	RetriedAt time.Time `json:"retriedAt,omitempty"`
	RetriedBy string    `json:"retriedBy,omitempty"`

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
