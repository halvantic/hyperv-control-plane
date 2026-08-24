package types

import "time"

// ---------------------------------------------------------------------------
// Replication runbooks
// ---------------------------------------------------------------------------
//
// This file is part of the same schema package as desiredstate.go and is
// governed by the same rule: it is the source of truth for these types and
// every consumer references it. It is a separate file only because
// desiredstate.go is already long, not because runbooks are a separate model.

// Runbook is an authored, reusable disaster-recovery plan: which VMs fail over,
// in what order, and what must be true before the next group is allowed to
// start. Running one produces a RunbookRun.
//
// A runbook is an ORCHESTRATOR over relationships that already exist. It does
// not declare replication and it does not declare a target: every VM in it
// already carries VMReplicationSpec, which names the replica server, and the
// centre resolves the host holding the replica copy from that. Restating the
// target here would create a second authority over one relationship, and the
// two would disagree the first time somebody moved a replica.
//
// Like Site, Dvport, VMTemplate and ClusterUpdateRun this is centre-only: no
// agent is ever given one, nothing reconciles towards it, and it needs no
// proto. What it drives is ordinary Jobs and ordinary desired state, so a
// failover a runbook performs is indistinguishable from one an operator
// performed by hand — there is no second code path to keep honest.
//
// The order is the whole point. Domain controllers and DNS must be serving
// before SQL will start, and SQL must be serving before the application tier
// comes up healthy. Booting everything at once is not a faster recovery, it is
// a recovery that fails in a way nobody can read.
type Runbook struct {
	Meta ObjectMeta  `json:"meta"`
	Spec RunbookSpec `json:"spec"`
}

// RunbookSpec is the authored plan.
type RunbookSpec struct {
	// Description is free text for the operator running this at 03:00 — what
	// this plan recovers and anything they need to know before starting it.
	Description string `json:"description,omitempty"`

	// Groups are walked strictly in order. Members WITHIN a group are started in
	// parallel; the gate BETWEEN groups is absolute.
	Groups []RunbookGroup `json:"groups,omitempty"`

	// TestIsolation configures the network bubble a test failover runs in. Nil
	// means test failovers refuse to run: a test VM that boots onto the
	// production network duplicates the IP and MAC of a machine that is still
	// serving, which is a worse outcome than not testing. Making the isolation
	// explicit rather than defaulted is deliberate — the operator says where the
	// bubble lives, because only they know which switch on the DR side has no
	// path to production.
	TestIsolation *TestIsolationSpec `json:"testIsolation,omitempty"`
}

// RunbookGroup is one tier of the recovery: a set of VMs that may come up
// together, and the condition that must hold before the next tier starts.
type RunbookGroup struct {
	// Name is what the operator sees in the run view, e.g. "Directory and DNS".
	Name string `json:"name"`

	// Members are brought up in parallel. Their order within the slice carries no
	// meaning; if two of them must be sequenced, they belong in different groups.
	Members []RunbookMember `json:"members,omitempty"`

	// Gate is what must be true of every member before the next group starts.
	// The zero value (Kind empty) means the group is gated only on its failover
	// jobs succeeding — the VM was created and started, nothing more is asserted.
	Gate HealthCheckConfig `json:"gate,omitempty"`

	// TimeoutSeconds is the whole group's budget: failover plus gate, for every
	// member. Exceeding it PAUSES the run naming the group and the members still
	// outstanding, rather than proceeding to the next group. Zero uses
	// DefaultGroupTimeoutSeconds.
	//
	// It pauses rather than fails because the usual cause is a guest taking
	// longer to boot than expected, and a paused run can be resumed once the
	// operator has looked. A failed one has already given up on the tiers below
	// it, which during a real disaster is the expensive mistake.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	// ContinueOnMemberFailure lets the group proceed to its gate — and the run to
	// the next group — even though one member never came up. Off by default: in
	// a tier whose whole purpose is to be a prerequisite, a missing member
	// usually means the tiers below it will fail in a more confusing way.
	//
	// Turn it on for a group of peers where losing one is survivable — two DNS
	// servers, a web farm — and the gate then proves the tier is serving on
	// whatever did come up.
	ContinueOnMemberFailure bool `json:"continueOnMemberFailure,omitempty"`
}

