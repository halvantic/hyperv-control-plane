package types

import "time"

/* Emptying a Hyper-V host or cluster onto another one.

   The single-VM move already exists — shared-nothing live migration, Move-VM
   with -IncludeStorage, run on the SOURCE host, with the Kerberos and
   constrained-delegation setup that makes a service-initiated move work at all.
   What was missing is doing it to forty VMs without a person driving each one,
   which is what building a new environment actually consists of.

   AN OBJECT, NOT A BUTTON. An evacuation runs for hours, the centre may restart
   in the middle of it, and half-moved is a real state somebody has to be able
   to look at. So it is a record with per-VM outcomes and a controller that
   takes one step per tick, the same shape as a VMware migration and for the
   same reasons.

   PLACEMENT IS REWRITTEN AFTER THE MOVE, NEVER BEFORE. Spec.Placement.HostName
   decides which agent owns a VM, and the move itself has to be run by the agent
   that currently HAS it. Rewriting placement first points the destination agent
   at a VM it has not got, and an agent reconciling a VM it cannot find creates
   one — an empty machine with the right name over the top of a real migration.

   WHAT IS DELIBERATELY NOT HERE: nothing removes the source host from anything,
   and nothing decommissions it. An evacuation moves workloads off; what happens
   to the empty host afterwards is a separate decision, taken by a person. */
type Evacuation struct {
	Meta   ObjectMeta       `json:"meta"`
	Spec   EvacuationSpec   `json:"spec"`
	Status EvacuationStatus `json:"status"`
}

type EvacuationSpec struct {
	/* SourceHost or SourceCluster names what is being emptied. Exactly one.

	   They are separate fields rather than one string with a type beside it
	   because the work genuinely differs: a clustered VM is owned by the
	   cluster, and moving it off means removing its HA role first — with the
	   role left removed if the destination is standalone, and re-created there
	   if it is not. Collapsing both into "source" would leave the code deciding
	   which it was from context, on an object whose whole purpose is to be
	   unambiguous about a destructive-looking operation. */
	SourceHost    string `json:"sourceHost,omitempty"`
	SourceCluster string `json:"sourceCluster,omitempty"`

	// TargetHost or TargetCluster is where the VMs are going. Exactly one. A
	// cluster target means each VM arrives as an HA role; the move itself still
	// runs against a specific member node, chosen when the VM is moved rather
	// than fixed here, so a member going down between planning and moving does
	// not strand the rest of the evacuation.
	TargetHost    string `json:"targetHost,omitempty"`
	TargetCluster string `json:"targetCluster,omitempty"`

	/* DestinationPath is where each VM's files land, e.g. a CSV mount. The VM's
	   own name is appended, so twenty VMs do not share one directory.

	   Required. Move-VM defaults to C:\VMs\<name> when it is empty, which is
	   almost never what somebody evacuating onto shared storage meant, and is
	   discovered when the destination's system volume fills. */
	DestinationPath string `json:"destinationPath"`

	/* VMs limits the evacuation to these VMs by name. Empty means every VM the
	   source currently holds, read when each VM is picked up rather than fixed
	   at planning time — a VM created on the source after the plan was made is
	   still on the host being emptied, and leaving it behind is the one outcome
	   an evacuation must not produce.

	   Named VMs are the other half of the same question: moving a subset is how
	   an operator drains a host gradually, or retries the three that failed. */
	VMs []string `json:"vms,omitempty"`

	/* Strategy is how the VMs get there: "move" or "copy". Empty means move.

	   MOVE is Move-VM with its storage — one operation, no downtime for a
	   running guest, and it depends on the destination being compatible: live
	   migration enabled both ends, Kerberos delegation between the computer
	   accounts, SMB reachable, processors that match.

	   COPY exports each VM to the destination's storage and imports it there,
	   then removes the original. It needs the guest stopped and it writes every
	   byte twice, but it asks almost nothing of the two hosts — which is what a
	   move into a NEW environment usually looks like. It is also the path whose
	   compatibility fixing is known to work: Compare-VM's report can be resolved
	   on the host holding the files, which is how Ballast already imports a VM
	   whose switch does not exist here.

	   Neither is the default for all time. Move is the optimisation; copy is the
	   one that works when the two ends have nothing arranged between them. */
	Strategy string `json:"strategy,omitempty"`

	/* NetworkMap points each source switch at one on the destination.

	   Without it a move fails on compatibility rather than on anything to do
	   with the copy:

	     The virtual machine 'HVNew01' is not compatible with physical computer
	     'HVNEW06'. Could not find Ethernet switch 'ConvergedSwitch2'.

	   A VM carries the NAME of the switch its adapters are on, and two hosts
	   built separately do not agree on names — which is most of why an
	   evacuation exists at all: moving into a new environment. So the mapping is
	   asked for rather than guessed, and a source network with no entry is
	   ARRIVED DISCONNECTED rather than attached to whatever happens to be there.
	   A VM silently on the wrong network is worse than one obviously on none. */
	NetworkMap []EvacuationNIC `json:"networkMap,omitempty"`

	/* Concurrency is how many VMs move at once. Zero means one.

	   Deliberately conservative. Every concurrent move is a full copy of a VM's
	   storage across the same link, and running ten at once does not move them
	   ten times faster — it makes all ten slow, and makes the first completion,
	   which is the one that tells an operator this is working, arrive last. */
	Concurrency int `json:"concurrency,omitempty"`

	/* StopOnFailure halts the evacuation when a VM fails to move, rather than
	   carrying on to the next.

	   On by default (the field is the opt-OUT) because the first failure is
	   usually a fact about the pair of hosts rather than about that VM: no
	   delegation, no route, incompatible processors, a destination path that
	   does not exist. Marching thirty-nine more VMs into the same wall produces
	   thirty-nine identical failures and an operator who has to read all of
	   them to learn one thing. */
	ContinueOnFailure bool `json:"continueOnFailure,omitempty"`
}