// RunbookMember is one VM's place in a group.
type RunbookMember struct {
	// VMName is the VM as the centre knows it. It must have replication
	// configured; a member that does not is reported when the run is created,
	// not discovered half way down the plan.
	VMName string `json:"vmName"`

	// Network and Dvport say what the VM's adapters connect to ON THE DR SIDE.
	// The switch the VM used at the primary usually does not exist there, and a
	// failover that leaves the NIC unconnected produces a VM that is running and
	// unreachable — which reads as a successful failover.
	//
	// Dvport is preferred: it carries switch and VLAN together and is what the
	// desired-state flip binds the NIC to. Network is the raw switch name for a
	// target with no dvport declared. In test mode both are ignored and the
	// bubble's switch is used instead, which is the point of the bubble.
	Network string `json:"network,omitempty"`
	Dvport  string `json:"dvport,omitempty"`
	VLANID  int    `json:"vlanId,omitempty"`

	// RecoveryPoint names a specific Hyper-V Replica recovery point for an
	// unplanned failover. Empty means the latest received data, which is what an
	// unplanned failover almost always wants. Ignored in planned and test modes.
	RecoveryPoint string `json:"recoveryPoint,omitempty"`

	// GuestNetwork re-addresses the guest after it boots, for a DR site on a
	// different subnet. Nil leaves the guest's own configuration alone — correct
	// for a stretched subnet, and correct whenever DHCP covers it.
	GuestNetwork *RunbookGuestNetwork `json:"guestNetwork,omitempty"`
}

// RunbookGuestNetwork re-addresses a guest at the DR site.
//
// This is done with PowerShell Direct (the existing GuestSetIP job), not with a
// KVP write. KVP delivers a key/value pair over the integration services and
// stops there: nothing in a stock Windows or Linux guest reads it and acts on
// it, so a KVP-based "IP injection" reports success while the guest keeps the
// address it booted with. PowerShell Direct runs inside the guest over VMBus —
// no network required, which is exactly the situation here, since the guest's
// network is the thing being fixed.
//
// The cost is that it needs credentials for the guest and an integration
// services channel, so it does not work on a guest without PowerShell Direct
// support (Linux, and Windows older than 2016). For those the run says so
// plainly and names the one step — set the address in the guest — rather than
// reporting a success the guest never received.
type RunbookGuestNetwork struct {
	// InterfaceAlias names the adapter inside the guest. Empty picks the first
	// connected adapter, which is right for the single-NIC VMs this mostly
	// applies to.
	InterfaceAlias string `json:"interfaceAlias,omitempty"`

	// Address is CIDR, e.g. "10.20.5.14/24".
	Address string `json:"address"`
	Gateway string `json:"gateway,omitempty"`
	// DNSServers are applied in order.
	DNSServers []string `json:"dnsServers,omitempty"`

	// CredentialSecret names a vault secret with "username" and "password" for a
	// local administrator in the guest.
	CredentialSecret string `json:"credentialSecret,omitempty"`
}

// HealthCheckConfig is a group's gate: the evidence the runbook requires before
// it believes a tier is actually serving.
//
// "The failover job succeeded" is not that evidence. It means Hyper-V created
// and started a VM. A domain controller whose VM is running but whose directory
// service has not finished starting will fail every join the next tier
// attempts, and the run would already have moved on. The gate is where a
// runbook stops being a batch script.
type HealthCheckConfig struct {
	// Kind selects the probe. Empty means no gate beyond the failover succeeding.
	Kind HealthCheckKind `json:"kind,omitempty"`

	// Address overrides where TCP and ICMP probe. Empty uses the address the
	// guest reports through integration services — which is the right default,
	// because after a cross-subnet failover the VM's address is not the one the
	// runbook was authored against.
	Address string `json:"address,omitempty"`

	// Port is the TCP port for HealthCheckTCP: 389 for a domain controller, 1433
	// for SQL, 443 for a web tier.
	Port int `json:"port,omitempty"`

	// ScriptPath is a PowerShell script ON THE AGENT'S HOST for
	// HealthCheckScript, run by the agent at the DR site. It is deliberately not
	// an inline script body: a runbook is authored once and run under pressure,
	// and a script that lives on the host can be reviewed, signed and tested
	// where it will run.
	ScriptPath string `json:"scriptPath,omitempty"`
	// ExpectedExitCode is the exit code that counts as healthy. Zero is the
	// default and the usual answer.
	ExpectedExitCode int `json:"expectedExitCode,omitempty"`

	// IntervalSeconds is how often the probe is retried while it is failing.
	// Zero uses DefaultGateIntervalSeconds. Each attempt is a short, one-shot
	// agent job rather than a long-running poll, so a centre restart mid-gate
	// resumes the gate instead of stranding it.
	IntervalSeconds int `json:"intervalSeconds,omitempty"`

	// ConsecutiveSuccesses is how many probes in a row must pass. Zero means one.
	// Raise it for a service that answers on its port before it is ready to serve
	// — a SQL instance accepts connections during recovery and then refuses the
	// queries the next tier makes.
	ConsecutiveSuccesses int `json:"consecutiveSuccesses,omitempty"`
}

// HealthCheckKind names a probe.
type HealthCheckKind string

const (
	// HealthCheckNone gates only on the failover job succeeding.
	HealthCheckNone HealthCheckKind = ""
	// HealthCheckHeartbeat waits for the Hyper-V integration services heartbeat
	// to report OK — the guest OS has booted far enough to talk to the host. It
	// needs no guest network at all, which makes it the only gate that works
	// before a cross-subnet re-address has happened.
	HealthCheckHeartbeat HealthCheckKind = "Heartbeat"
	// HealthCheckTCP connects to Address:Port from the agent at the DR site.
	HealthCheckTCP HealthCheckKind = "TCP"
	// HealthCheckICMP pings Address from the agent at the DR site. Weakest of the
	// gates: a machine answers ping long before it serves anything.
	HealthCheckICMP HealthCheckKind = "ICMP"
	// HealthCheckScript runs ScriptPath on the agent and compares its exit code.
	HealthCheckScript HealthCheckKind = "Script"
)

// Valid reports whether the gate is usable as authored, and why not if it is
// not. Checked when a run is created, so a plan with a TCP gate and no port is
// refused up front rather than probing port 0 for twenty minutes.
func (h HealthCheckConfig) Valid() (bool, string) {
	switch h.Kind {
	case HealthCheckNone, HealthCheckHeartbeat, HealthCheckICMP:
		return true, ""
	case HealthCheckTCP:
		if h.Port <= 0 || h.Port > 65535 {
			return false, "a TCP gate needs a port"
		}
		return true, ""
	case HealthCheckScript:
		if h.ScriptPath == "" {
			return false, "a script gate needs the path of a script on the host"
		}
		return true, ""
	}
	return false, "unknown health check " + string(h.Kind)
}

// TestIsolationSpec describes the network bubble a test failover runs inside: a
// private vSwitch on the DR host with no uplink and no management OS adapter,
// so a test VM can boot with the same IP and MAC as the production machine it
// is a copy of and reach nothing.
//
// Hyper-V calls this switch type Private. There is no VMware-style port group
// here and none is invented: the switch IS the isolation, and a VLAN on a
// connected switch is not — a mis-typed VLAN on an external switch puts a
// duplicate domain controller on the production network.
type TestIsolationSpec struct {
	// SwitchName is the private switch the agent ensures exists on the DR host
	// before the test VMs are attached to it. Created if absent; left alone if
	// present and already private. If a switch of that name exists and is NOT
	// private the run refuses rather than reconfiguring it — an external switch
	// with that name is somebody's production networking.
	SwitchName string `json:"switchName"`

	// Kind is how tight the bubble is, and it is a genuine trade-off rather than
	// a preference. Empty means Private.
	//
	//   Private  — no uplink and no management OS adapter. Nothing outside the
	//              switch can reach the test VMs and they can reach nothing.
	//              This is the safe answer, and it means a TCP or ICMP gate
	//              CANNOT PASS: the agent probing from the host has no path to
	//              the VM. Only Heartbeat and Script gates work in a private
	//              bubble. A run authored with a private bubble and a TCP gate is
	//              refused when it is created, not left to time out.
	//   Internal — no uplink, but the host gets an adapter on the switch, so the
	//              agent can probe the test VMs and a TCP gate becomes possible.
	//              The cost is real: the test VMs keep the production addresses
	//              they were replicated with, so the host now has an interface in
	//              a subnet it already routes to. Choose it when the gate matters
	//              more than that, and not by default.
	//
	// There is no External option. An external switch is not isolation, and
	// offering it here would make the field a way to accidentally boot a second
	// domain controller onto the production network.
	Kind TestIsolationKind `json:"kind,omitempty"`

	// RemoveOnTeardown deletes the switch when the test ends. Off by default: the
	// switch costs nothing to keep, and a bubble torn down and recreated on every
	// test is a bubble whose configuration nobody has ever reviewed.
	RemoveOnTeardown bool `json:"removeOnTeardown,omitempty"`
}