/* EvacuationNIC maps one source switch to a destination.

   By switch NAME on both sides, because that is what a VM's adapter carries and
   what Hyper-V compares. A dvport chosen in the console resolves to its switch
   and VLAN before it gets here, the same way a VMware migration's does. */
type EvacuationNIC struct {
	/* VM binds this mapping to ONE virtual machine. Empty applies it to every VM
	   on SourceSwitch, which is what forty VMs off one switch want — that is one
	   decision, not forty.

	   Both forms exist because both questions are real. A pair of VMs on the same
	   switch can belong on different networks at the other end: one fronting a
	   service, one that should land somewhere isolated until it is checked. A
	   mapping keyed on the switch alone cannot tell them apart, and making an
	   operator answer per VM to express the rare case would make the ordinary
	   case forty times harder. */
	VM string `json:"vm,omitempty"`

	// SourceSwitch is the vSwitch name the VM's adapter is attached to now.
	SourceSwitch string `json:"sourceSwitch"`
	// TargetSwitch is the vSwitch on the destination. Empty means the adapter
	// arrives disconnected, which is a deliberate answer and not a gap.
	TargetSwitch string `json:"targetSwitch,omitempty"`
	// VLANID retags the adapter on arrival. Zero leaves the tag alone.
	VLANID int `json:"vlanId,omitempty"`
}

// Evacuation strategies.
const (
	EvacMove = "move"
	EvacCopy = "copy"
)

// EvacuationStrategy is the strategy with its default applied.
func (s EvacuationSpec) EvacuationStrategy() string {
	if s.Strategy == EvacCopy {
		return EvacCopy
	}
	return EvacMove
}

// Evacuation phases.
const (
	EvacuationPending   = "Pending"
	EvacuationMoving    = "Moving"
	EvacuationDone      = "Done"
	EvacuationFailed    = "Failed"
	EvacuationCancelled = "Cancelled"
)

// EvacuationTerminal reports whether an evacuation has finished, however it
// finished.
func EvacuationTerminal(phase string) bool {
	return phase == EvacuationDone || phase == EvacuationFailed || phase == EvacuationCancelled
}

type EvacuationStatus struct {
	Phase   string `json:"phase"`
	Message string `json:"message,omitempty"`

	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`

	/* VMs is one entry per VM the evacuation has taken responsibility for, in
	   the order it picked them up.

	   The record of what happened to each, kept whatever the outcome. A VM that
	   failed to move is the single most useful thing on this object — it is
	   still on the source, still running, and somebody has to decide what to do
	   about it — so it is never dropped to tidy the list. */
	VMs []EvacuationVM `json:"vms,omitempty"`

	// Cancelled records that a person stopped this. Moves already running are
	// left to finish: a live migration interrupted halfway is the one state
	// worse than either end of it.
	CancelledBy string    `json:"cancelledBy,omitempty"`
	CancelledAt time.Time `json:"cancelledAt,omitempty"`
}

// Evacuation per-VM states.
const (
	EvacVMPending = "Pending"
	EvacVMMoving  = "Moving"
	EvacVMMoved   = "Moved"
	EvacVMFailed  = "Failed"
	// EvacVMSkipped is a VM that cannot be live migrated and was not attempted.
	// Distinct from Failed: nothing went wrong, and retrying changes nothing
	// until the reason is dealt with.
	EvacVMSkipped = "Skipped"
)

type EvacuationVM struct {
	Name string `json:"name"`
	// State is one of the EvacVM constants.
	State string `json:"state"`
	// JobID is the move job on the source host, so the activity log and this
	// record point at each other.
	JobID string `json:"jobId,omitempty"`
	// Node is the member the VM was moved ONTO for a cluster target, which is
	// not knowable from the spec and is what somebody looking for the VM needs.
	Node string `json:"node,omitempty"`
	// Message is why a VM failed or was skipped. Empty on success — a moved VM
	// needs no explanation.
	Message   string    `json:"message,omitempty"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
}

// EvacuationCounts summarises the per-VM states, for a one-line message and for
// the console's progress. Computed rather than stored: two figures that can
// disagree with the list they came from is a bug waiting to be believed.
func (s EvacuationStatus) Counts() (moved, failed, skipped, pending, moving int) {
	for _, v := range s.VMs {
		switch v.State {
		case EvacVMMoved:
			moved++
		case EvacVMFailed:
			failed++
		case EvacVMSkipped:
			skipped++
		case EvacVMMoving:
			moving++
		default:
			pending++
		}
	}
	return
}