// TestIsolationKind is how tight a test bubble is.
type TestIsolationKind string

const (
	// TestIsolationPrivate is the default and the safe one.
	TestIsolationPrivate TestIsolationKind = ""
	// TestIsolationInternal lets the host — and only the host — reach the bubble.
	TestIsolationInternal TestIsolationKind = "Internal"
)

// SwitchType returns the Hyper-V switch type this bubble needs.
func (t TestIsolationSpec) SwitchType() string {
	if t.Kind == TestIsolationInternal {
		return "Internal"
	}
	return "Private"
}

// Reachable reports whether an agent on the host can reach VMs inside this
// bubble over the network — which is what a TCP or ICMP gate requires.
func (t TestIsolationSpec) Reachable() bool { return t.Kind == TestIsolationInternal }

// Runbook execution modes.
const (
	// RunbookModeTest builds a temporary "<vm> - Test" copy from the latest
	// recovery point on an isolated switch and starts it. The primary keeps
	// running and keeps replicating throughout, so this is safe at any time —
	// and it is the only one of the three that is. It ends with a teardown.
	RunbookModeTest = "Test"
	// RunbookModePlanned gracefully stops each source VM, flushes its final
	// delta so nothing is lost, brings the replica up as primary, and reverses
	// replication so the old primary becomes the new replica. It requires the
	// source side to be reachable — it is a migration, not a recovery.
	RunbookModePlanned = "Planned"
	// RunbookModeUnplanned assumes the primary is gone and NEVER CONTACTS IT.
	// That is the defining property, not an optimisation: a call to a dead host
	// does not fail, it hangs until it times out, and a recovery that stops to
	// ask a dead cluster a question it cannot answer is how a five-minute
	// failover becomes a thirty-minute one. Replication is left broken until
	// somebody reverses it once the primary is back.
	RunbookModeUnplanned = "Unplanned"
)

// ValidRunbookMode reports whether mode is one of the three.
func ValidRunbookMode(mode string) bool {
	switch mode {
	case RunbookModeTest, RunbookModePlanned, RunbookModeUnplanned:
		return true
	}
	return false
}

// Runbook run defaults. Each is generous: the cost of being too tight is a run
// that pauses on a fleet that was merely busy, in the middle of a disaster.
const (
	// DefaultGroupTimeoutSeconds is one group's whole budget — failover and gate,
	// for every member — when the group does not set its own.
	DefaultGroupTimeoutSeconds = 20 * 60
	// DefaultGateIntervalSeconds is how often a failing gate is re-probed.
	DefaultGateIntervalSeconds = 15
)

// RunbookRun is one execution of a runbook, in one mode.
//
// The plan is COPIED into the run at creation rather than referenced. A run is
// the record of what was actually done, and a runbook edited during or after a
// run must not rewrite the history of it — nor change what a paused run does
// when it resumes. The runbook name is kept for provenance only.
//
// A run is durable and holds all of its own progress, so a centre restart
// resumes a half-finished recovery rather than starting it again — which would
// mean failing over the tier that had already come up.
type RunbookRun struct {
	// ID is assigned by the centre. Runs are history and are never keyed by
	// runbook name.
	ID string `json:"id"`
	// RunbookName is provenance. The runbook may since have been edited or
	// deleted; the plan this run walks is the copy in Groups.
	RunbookName string `json:"runbookName,omitempty"`

	// Mode is one of the RunbookMode constants and is fixed for the run's life.
	Mode string `json:"mode"`

	State   string `json:"state"`
	Message string `json:"message,omitempty"`

	// Groups is the plan as copied at creation, each carrying its own progress.
	Groups []RunbookGroupRun `json:"groups,omitempty"`

	// TestIsolation is the bubble this run used, copied from the runbook. Kept on
	// the run so a teardown removes what was actually built, even if the runbook
	// has since been edited to name a different switch.
	TestIsolation *TestIsolationSpec `json:"testIsolation,omitempty"`

	// PreTeardownState is the state the run reached before a teardown began, so
	// ending the test restores it rather than inventing an outcome. A test that
	// ran cleanly goes back to Succeeded; one that was paused half way goes back
	// to Paused, because tidying up the copies did not fix what stopped it.
	PreTeardownState string `json:"preTeardownState,omitempty"`

	// TestActive is true once test VMs exist and until the teardown has removed
	// them. A test run reaching Succeeded is NOT finished: the copies are still
	// running on the DR host, consuming its memory and its disk, and the console
	// must keep saying so until somebody ends the test. A test quietly left up is
	// how a DR host runs out of storage a fortnight later.
	TestActive bool `json:"testActive,omitempty"`

	CreatedBy string    `json:"createdBy,omitempty"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
}

// Runbook run states. Deliberately the same vocabulary as ClusterUpdateRun: an
// operator should not have to learn two words for a paused orchestration.
const (
	RunbookPending = "Pending"
	RunbookRunning = "Running"
	// RunbookPaused stopped because something was not safe to proceed through — a
	// group that ran out of budget, a member that failed in a group that does not
	// tolerate it. It HOLDS rather than skipping ahead, and Message says why.
	// Resuming is an operator decision.
	RunbookPaused = "Paused"
	// RunbookTearingDown is a test run removing its test VMs and its bubble.
	RunbookTearingDown = "TearingDown"
	RunbookSucceeded   = "Succeeded"
	RunbookFailed      = "Failed"
	RunbookCancelled   = "Cancelled"
)

// RunbookGroupRun is one group's progress.
type RunbookGroupRun struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`

	Members []RunbookMemberRun `json:"members,omitempty"`

	Gate                    HealthCheckConfig `json:"gate,omitempty"`
	TimeoutSeconds          int               `json:"timeoutSeconds,omitempty"`
	ContinueOnMemberFailure bool              `json:"continueOnMemberFailure,omitempty"`

	StartedAt time.Time `json:"startedAt,omitempty"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
}

// Group states.
const (
	RunbookGroupPending = "Pending"
	// RunbookGroupFailingOver has every member's failover job outstanding.
	RunbookGroupFailingOver = "FailingOver"
	// RunbookGroupAddressing is re-addressing guests, between failover and gate.
	RunbookGroupAddressing = "Addressing"
	// RunbookGroupGating is the health gate being probed.
	RunbookGroupGating = "Gating"
	RunbookGroupDone   = "Done"
	RunbookGroupFailed = "Failed"
)

// RunbookMemberRun is one VM's progress within a group.
type RunbookMemberRun struct {
	VMName string `json:"vmName"`
	State  string `json:"state"`
	// Message is what this member is waiting on, or why it failed, in the
	// operator's terms rather than the controller's.
	Message string `json:"message,omitempty"`

	// Plan, copied from the runbook so the run is self-contained.
	Network       string               `json:"network,omitempty"`
	Dvport        string               `json:"dvport,omitempty"`
	VLANID        int                  `json:"vlanId,omitempty"`
	RecoveryPoint string               `json:"recoveryPoint,omitempty"`
	GuestNetwork  *RunbookGuestNetwork `json:"guestNetwork,omitempty"`

	// TargetHost is the host that holds the replica and therefore runs every job
	// for this member. Resolved once, when the member's turn begins, and then
	// kept: the run must send its follow-up jobs to the host that actually did
	// the failover, not to wherever the resolution would point later.
	TargetHost string `json:"targetHost,omitempty"`

	// SourceHost is where the VM was running before a planned failover, recorded
	// so the run can say what it stopped. Never populated in unplanned mode,
	// which does not contact the source at all.
	SourceHost string `json:"sourceHost,omitempty"`

	// JobID is the job currently outstanding for this member — the failover, the
	// re-address, or the latest gate probe. The console links to it so an
	// operator can read the agent's own error rather than a summary of it.
	JobID string `json:"jobId,omitempty"`

	// GateSuccesses counts consecutive passing probes, for
	// HealthCheckConfig.ConsecutiveSuccesses.
	GateSuccesses int `json:"gateSuccesses,omitempty"`
	// LastProbeAt paces re-probing without needing a timer the centre would lose
	// on restart.
	LastProbeAt time.Time `json:"lastProbeAt,omitempty"`

	StartedAt time.Time `json:"startedAt,omitempty"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
}

// Per-member states, in the order a member passes through them.
const (
	RunbookMemberPending = "Pending"
	// RunbookMemberPreparing is the isolated switch being ensured on the DR host,
	// before a test failover attaches anything to it. Only test runs use it: for
	// planned and unplanned the network already exists.
	RunbookMemberPreparing = "Preparing"
	// RunbookMemberFailingOver has the failover job outstanding on the DR host.
	RunbookMemberFailingOver = "FailingOver"
	// RunbookMemberAddressing has the guest re-address job outstanding.
	RunbookMemberAddressing = "Addressing"
	// RunbookMemberGating has the member's health gate being probed.
	RunbookMemberGating = "Gating"
	RunbookMemberDone   = "Done"
	RunbookMemberFailed = "Failed"

	// Teardown states, reached only by a test run being ended. They are member
	// states rather than a run-level counter so the console can show WHICH copy
	// is refusing to go away — the one thing an operator needs when a DR host is
	// filling up.
	RunbookMemberTearingDown = "TearingDown"
	RunbookMemberTornDown    = "TornDown"
)

// Active reports whether the run is still the controller's business.
func (r RunbookRun) Active() bool {
	switch r.State {
	case RunbookPending, RunbookRunning, RunbookTearingDown:
		return true
	}
	return false
}

// Terminal reports whether the run has finished, one way or another.
//
// A succeeded TEST run is terminal as an orchestration and still has test VMs
// running; TestActive, not this, is what tells the console there is cleanup
// outstanding.
func (r RunbookRun) Terminal() bool {
	switch r.State {
	case RunbookSucceeded, RunbookFailed, RunbookCancelled:
		return true
	}
	return false
}

// CurrentGroup returns the group the run is working on, and whether there is
// one. Groups are strictly sequential, so this is the first one not finished.
func (r RunbookRun) CurrentGroup() (int, bool) {
	for i, g := range r.Groups {
		switch g.State {
		case RunbookGroupDone, RunbookGroupFailed:
			continue
		}
		return i, true
	}
	return 0, false
}

// Budget returns the group's timeout, applying the default.
func (g RunbookGroupRun) Budget() time.Duration {
	if g.TimeoutSeconds > 0 {
		return time.Duration(g.TimeoutSeconds) * time.Second
	}
	return DefaultGroupTimeoutSeconds * time.Second
}

// Done reports whether a member has finished, successfully or not.
func (m RunbookMemberRun) Done() bool {
	return m.State == RunbookMemberDone || m.State == RunbookMemberFailed
}

// ProbeInterval returns the gate's re-probe cadence, applying the default.
func (h HealthCheckConfig) ProbeInterval() time.Duration {
	if h.IntervalSeconds > 0 {
		return time.Duration(h.IntervalSeconds) * time.Second
	}
	return DefaultGateIntervalSeconds * time.Second
}

// Needed reports whether this gate asserts anything beyond the failover having
// succeeded.
func (h HealthCheckConfig) Needed() bool { return h.Kind != HealthCheckNone }

// RequiredSuccesses returns how many consecutive passes the gate needs,
// applying the default of one.
func (h HealthCheckConfig) RequiredSuccesses() int {
	if h.ConsecutiveSuccesses > 1 {
		return h.ConsecutiveSuccesses
	}
	return 1
}

// TestVMName is what Hyper-V names the throwaway copy a test failover creates.
// The agent's TestFailover builds "<vm> - Test"; every follow-up the run makes
// against a test member — the health gate, the console link — must address that
// name and not the replica's, or it probes a VM that is switched off.
func TestVMName(vmName string) string { return vmName + " - Test" }
