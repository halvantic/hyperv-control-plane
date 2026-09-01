package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/agent/reconcile"
	"github.com/joshua-fourie/ballast/agent/store"
	ballastpb "github.com/joshua-fourie/ballast/api/proto"
	"github.com/joshua-fourie/ballast/api/types"
	"github.com/joshua-fourie/ballast/version"
)

// jobTimeout caps a single imperative job. A job that exceeds it is cancelled
// and reported Failed, so a stuck host operation can never wedge the agent.
const jobTimeout = 10 * time.Minute

/*
jobTimeoutFor returns the cap for a job. Most finish in seconds, but storage

	rebuilds/repairs and live migrations legitimately run for many minutes (disk
	rebalance, VM+storage copy), so they get a longer cap rather than being
	killed mid-operation and left in a half-applied state.

	It takes the JOB, not just the kind, because one kind is not one duration. A
	template capture that only copies a disk and one that runs sysprep first are
	the same job kind and an hour apart: sysprep shuts the guest down on its own
	schedule and nothing can hurry it.

	That distinction is why generalising captures failed. The capture script
	waits SysprepDeadline for the guest to stop; this function did not recognise
	the kind at all and gave it the ten-minute default. Sysprep ran, the guest
	shut down exactly as promised, and the job was then cancelled and reported
	Failed part-way through with no template produced. The budget now comes from
	hyperv.CaptureBudget so the two cannot disagree again.
*/
func jobTimeoutFor(job types.Job) time.Duration {
	switch job.Kind {
	case types.JobVMCaptureTemplate:
		return hyperv.CaptureBudget(job.Params["generalise"] == "true")
	case types.JobRebuildPool, types.JobRepairPool, types.JobClusterMoveVM, types.JobFetchISO, types.JobVMExport:
		return 30 * time.Minute
	case types.JobMigrateVM, types.JobCopyVM:
		/* Both of these copy a VM's ENTIRE STORAGE across a network link, so
		   their duration is set by disk size and link speed and by nothing on
		   this host. Half a terabyte over a gigabit link is an hour and a quarter
		   at the theoretical rate, and evacuations are exactly when a link is
		   busiest.

		   CopyVM was not listed here at all and took the ten-minute default,
		   which is the same mistake the capture comment above describes: a kind
		   the function did not recognise, killed part-way through. Observed
		   2026-09-01 copying HVNew01 to HVNEW06. MigrateVM's thirty minutes was
		   the same bug one step less obvious — enough for a small VM, and short
		   for the ones that most need moving.

		   Cutting either short wastes every byte already copied: the export or
		   the move restarts from nothing, and CopyVM additionally leaves a part
		   file on the destination that blocks the retry until somebody removes
		   it. A copy that is genuinely stuck shows as progress that has stopped
		   moving, which is a signal an operator can act on long before this. */
		return hyperv.CopyBudget
	case types.JobMigrationPass:
		/* A VMware copy pass is bounded by somebody else's disk and somebody
		   else's link, not by anything on this host. A terabyte at a plausible
		   200MB/s is an hour and a half; the same terabyte over a saturated
		   1Gb/s is nearer three.

		   Twelve hours, then. Deliberately generous, because cutting a base copy
		   short at hour four wastes every byte of it — the disk is dismounted
		   mid-write and the retry starts again from nothing. A pass that is
		   genuinely stuck shows as no progress in the console long before this,
		   which is the signal an operator can act on. */
		return 12 * time.Hour
	case types.JobMigrationCleanup:
		// Removing a snapshot means consolidating it, which is real I/O on the
		// source datastore. It is also the job that must not be abandoned: a
		// snapshot left behind grows until somebody else's datastore fills.
		return 30 * time.Minute
	default:
		return jobTimeout
	}
}

// jobReportTimeout bounds the report of a job's TERMINAL state, which is sent on
// its own context rather than the job's — see the failure path in runJobs.
const jobReportTimeout = 30 * time.Second

/*
describeJobFailure says what actually happened when a job ends.

	A job killed by its budget reports Go's own "context deadline exceeded",
	which names neither the budget nor the operation and reads as an internal
	error rather than as a long job that was cut short. For a capture that is
	actively misleading: sysprep ran, the guest shut down as asked, and the
	operator is shown a phrase that suggests nothing happened at all.
*/
func describeJobFailure(job types.Job, jctx context.Context, err error) string {
	if jctx.Err() == nil || !errors.Is(jctx.Err(), context.DeadlineExceeded) {
		return err.Error()
	}
	budget := jobTimeoutFor(job)
	return fmt.Sprintf("%s ran longer than the %v this agent allows for it and was stopped. "+
		"Anything it had already done on the host stands — check the host before running it again. (%v)",
		job.Kind, budget, err)
}

// fullResyncEvery forces a full desired-state pull (ignoring the cached
// generation) every N cycles, so the agent self-heals from a reused/colliding
// generation rather than trusting an "unchanged" answer indefinitely.
const fullResyncEvery = 20

// journalPruneEvery is how often the status journal is tidied. It is pure
// housekeeping on a background goroutine — never on the startup path, where it
// once cost a host its agent, and never on the status-delivery path, where the
// scan it replaced was timing out reports.
const journalPruneEvery = 15 * time.Minute

// The reconcile cycle is bounded so a blocked PowerShell call (an unresponsive
// cluster/CSV query, a wedged Hyper-V op) can never hang the agent forever:
// execPowerShell uses exec.CommandContext, so the deadline terminates the
// process. Only the WHOLE cycle is bounded, not each operation — a host reconcile
// is a sequence of many cmdlet calls (per-switch, per-vNIC, prune, DNS) that on a
// busy cluster member legitimately takes well over a minute, so bounding a single
// operation would kill working work mid-sequence and flag the host Degraded.
// Jobs and liveness run on their own goroutines, so a slow cycle never delays an
// operator action or makes the host look offline.
const (
	// cycleTimeout caps an entire reconcile cycle — a generous backstop that lets a
	// legitimately slow pass complete while guaranteeing the loop always regains
	// control and retries (a true wedge recovers within this window).
	cycleTimeout = 5 * time.Minute
	// collectTimeout caps a single best-effort read (e.g. a console screen capture)
	// so a wedged capture cannot eat the cycle budget.
	collectTimeout = 45 * time.Second
	// observeBudget caps each of the CACHED observations — metrics, inventory,
	// resources — for the same reason, one tier up.
	//
	// These are best-effort reads whose values are already carried forward
	// between refreshes, so the cost of abandoning one is a stale reading. They
	// were handed the whole cycle context, so the cost of NOT abandoning one was
	// the entire pass: on bcluster2 2026-08-14, with the cluster's Health Service
	// down and the pool degraded, CollectInventory took 4m58s of a 5m0s cycle on
	// HVNEW02 and 2m44s on HVNEW01. Those cycles were killed at the cap before
	// reaching reportStatus, so the hosts' readings froze at fifteen minutes old
	// and every reconcile step after the stall reported NotAttempted. One sick
	// subsystem blinded the centre to three whole hosts.
	//
	// It looks host-local and is not: CollectInventory runs Get-PhysicalDisk,
	// which on an S2D node enumerates every disk in the CLUSTER, and
	// Get-ClusterResource for the cluster IPs. Both go through the clustered
	// storage subsystem, which is exactly what fails when a cluster is sick.
	//
	// 90s rather than collectTimeout's 45s: a legitimately slow S2D inventory on
	// a busy node is not a fault, and three of these plus a reconcile still fit
	// inside the cycle with room to report.
	observeBudget = 90 * time.Second
)

// bounded runs a cached observation under its own deadline, so one that hangs
// costs a stale reading rather than the pass. Returns the zero value and the
// context error when the budget expires; every caller keeps its cached value on
// error, which is what makes abandoning it safe.
func bounded[T any](ctx context.Context, budget time.Duration, f func(context.Context) (T, error)) (T, error) {
	c, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return f(c)
}

// Observation refresh cadences, tiered by how often each class of state actually
// changes so a 15s cycle is not dominated by rescanning things that rarely move.
// Each observation is cached and reused between refreshes; the reported status
// still carries the last-collected value every cycle, so the centre is never
// missing data — only the RE-collection is throttled. A job completion sets
// forceScan (via requestNudge) to refresh the operationally-live classes at once.
const (
	// observeVMsEvery — the VM inventory (roles, power, replication mode) is the
	// operationally-live view an operator watches during power/failover ops, so it
	// refreshes often. It is moderately heavy (Get-VM + per-VM replication), hence
	// not every single cycle. ~30s at a 15s heartbeat. This is the failover-lag fix:
	// only the host that runs a failover job gets a forceScan nudge, so the OTHER
	// side (former primary → replica) relied on this cadence to show its new role.
	observeVMsEvery = 2
	// vmRemovedHold — how long the reconciler refuses to recreate a VM a RemoveVM
	// job has just deleted. It has to outlast a full reconcile pass (1–2 minutes
	// on a busy host), because that pass is what would otherwise recreate the VM
	// from a cached set loaded before the deletion. Nothing is lost by holding: if
	// the centre still wants the VM, the next pull says so and the hold is dropped
	// at once.
	vmRemovedHold = 5 * time.Minute
	// netProfileRefreshEvery — the network-location query (weakest connection-profile
	// category) changes only when domain reachability does. ~60s.
	netProfileRefreshEvery = 4
	// resourceRefreshEvery — switch details, volumes, ISO scan, vNIC topology. Heavy
	// (the ISO scan walks the filesystem) and changes rarely. ~120s.
	resourceRefreshEvery = 8
	// inventoryRefreshEvery — physical hardware (adapters, disks, CPU, memory). Does
	// not change without a reboot or a hardware event, so collecting it every 15s was
	// pure waste; ~120s is ample. (Also collected on demand before registration.)
	inventoryRefreshEvery = 8
	// identityRefreshEvery — computer name and AD domain. Effectively immutable after
	// a host is joined, so refresh it only occasionally. ~5min.
	identityRefreshEvery = 20
	// clusterReconcileEvery — the cluster-wide reconcile (the Failover-Clustering
	// feature, S2D/CSV provisioning, the Replica Broker, migration delegation) is the
	// most expensive and most hang-prone pass, yet cluster desired state changes very
	// rarely. So it is edge-triggered — run at once when the cluster's Generation
	// changes (an operator edit) or after a job (forceScan) — and otherwise only on
	// this slow idle sweep (~2min) as a drift/retry/post-reboot safety net. Failover
	// Clustering owns availability and quorum between sweeps regardless. The centre
	// keeps the last reported cluster status between passes, so this does not blank
	// the UI; the one thing it can lag is an *unplanned* failover's owner node (a
	// planned move goes through a job, which forces an immediate pass).
	clusterReconcileEvery = 8
	// isoLibraryEvery — the boot-media share probe. It reaches off the host to a
	// file server and its computer-account half runs a one-shot scheduled task
	// that can wait up to 45s, so it is the least appropriate thing to do on
	// every pass. A share's contents and its permissions change on human
	// timescales; the centre carries the last result forward between probes, so a
	// skipped pass shows the previous answer rather than blanking the library.
	isoLibraryEvery = 20
	// windowsLicenceEvery — the Windows edition and activation read. The slowest
	// of the lot, and rightly: an edition changes once in a host's life and an
	// activation state on the order of months. Reading it every pass would spend a
	// PowerShell invocation to re-learn a 120-day countdown that moves by a day at
	// a time. The last value is carried forward between reads, so the console is
	// never blank between them.
	windowsLicenceEvery = 60
	// screenCaptureEvery — the VM console thumbnail. Per RUNNING VM per cycle, and
	// the least urgent thing collected: a preview of a screen nobody may be
	// looking at, on a path the live console does not use. On a host with many
	// VMs it was a PowerShell invocation each, every pass.
	//
	// 4 rather than 8 because a cycle is not the 15s heartbeat in practice — the
	// rig measures ~45s — so 8 would leave a thumbnail six minutes stale. This
	// still skips three captures in four. The centre carries the last screen
	// forward across the skipped passes; without that the console 404s on the
	// cycles that do not capture, which is how this was first found.
	screenCaptureEvery = 4
	// vmFullReadEvery — the drift sweep for VM configuration. The full per-VM
	// read (guest OS via KVP/XML, checkpoints, a Get-VHD per disk, a VLAN query
	// per adapter) is ~1.4s per VM, and processor count, memory config,
	// generation, disks and adapters settle on a power cycle or an explicit job
	// rather than changing on their own. The reconciler already re-reads a VM
	// whose power state moved, and a job or a desired-state change forces one.
	//
	// This is the safety net for the case none of those cover: an edit made
	// outside Ballast — Hyper-V Manager on the host, a script — to a VM nobody
	// ever reboots. Without it that drift is invisible for ever, which is the
	// autonomy guarantee, not a nicety. At 1 pass in 8 it costs almost nothing.
	vmFullReadEvery = 8
)

// runnerConfig is the agent's runtime configuration, independent of how the
// process was started (Windows service or console).
type runnerConfig struct {
	centreAddr string
	hostName   string
	storePath  string
	heartbeat  time.Duration
	// tlsDir holds the CA + this agent's client cert/key for mutual TLS to the
	// centre, provisioned at onboard. Empty connects insecure.
	tlsDir string
}

// runner is the OS-independent agent loop: it adopts its last-honoured desired
// state from the local store, then on each cycle (best-effort) registers with
// the centre, pulls and caches desired state, and journals status. It
// deliberately does NOT yet reconcile actual host state towards desired — that
// loop is the next step and slots in at the marked point below.
//
// Reaching the centre is never a precondition for the agent running. If the
// centre is unreachable — at first boot after a host restart, or mid-run — the
// agent keeps enforcing its cached desired state, marks itself Autonomous, and
// queues status locally until the centre returns. This is the product thesis;
// nothing in the loop may block on the centre.
type runner struct {
	cfg        runnerConfig
	log        *slog.Logger
	hv         hyperv.Interface
	st         *store.Store
	reconciler *reconcile.Reconciler

	// uid is this host's centre-assigned identity. It is adopted from cached
	// desired state on boot and refreshed by a successful registration, so the
	// agent has an identity to report under even before it can reach the centre.
	uid string

	// cycles counts heartbeat cycles, used to schedule a periodic full resync.
	cycles int

	// lastClusterGen is the cluster Generation the last cluster reconcile ran
	// against, so a change (an operator edit) edge-triggers an immediate pass
	// between the slow idle sweeps (see clusterReconcileEvery).
	lastClusterGen int64

	// lastMaintenance is whether the node was OBSERVED out of service last cycle,
	// so pausing or resuming edge-triggers an immediate cluster pass. A drain
	// moves the node's roles and takes its disks out of the pool at once, which
	// changes most of what cluster status reports — and waiting for the slow idle
	// sweep left the console describing the cluster as it was up to ten minutes
	// earlier, with alarms drawn on a snapshot that predated the drain.
	//
	// This is a scheduling hint ONLY, never intent: losing it across a restart
	// costs one extra cluster pass and nothing else. Maintenance intent itself
	// lives in desired state and is re-derived every cycle, which is what stops a
	// remembered "wanted" from stranding a node paused for ever — the bug that
	// removing the old in-memory lastWantedMaintenance fixed. Do not grow this
	// into a decision input.
	lastMaintenance bool

	// registered is true once a registration round-trip has been confirmed.
	// Until then each cycle retries registration; the retry never blocks the
	// loop, so reconcile and status journalling proceed regardless.
	registered bool

	// observedGen is the last desired Generation the agent fully honoured. It
	// persists across cycles and across autonomy windows, and is what the agent
	// reports as Status.ObservedGeneration.
	observedGen int64

	// isoLib is the ISO library that currently applies to this host — the
	// cluster's for a member, its own for a standalone host, resolved by
	// reconcile.EffectiveISOLibrary and refreshed only on a successful pull.
	isoLib *types.ISOLibrarySpec
	// isoLibState is the last probe result, carried forward across the cycles
	// that skip the probe so a skipped pass shows the previous answer rather than
	// blanking the library in the console.
	isoLibState *types.ISOLibraryStatus

	// licenceState is the last Windows edition/activation read, carried forward
	// between the slow refreshes so the console always has the previous answer
	// rather than a gap.
	licenceState *types.WindowsLicenceStatus
	// nodeSelf is this host's own view of its cluster membership, refreshed each
	// pass. Held here because it is read once and used in the status build.
	nodeSelf hyperv.NodeSelf

	// clusterISCSI is this member's own iSCSI view from the last cluster
	// reconcile, carried forward between cluster passes (which run on a slower
	// cadence than the status report) so every report carries it.
	clusterISCSI *types.ISCSIStatus

	// vmObservedGen tracks, per VM name, the last desired Generation fully
	// honoured for that VM. Like observedGen it persists across cycles and
	// autonomy windows. Lazily initialised.
	vmObservedGen map[string]int64

	// jobsInflight dedups jobs that are executing in the background so a later
	// cycle (which still sees them Pending until the agent reports Running) does
	// not launch them twice. Guarded by jobsMu.
	jobsMu       sync.Mutex
	jobsInflight map[string]struct{}
	// jobsVMs counts the in-flight jobs holding each VM, by lower-cased VM name,
	// so the reconciler can stand off a VM being moved, cloned or captured rather
	// than fighting the job and reporting Degraded throughout. A count, not a
	// flag: two jobs can legitimately name the same VM.
	jobsVMs map[string]string
	// vmRemoved holds, by lower-cased VM name, when a RemoveVM job successfully
	// deleted a VM from this host. The reconciler stands off those names for
	// vmRemovedHold, because clearing the hold the moment the job returns is not
	// enough: a pass loads the cached desired set once and can reach a given VM a
	// minute or more later, still holding a name the job has since deleted, and
	// then recreates it. Guarded by jobsMu.
	//
	// The durable half of the same fix is store.DropDesiredVM; this covers only
	// the window in which a pass already in flight holds the name in memory.
	// Cleared early by a pull that re-lists the VM — that is the centre asking
	// for it back, and intent always wins over the stand-off.
	vmRemoved map[string]time.Time

	// nudge lets a finished job ask the main loop to run an extra cycle now,
	// instead of waiting for the next heartbeat, so the centre reflects the new
	// state (e.g. a migrated VM's new owner) within a second or two. Buffered so a
	// send never blocks the job goroutine; a pending nudge coalesces many jobs.
	nudge chan struct{}

	// lastStatus is the most recent host status built by a cycle. A lightweight
	// keepalive goroutine re-sends it between cycles to refresh the centre's
	// LastContact, so a long reconcile/cluster pass does not make the host look
	// offline (the reconcile loop only reports at the end of each cycle). Guarded
	// by statusMu.
	statusMu   sync.Mutex
	lastStatus *types.HostStatus

	// Caches for the throttled observations (see the *RefreshEvery cadences),
	// reused between refreshes so a 15s cycle is not dominated by rescanning state
	// that rarely changes. Each `have*` flag forces a first collection.
	lastResources  types.HostResources
	lastNetProfile string
	haveResources  bool
	lastInventory  types.HostInventory
	haveInventory  bool
	// lastMetrics is carried forward when a collection fails or times out, so a
	// host whose storage is hanging reports its last real CPU and memory rather
	// than zeros. See the collect site.
	lastMetrics  types.HostMetrics
	lastIdentity hyperv.HostIdentity
	haveIdentity bool

	// lastObservedVMs caches the host-wide VM inventory (roles/power/replication)
	// for discovery/adoption and the operational VM view; refreshed on the
	// observeVMsEvery cadence (faster than resources — it is what changes during
	// power/failover ops).
	lastObservedVMs []types.ObservedVM
	haveObservedVMs bool

	// forceScan makes the next cycle re-collect the throttled observations
	// (resources and the VM inventory) instead of reusing the cache. A finished
	// job sets it via requestNudge so the centre reflects what the job changed
	// immediately — e.g. a stopped test failover's clone disappears at once,
	// rather than lingering until the next throttled refresh.
	forceScan atomic.Bool

	// forceObserveVMs forces just the VM observation next cycle, without the
	// heavier resource/inventory rescan forceScan triggers. The VM-state watcher
	// sets it (via requestObserveNudge) so a power change surfaces within a couple
	// of seconds while an event storm cannot churn the expensive scans.
	forceObserveVMs atomic.Bool

	// lastPassMessage explains the last COMPLETED cycle, when that cycle was slow
	// enough to be worth explaining. A cycle can only be measured once it has
	// ended, and status is reported partway through one, so the message always
	// travels on the following report — which is why it says "the last completed
	// pass" rather than "this pass". Empty once a cycle comes in under the
	// threshold, so the condition clears itself when the host recovers.
	lastPassMessage string

	// switchWasFailing remembers whether the previous cycle's switch step failed,
	// so the adapter re-read fires on the transition into failure rather than on
	// every cycle it persists.
	switchWasFailing bool

	// cyclePhases is the running cycle's phase timer, so work done deeper in the
	// call stack can be attributed without threading a timer through every
	// signature. Set at the top of a cycle and cleared at the end; only ever
	// touched from the cycle goroutine.
	cyclePhases *phaseTimer
}

// phase records a span against the running cycle, or does nothing when called
// outside one (the keepalive goroutine, a test).
func (r *runner) phase(name string) func() {
	if r.cyclePhases == nil {
		return func() {}
	}
	return r.cyclePhases.mark(name)
}

// hasFailedSwitchStep reports whether a virtual switch could not be applied this
// pass. The console's re-map offer is derived from the adapter inventory, so
// this is what says that inventory is now the thing worth having fresh.
func hasFailedSwitchStep(conds []types.Condition) bool {
	for _, c := range conds {
		if c.Reason == "ApplyFailed" && strings.HasPrefix(c.Type, "Switch/") {
			return true
		}
	}
	return false
}

// passConditions puts the pass-timing condition into a condition set, replacing
// any it already carries. One function so the freshly built status and the
// keepalive's replay cannot describe the pass differently — and so a replay
// carrying a NEWER message than the status it rides on still ends up with one
// ReconcilePass condition rather than two.
func passConditions(conds []types.Condition, msg string) []types.Condition {
	out := conds[:0:0]
	for _, c := range conds {
		if c.Type != "ReconcilePass" {
			out = append(out, c)
		}
	}
	if msg == "" {
		return out
	}
	return append(out, types.Condition{
		Type: "ReconcilePass", Status: false, Reason: "Slow",
		Message: msg, LastTransitionTime: time.Now().UTC(),
	})
}

// withPassMessage returns st with its pass-timing condition replaced. Used by
// the keepalive, whose payload is otherwise a byte-for-byte replay.
func withPassMessage(st types.HostStatus, msg string) types.HostStatus {
	if msg == "" {
		return st
	}
	st.Conditions = passConditions(st.Conditions, msg)
	return st
}

// notePassTiming records the finished cycle's cost for the next status report.
// Called from the cycle's own deferred drain, on the cycle goroutine.
func (r *runner) notePassTiming(msg string) {
	// Under statusMu: the keepalive goroutine reads this to carry the newest
	// timing on a replayed status, which is the only route out for a cycle that
	// wedges before it can build one.
	r.statusMu.Lock()
	r.lastPassMessage = msg
	r.statusMu.Unlock()
	if msg != "" {
		r.log.Warn("slow reconcile pass", "detail", msg)
	}
}

// requestNudge asks the main loop for an immediate follow-up cycle (non-blocking:
// if one is already queued, this is a no-op).
func (r *runner) requestNudge() {
	if r.nudge == nil {
		return
	}
	// Force the nudged cycle to re-scan resources and the VM inventory, not reuse
	// the throttled cache — set before signalling so the cycle sees it.
	r.forceScan.Store(true)
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

// requestObserveNudge asks for an immediate cycle that refreshes only the VM
// observation, not the heavier resource/inventory scans. The VM-state watcher
// uses this so a power change is reflected promptly without the cost (or the
// event-storm amplification) of a full forceScan. Non-blocking and coalescing.
func (r *runner) requestObserveNudge() {
	if r.nudge == nil {
		return
	}
	r.forceObserveVMs.Store(true)
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

const agentVersion = version.Version

// run drives the agent until ctx is cancelled.
func (r *runner) run(ctx context.Context) error {
	// Adopt last-honoured state before touching the network, so a host that
	// reboots while the centre is offline comes straight back up enforcing the
	// intent it was last given.
	r.adoptCachedState()

	// Cap the reconnect backoff well under the centre's 90s staleness window: the
	// gRPC default maxes at ~120s, so after the centre restarts (or any blip) the
	// agent could take up to two minutes to re-report, making a healthy host — and
	// its cluster — flap to "offline". A 20s cap keeps LastContact fresh.
	conn, err := grpc.NewClient(r.cfg.centreAddr,
		transportCreds(r.cfg.tlsDir, r.log),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: time.Second, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 20 * time.Second},
			MinConnectTimeout: 10 * time.Second,
		}),
	)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := ballastpb.NewAgentServiceClient(conn)

	r.nudge = make(chan struct{}, 1)

	// Start the keepalive BEFORE the first reconcile cycle. The first cycle can be
	// slow or even hang (e.g. reconciling a paused-critical VM or degraded storage),
	// and it must not gate liveness — otherwise the host reads offline until the
	// cycle returns. The keepalive re-sends the last built status once the first
	// cycle produces one; until then it is a harmless no-op. This is decoupled from
	// the reconcile loop, which the centre's 90s staleness window otherwise trips on
	// a busy host.
	go r.keepalive(ctx, client)

	// Imperative jobs (stop/restart/checkpoint/…) are one-shot operator actions
	// that must feel immediate. They arrive on the pull, but the reconcile cycle
	// pulls only every heartbeat AND runs jobs at its tail — after host/VM/cluster
	// reconcile — so a slow or stuck cycle (S2D/cluster queries on a busy member)
	// leaves a queued job Pending for many seconds while the keepalive still shows
	// the host online. Poll for jobs on a short, dedicated interval, decoupled from
	// reconcile, so a stop/restart is picked up within a couple of seconds.
	go r.pollJobs(ctx, client)

	// Subscribe to VM power-state changes so a stop/start/pause is reflected within
	// a couple of seconds instead of waiting for the polled observe tier. This only
	// nudges a re-observe — the periodic cycle stays the source of truth, so a
	// dropped subscription or missed event costs latency, never correctness.
	go r.watchVMState(ctx)

	// Store maintenance runs here, in the background, and NOT inside store.Open.
	//
	// It used to run on the way in, and on the one agent whose journal was big
	// enough that was fatal: HVNEW04 arrived with 186,000 entries in a 1 GiB
	// store, spent 20 seconds and 700 MB pruning them, and the Windows SCM —
	// which allows 30 seconds to report Running — killed and restarted it on a
	// loop. The host had no agent at all. Startup must stay cheap; the tidying
	// can take as long as it likes out here.
	go r.maintainStore(ctx)

	// One immediate cycle so state is fresh on startup, then on a ticker.
	r.cycle(ctx, client)

	ticker := time.NewTicker(r.cfg.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.cycle(ctx, client)
		case <-r.nudge:
			// A job just finished — run an extra cycle now so the centre reflects
			// the new state (migrated VM's owner, power change, etc.) promptly.
			r.cycle(ctx, client)
		}
	}
}

// maintainStore prunes the status journal on its own schedule, off both the
// startup path and the status-delivery path.
//
// Once soon after start, to clear whatever a previous version left behind, then
// periodically to hold it bounded. PruneJournal works in chunks, so a large
// backlog costs many short transactions rather than one enormous one, and the
// agent keeps reconciling throughout.
//
// Errors are logged and dropped: a store that cannot prune still works, it is
// just larger than it needs to be, and failing the agent over housekeeping would
// trade a disk-space problem for an unmanaged host.
func (r *runner) maintainStore(ctx context.Context) {
	prune := func() {
		start := time.Now()
		if err := r.st.PruneJournal(); err != nil {
			r.log.Warn("prune status journal failed", "err", err)
			return
		}
		if d := time.Since(start); d > time.Second {
			// Only worth a line when it actually did something substantial —
			// the first run after an upgrade, or a long spell offline.
			r.log.Info("status journal pruned", "took", roundMS(d))
		}
	}
	// A short delay so the first prune does not compete with registration and
	// the first reconcile for the host's disk.
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}
	prune()

	t := time.NewTicker(journalPruneEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}

// adoptCachedState loads the last-honoured desired Host and adopts its identity
// so the agent can operate (and, once reconcile exists, enforce) without first
// reaching the centre.
func (r *runner) adoptCachedState() {
	cached, ok, err := r.st.LoadDesiredHost()
	if err != nil {
		r.log.Error("read cached desired state failed", "err", err)
		return
	}
	if !ok {
		r.log.Info("no cached desired state; awaiting first pull from centre")
		return
	}
	r.uid = cached.Meta.UID
	r.log.Info("adopted last-honoured desired state",
		"host", cached.Meta.Name, "generation", cached.Meta.Generation, "uid", r.uid)
}

// tryRegister attempts a single best-effort registration. Failure is logged and
// retried on the next cycle; it never blocks the loop. inv is the inventory
// already collected this cycle, reused to avoid a second host query.
func (r *runner) tryRegister(ctx context.Context, client ballastpb.AgentServiceClient, inv types.HostInventory) {
	resp, err := client.RegisterHost(ctx, &ballastpb.RegisterHostRequest{
		HostName:     r.cfg.hostName,
		Fqdn:         "", // resolved host-side later; centre seeds FQDN in desired state
		Inventory:    ballastpb.InventoryToProto(inv),
		AgentVersion: agentVersion,
	})
	if err != nil {
		r.log.Warn("register failed; running on cached state, will retry next cycle", "err", err)
		return
	}
	r.uid = resp.GetUid()
	r.registered = true
	r.log.Info("registered with centre",
		"host", r.cfg.hostName, "uid", r.uid, "known", resp.GetKnown())
}

// keepalive refreshes the centre's LastContact between reconcile cycles by
// re-sending the last built status on a short, fixed interval (well under the
// centre's 90s staleness window). It is intentionally lightweight — a direct
// ReportStatus with no journaling or reconcile — so a slow cycle (large CSV I/O,
// cluster/storage queries) never makes the host appear offline. Best-effort: a
// failure (centre unreachable) is ignored; the next full cycle handles autonomy.
func (r *runner) keepalive(ctx context.Context, client ballastpb.AgentServiceClient) {
	t := time.NewTicker(25 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.statusMu.Lock()
			st := r.lastStatus
			msg := r.lastPassMessage
			r.statusMu.Unlock()
			if st == nil || r.uid == "" {
				continue
			}
			// Carry the newest pass timing even though the rest is a replay.
			//
			// A status is only BUILT partway through a cycle, so a cycle that runs
			// out of its budget before reaching that point never builds one — and
			// the keepalive goes on resending the last good status for as long as
			// the wedge lasts. The pass message is the one thing that says WHY, it
			// is recorded when the cycle ends (including when it is cancelled), and
			// attaching it here is the only way it gets out.
			//
			// HVNEW02, 2026-08-14: readings eleven minutes old, conditions frozen
			// at "HyperVRole not attempted — something earlier in the pass is slow",
			// and the phase breakdown that names the slow phase sat on the host
			// where nobody could see it. The diagnosis was stuck behind exactly the
			// fault it diagnoses.
			replay := withPassMessage(*st, msg)
			if _, err := client.ReportStatus(ctx, &ballastpb.ReportStatusRequest{
				HostName: r.cfg.hostName,
				Uid:      r.uid,
				Status:   ballastpb.StatusToProto(replay),
				// Marked as a replay, not a fresh look. The payload is byte-for-byte
				// the last real report, so the centre would otherwise re-date every
				// reading in it once every 25s and a wedged reconcile would keep
				// looking current for as long as the process stayed up.
				Keepalive: true,
			}); err != nil {
				r.log.Debug("keepalive report failed", "err", err)
			}
		}
	}
}

// jobPollInterval is how often the dedicated job poll checks for queued
// imperative actions — short enough that stop/restart/checkpoint feel immediate,
// independent of the (potentially slow) reconcile cycle.
const jobPollInterval = 3 * time.Second

// pollJobs picks up and runs queued imperative jobs on a short interval, so an
// operator action is not held hostage by the reconcile cycle's cadence or a slow
// pass. It sends the known generation so the desired-state part of the pull is a
// cheap "unchanged"; only the jobs are acted on here (runJobs is dedup-safe, so
// running alongside the cycle never double-executes). Best-effort: an unreachable
// centre just retries next tick, and jobs never run autonomously anyway.
func (r *runner) pollJobs(ctx context.Context, client ballastpb.AgentServiceClient) {
	t := time.NewTicker(jobPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if r.uid == "" {
				continue // not registered yet; the first cycle establishes identity
			}
			known, _, _ := r.st.LoadDesiredHost()
			resp, err := client.PullDesiredState(ctx, &ballastpb.PullDesiredStateRequest{
				HostName:        r.cfg.hostName,
				Uid:             r.uid,
				KnownGeneration: known.Meta.Generation,
			})
			if err != nil {
				continue
			}
			if jobs := resp.GetJobs(); len(jobs) > 0 {
				r.runJobs(ctx, client, jobs)
			}
		}
	}
}

// watchVMState supervises the VM power-state subscription: it runs the watcher,
// and if it ends (the CIM provider restarts, the query faults, powershell.exe
// exits) re-establishes it after a bounded backoff, until ctx is cancelled. Each
// change nudges a light re-observe; the subscription carries no state, so a gap
// while it restarts only defers to the periodic cycle. Purely an optimisation, so
// a persistent failure degrades to plain polling rather than breaking anything.
func (r *runner) watchVMState(ctx context.Context) {
	const (
		baseBackoff = 2 * time.Second
		maxBackoff  = 60 * time.Second
	)
	backoff := baseBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		err := r.hv.WatchVMState(ctx, r.requestObserveNudge)
		if ctx.Err() != nil {
			return
		}
		// A watcher that ran for a good while then ended is a transient provider
		// blip, not a config problem — reset the backoff so it re-subscribes fast.
		if time.Since(start) > 2*maxBackoff {
			backoff = baseBackoff
		}
		r.log.Warn("vm state watcher ended; re-subscribing", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// cycle is one heartbeat. It collects inventory once, then in order:
// (best-effort) registers if not yet registered, pulls and caches desired
// state, reconciles (next step), and reports status. Every centre interaction
// is best-effort: a failure flips the agent to Autonomous and journals status
// locally for later replay, but the cycle still completes.
func (r *runner) cycle(parent context.Context, client ballastpb.AgentServiceClient) {
	r.cycles++
	// Time the cycle and its phases, and say so at the end.
	//
	// There was no way to measure this. The centre logs a status report per
	// cycle, but the keepalive sends one too, so the intervals in its log are a
	// mixture and the average reads far lower than reality. The only handle was
	// subtracting condition timestamps out of a stored status by hand, which
	// gives the host reconcile alone and nothing else.
	//
	// It matters more the more the agent does: cluster-aware updating walks a
	// cluster a node at a time, so per-cycle latency multiplies by the number of
	// nodes, and "the console feels slow" needs to resolve to a phase.
	cycleStart := time.Now()
	var t phaseTimer
	r.cyclePhases = &t
	defer func() { r.cyclePhases = nil }()
	// Bound the whole cycle: if any operation hangs, the deadline cancels it (and
	// kills the underlying powershell.exe) so the loop always regains control and
	// the next cycle retries — the cycle can never wedge the agent.
	ctx, cancel := context.WithTimeout(parent, cycleTimeout)
	defer cancel()
	defer func() {
		// INFO, not DEBUG: this is the number anyone diagnosing a slow console
		// asks for first, and an agent log nobody can read at its default level
		// would not have answered the question that prompted it.
		r.log.Info("cycle complete", append([]any{"total", roundMS(time.Since(cycleStart))}, t.fields()...)...)
		// Drain the call timings HERE, against the cycle this deferred function
		// belongs to. The drain claims every call made since the last one, so it
		// has to be paired with a timer spanning the same window — everything the
		// cycle did, the collections and both reconciles — or the list describes
		// work the duration never covered. See reconcile/timing.go.
		//
		// Deferred so every path drains, including the early returns above: a cycle
		// that leaves its calls behind makes the NEXT cycle report them as its own,
		// which is the same defect from the other end.
		// ctx.Err() distinguishes a cycle that ENDED from one that was cut off at
		// the cap. They read identically as a duration — HVNEW03 reported "5m0s"
		// three times running and only the round number gave it away — and they
		// mean opposite things: one is a slow pass that did all its work, the
		// other did not finish and everything after the stall never ran.
		r.notePassTiming(reconcile.SlowPassMessage(
			r.hv.TakeTimings(), t.timings(), time.Since(cycleStart), reconcile.SlowPassThreshold,
			ctx.Err() != nil))
	}()
	// A finished job asks (via requestNudge) for a fresh scan of the throttled
	// observations so its effect shows up now; read-and-clear it once for the
	// whole cycle. observeForce is the lighter VM-only variant the state watcher
	// raises — it refreshes the VM view without the resource/inventory rescan.
	force := r.forceScan.Swap(false)
	observeForce := r.forceObserveVMs.Swap(false)

	// Metrics (CPU/memory/uptime) are live and cheap — collect every cycle.
	doneMetrics := t.mark("metrics")
	metrics, merr := bounded(ctx, observeBudget, r.hv.CollectMetrics)
	doneMetrics()
	if merr != nil {
		// The last real reading, not zeros. A failed collection is a reading
		// nobody took, and reporting 0% CPU and 0 bytes of memory in use states
		// the opposite of what is known — on a host whose storage subsystem is
		// hanging, which is exactly when someone is looking at those numbers.
		// Absent is not zero.
		r.log.Error("collect metrics failed (keeping last known)", "err", merr)
		metrics = r.lastMetrics
	} else {
		r.lastMetrics = metrics
	}
	// Physical hardware inventory does not change without a reboot, so refresh it
	// on a slow cadence — but always collect it before we are registered, since
	// registration reports it.
	inv := r.lastInventory
	if !r.haveInventory || force || !r.registered || r.cycles%inventoryRefreshEvery == 0 {
		doneInv := t.mark("inventory")
		got, ierr := bounded(ctx, observeBudget, r.hv.CollectInventory)
		doneInv()
		if ierr != nil {
			r.log.Error("collect inventory failed", "err", ierr)
		} else {
			inv = got
			r.lastInventory = got
			r.haveInventory = true
		}
	}
	// Resources (switch details, volumes, ISO scan, vNIC topology) are the heaviest
	// observation and change rarely — refresh on the slow cadence, reuse between.
	resources := r.lastResources
	if force || !r.haveResources || r.cycles%resourceRefreshEvery == 0 {
		doneRes := t.mark("resources")
		res, resErr := bounded(ctx, observeBudget, r.hv.CollectResources)
		doneRes()
		if resErr != nil {
			r.log.Error("collect resources failed", "err", resErr)
		} else {
			resources = res
			r.lastResources = res
			r.haveResources = true
		}
	}
	// Host identity (computer name, AD domain) is effectively immutable after join —
	// refresh it only occasionally.
	identity := r.lastIdentity
	if !r.haveIdentity || r.cycles%identityRefreshEvery == 0 {
		doneID := t.mark("identity")
		id, iderr := r.hv.GetHostIdentity(ctx)
		doneID()
		if iderr != nil {
			r.log.Error("get host identity failed", "err", iderr)
		} else {
			identity = id
			r.lastIdentity = id
			r.haveIdentity = true
		}
	}

	// Establish identity if we have not confirmed a registration yet. This never
	// blocks: on failure we proceed on cached state and retry next cycle.
	if !r.registered {
		r.tryRegister(ctx, client, inv)
	}

	autonomous := false
	var assignment *reconcile.ClusterAssignment
	var secrets map[string]types.Secret

	// Pull desired state, sending the generation we already hold so the centre
	// can answer "unchanged" cheaply. Periodically force a full pull
	// (KnownGeneration=0) so the agent self-heals if a generation is ever reused
	// with different content — e.g. an in-memory centre restart resets the
	// generation counter while the agent still caches the old content at that
	// number. Without this the agent would trust the "unchanged" answer forever.
	known, _, _ := r.st.LoadDesiredHost()
	knownGen := known.Meta.Generation
	// A forced cycle also pulls in full. An operator asking for a reconcile is
	// usually asking precisely because they doubt what the agent is holding, and
	// the generation-match short-circuit is the one thing that could keep a stale
	// cache alive across the request.
	if force || r.cycles%fullResyncEvery == 0 {
		knownGen = 0
	}
	resp, err := client.PullDesiredState(ctx, &ballastpb.PullDesiredStateRequest{
		HostName:        r.cfg.hostName,
		Uid:             r.uid,
		KnownGeneration: knownGen,
	})
	switch {
	case err != nil:
		autonomous = true
		r.log.Warn("pull desired state failed; running autonomously on cached state", "err", err)
	case resp.GetHasDesiredState() && !resp.GetUnchanged():
		desired := ballastpb.HostFromProto(resp.GetHost())
		if serr := r.st.SaveDesiredHost(desired); serr != nil {
			r.log.Error("persist desired state failed", "err", serr)
		} else {
			r.log.Info("desired state updated",
				"host", desired.Meta.Name, "generation", desired.Meta.Generation)
		}
	}
	// The centre answered, so anything the agent finished while it was away can be
	// delivered now. Before status, and before the work below: a job that completed
	// during an outage should settle the moment contact returns, not linger Running
	// until an operator gives up and cancels it.
	if err == nil {
		r.replayJobResults(ctx, client)
	}
	// Cluster assignment is only acted on when the centre is reachable: once a
	// cluster exists, Failover Clustering maintains it without the centre.
	if err == nil {
		if ca := resp.GetClusterAssignment(); ca != nil && ca.GetIsMember() {
			assignment = &reconcile.ClusterAssignment{
				IsMember: true,
				IsFormer: ca.GetIsFormer(),
				Cluster:  ballastpb.ClusterFromProto(ca.GetCluster()),
			}
		}
		// Cache the VMs placed on this host as last-honoured intent, so the agent
		// keeps enforcing them while the centre is offline. The centre always
		// sends the full set, so this faithfully reflects additions and removals.
		vms := make([]types.VM, 0, len(resp.GetVms()))
		for _, pv := range resp.GetVms() {
			vms = append(vms, ballastpb.VMFromProto(pv))
		}
		// When the centre changes what VMs this host should run — a create, delete,
		// migration, or a failover flip that moves a VM on or off this host — the
		// host's observed VM view is immediately stale. Force a fresh observe THIS
		// cycle so the change surfaces at once, not on the next observeVMs tick.
		// This is what makes the OTHER side of a failover (the former primary that
		// becomes a replica, which never ran the job) reflect its new role promptly.
		if prev, ok, _ := r.st.LoadDesiredVMs(); !ok || vmSetChanged(prev, vms) {
			force = true
		}
		// The centre listing a VM again outranks any local stand-off from a delete.
		r.clearVMRemoved(vms)
		if serr := r.st.SaveDesiredVMs(vms); serr != nil {
			r.log.Error("persist desired vms failed", "err", serr)
		}
		// Which ISO library applies is resolved ONLY on a successful pull, because
		// a nil assignment is ambiguous: it means "not a member" after a good pull
		// and "we do not know" after a failed one. Recomputing on a failed pull
		// would read the second as the first, and a cluster member would silently
		// fall back to a host-level library the moment the centre went away —
		// which is the opposite of what autonomy promises. Held instead, so the
		// agent keeps enforcing the library it was last given.
		if cached, ok, _ := r.st.LoadDesiredHost(); ok {
			r.isoLib = reconcile.EffectiveISOLibrary(cached, assignment)
		}
		// Secrets are delivered fresh each pull and used in this cycle's
		// reconcile; they are never written to the local store.
		if ss := resp.GetSecrets(); len(ss) > 0 {
			secrets = make(map[string]types.Secret, len(ss))
			for _, ps := range ss {
				s := ballastpb.SecretFromProto(ps)
				secrets[s.Name] = s
			}
		}
	}

	// Reconcile against the cached desired state. This runs whether or not the
	// centre was reachable above: the agent enforces the last intent it was
	// given. ObservedGeneration only advances when the spec is fully honoured.
	phase := types.PhasePending
	var conds []types.Condition
	var hyperVInstalled, rebootRequired, inMaintenance bool
	var hostISCSI *types.ISCSIStatus
	if cached, ok, lerr := r.st.LoadDesiredHost(); lerr != nil {
		r.log.Error("read cached desired state failed", "err", lerr)
	} else if ok {
		doneHost := t.mark("hostReconcile")
		res, rerr := r.reconciler.Reconcile(ctx, cached, secrets)
		doneHost()
		phase, conds = res.Phase, res.Conditions
		hyperVInstalled, rebootRequired = res.HyperVInstalled, res.RebootRequired
		inMaintenance = res.InMaintenance
		hostISCSI = res.ISCSI
		if rerr != nil {
			r.log.Error("reconcile incomplete", "err", rerr)
		}
		if res.Honoured && r.observedGen != cached.Meta.Generation {
			r.observedGen = cached.Meta.Generation
			r.log.Info("generation honoured", "generation", r.observedGen)
		}
		// A switch that cannot be built is usually a switch whose team members have
		// been renamed, and the console explains that by comparing the spec's
		// adapter names against the INVENTORY. The inventory is refreshed every
		// eighth cycle, though — minutes on a slow host — so the failure arrived
		// with an adapter list old enough to still contain the missing names, and
		// the console had nothing to say until a refresh happened to come round.
		// The operator saw the raw Set-VMSwitchTeam error for that whole window.
		//
		// So a failed switch step refreshes it now, in the same cycle, and the
		// explanation reaches the centre in the same report as the failure. Only on
		// the FIRST failing cycle: the state persists until someone re-maps, and
		// re-reading the hardware every pass afterwards buys nothing.
		switchFailing := hasFailedSwitchStep(res.Conditions)
		if switchFailing && !r.switchWasFailing && r.haveInventory {
			doneInv := t.mark("inventoryAfterSwitchFailure")
			got, ierr := bounded(ctx, observeBudget, r.hv.CollectInventory)
			doneInv()
			if ierr != nil {
				r.log.Warn("re-read adapters after a switch failure", "err", ierr)
			} else {
				inv, r.lastInventory = got, got
				r.log.Info("switch step failed; adapters re-read so the console can name the missing ones")
			}
		}
		r.switchWasFailing = switchFailing
	}

	// Read before the status is built. Cheap, and it is the one reading that
	// survives the cluster being unreadable — which is exactly when it is needed,
	// since a quarantined node's cluster service is stopped and it can answer
	// nothing else.
	// A failed read keeps the last known value rather than clearing it — the same
	// absent-versus-unknown rule as the licence read — and says so, because an
	// empty membership that nobody can explain is how three hosts came to report
	// nothing with no way to find out why.
	if ns, err := r.hv.GetNodeSelf(ctx); err != nil {
		r.log.Warn("read own cluster membership failed (keeping last known)", "err", err)
	} else {
		r.nodeSelf = ns
	}
	st := r.buildStatus(inv, metrics, resources, autonomous, phase, conds, hyperVInstalled, rebootRequired, inMaintenance)
	st.ObservedVMs = r.observeVMs(ctx, force || observeForce)
	st.ISOLibrary = r.observeISOLibrary(ctx, force)
	st.WindowsLicence = r.observeWindowsLicence(ctx, force)
	// A standalone host's own array, or — for a member — its own view of the
	// cluster's, which the centre folds into the cluster's per-node list. Both are
	// the same fact about this host: its initiator, its sessions, its paths.
	st.ISCSI = hostISCSI
	if st.ISCSI == nil {
		st.ISCSI = r.clusterISCSI
	}
	// VMs found sitting on this host's storage that nothing has registered.
	//
	// REPORTED here, SCANNED only by the job. The walk runs Compare-VM on every
	// configuration it finds, so its cost scales with somebody else's data — the
	// one thing in a pass Ballast does not control — and it has no business in
	// the loop. What the pass carries is the last result together with when it
	// was taken, so a list is never shown without its age.
	st.ImportableVMs, st.ImportScan = r.reconciler.ImportStatus()
	st.MigrationPasses = r.reconciler.MigrationPasses()
	// The scan job needs to know which volumes this host can see, and the pass
	// is where that becomes known.
	r.reconciler.RememberImportRoots(resources)
	st.ComputerName = identity.ComputerName
	st.Domain = identity.Domain
	// Network location changes only when domain reachability does — refresh it
	// periodically and reuse the cached value otherwise.
	if r.lastNetProfile == "" || r.cycles%netProfileRefreshEvery == 0 {
		if np, nperr := r.hv.GetNetworkProfile(ctx); nperr != nil {
			r.log.Warn("get network profile failed", "err", nperr)
		} else {
			r.lastNetProfile = np
		}
	}
	st.NetworkProfile = r.lastNetProfile
	// Cache for the keepalive goroutine before reporting, so it can refresh
	// LastContact with current data between cycles.
	r.statusMu.Lock()
	cp := st
	r.lastStatus = &cp
	r.statusMu.Unlock()
	doneReport := t.mark("reportStatus")
	r.reportStatus(ctx, client, st)
	doneReport()

	// Cluster reconcile, only when the centre gave us a current assignment. Every
	// member reconciles (so the clustering feature is ensured on all of them), but
	// only the former REPORTS cluster status: it is the single authority, which
	// avoids a last-writer-wins race between members clobbering each other's view
	// (e.g. one member observing groups, another reporting none).
	//
	// This is the expensive, hang-prone pass and cluster desired state changes
	// rarely, so it is edge-triggered: run at once when the cluster's Generation
	// changes or a job asked for a rescan (force), and otherwise only on a slow idle
	// sweep as a drift/retry/post-reboot safety net (see clusterReconcileEvery).
	// Entering or leaving maintenance changes the cluster's roles, its pool and
	// its volumes all at once, so it earns an immediate pass the same way an
	// operator edit does.
	// The OBSERVED state, not the declared one: it flips when the node actually
	// pauses or resumes, which is when the pool and volumes actually change. The
	// declared value flips a pass earlier, while the drain is still running, so a
	// rescan fired on it would capture the state it was trying to replace.
	maintChanged := inMaintenance != r.lastMaintenance
	r.lastMaintenance = inMaintenance
	if assignment != nil {
		genChanged := assignment.Cluster.Meta.Generation != r.lastClusterGen
		if force || genChanged || maintChanged || r.cycles%clusterReconcileEvery == 0 {
			// Let the cluster reconcile time its own stages on the SAME timer, so
			// the sub-phases land in the same list as the phases around them
			// rather than in a second instrument that has to be kept honest.
			r.reconciler.SetPhaseRecorder(t.mark)
			doneCluster := t.mark("clusterReconcile")
			cres, cerr := r.reconciler.ReconcileCluster(ctx, *assignment, secrets)
			doneCluster()
			if cerr != nil {
				r.log.Error("cluster reconcile incomplete", "err", cerr)
			}
			r.lastClusterGen = assignment.Cluster.Meta.Generation
			// This node's OWN iSCSI view is kept whether or not it is the former,
			// and carried out on the host status below.
			//
			// Only the former reports CLUSTER status, which is right for everything
			// cluster-wide — several members writing the same groups, CSVs and pool
			// would race. But iSCSI is not cluster-wide: every member has its own
			// initiator, its own sessions and its own paths, and a non-former simply
			// computed that and dropped it. The centre then held one member's view
			// and called it the cluster's, which is exactly the failure iSCSI
			// produces — one node loses a path while the others are perfect.
			//
			// The host status channel is the right one: every member reports it, and
			// each host owns its own, so there is no write to race over.
			// A FAILED observation is not an absence. reconcileISCSI returns nil for
			// a pass that could not read the initiator — a transient PowerShell
			// failure, a slow VMMS, a credential not yet delivered — and assigning
			// that straight through made the console's multipath row vanish and
			// reappear as passes succeeded and failed. Absence has to mean the
			// cluster no longer declares an array, which is the one case worth
			// clearing for.
			if cres.ISCSI != nil || assignment.Cluster.Spec.StorageKind() != types.StorageKindISCSI {
				r.clusterISCSI = cres.ISCSI
			}
			if assignment.IsFormer {
				r.reportClusterStatus(ctx, client, assignment.Cluster, cres)
			}
		}
	}

	// VM reconcile, against the cached set. This runs whether or not the centre
	// was reachable — the agent enforces the VMs it was last given, same as the
	// host spec. Reporting is best-effort and skipped when autonomous.
	doneVMs := t.mark("vmReconcile")
	r.reconcileVMs(ctx, client, autonomous, force)
	doneVMs()

	// Imperative jobs are NOT run here. They are handled by the dedicated pollJobs
	// goroutine (under the long-lived root context, every few seconds), so a slow
	// or hung reconcile cycle never delays an operator action — and so a job's
	// goroutine is never tied to this cycle's timeout-bounded context, which would
	// otherwise kill a long job (a live migration) when the cycle returns.
}

// runJobs launches each pending job in the background so a long or stuck host
// operation never blocks the reconcile/report cycle (a synchronous hung job
// would otherwise stop the heartbeat and the host would look offline). Each job
// runs under a timeout, is deduped while in flight, and reports its own outcome.
func (r *runner) runJobs(ctx context.Context, client ballastpb.AgentServiceClient, jobs []*ballastpb.Job) {
	for _, pj := range jobs {
		job := ballastpb.JobFromProto(pj)
		r.jobsMu.Lock()
		if r.jobsInflight == nil {
			r.jobsInflight = make(map[string]struct{})
		}
		if _, busy := r.jobsInflight[job.ID]; busy {
			r.jobsMu.Unlock()
			continue
		}
		r.jobsInflight[job.ID] = struct{}{}
		// Record which VM this job holds, if any, so the reconcile loop leaves it
		// alone until the job is done.
		if vmName := job.Params["vm"]; vmName != "" && jobHoldsVM(job.Kind) {
			if r.jobsVMs == nil {
				r.jobsVMs = make(map[string]string)
			}
			r.jobsVMs[strings.ToLower(vmName)] = job.Kind
		}
		r.jobsMu.Unlock()

		go func(job types.Job) {
			defer func() {
				r.jobsMu.Lock()
				delete(r.jobsInflight, job.ID)
				if vmName := job.Params["vm"]; vmName != "" {
					delete(r.jobsVMs, strings.ToLower(vmName))
				}
				r.jobsMu.Unlock()
				// Ask for an immediate follow-up cycle so the centre reflects
				// whatever this job changed (VM owner/power, storage, cluster)
				// without waiting for the next heartbeat.
				r.requestNudge()
			}()
			jctx, cancel := context.WithTimeout(ctx, jobTimeoutFor(job))
			defer cancel()
			r.reportJob(jctx, client, job.ID, types.JobRunning, "")
			// A long-running job (live migration) streams progress notes; surface
			// each as the job's Running message so the console shows a live %.
			onProgress := func(note string) {
				r.reportJob(jctx, client, job.ID, types.JobRunning, note)
			}
			msg, jerr := r.reconciler.ExecuteJob(jctx, job, onProgress)
			if jerr != nil {
				/* Report the failure on a FRESH context, never the job's own.

				   jctx is exactly what has just expired when a job hits its
				   budget, so reporting the failure through it was a call on a
				   dead context that could not succeed. The centre therefore
				   never learned the job had ended: it stayed Running for ever,
				   and a capture behind it stayed Capturing — which the template
				   delete guard then refuses to clear, so the record could not be
				   removed from the console either. The one case where reporting
				   matters most was the one case it could not happen.

				   WithoutCancel so a cycle ending underneath us does not take
				   the report with it; a short budget of its own so this can
				   never be what wedges the agent. */
				rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), jobReportTimeout)
				r.log.Error("job failed", "id", job.ID, "kind", job.Kind, "err", jerr)
				r.reportJob(rctx, client, job.ID, types.JobFailed, describeJobFailure(job, jctx, jerr))
				rcancel()
				return
			}
			r.log.Info("job done", "id", job.ID, "kind", job.Kind, "result", msg)
			// A successful delete has to change what this agent WANTS, not just what
			// the host has. The centre drops the VM's desired state when it enqueues
			// the job, but the agent only learns that on its next pull — and until
			// then its cached set still names the VM, so the reconcile loop dutifully
			// creates it again. Prune the name here, and hold it off for a few
			// minutes so a pass already walking the old set cannot resurrect it
			// either. Deleting is the centre's intent; honouring it immediately is
			// the same contract as any other desired-state change.
			/* CopyVM belongs here for the same reason, and was missing.

			   A copy exports the VM to another host, imports it there and then
			   removes the original — so on THIS host it ends as a delete, and the
			   cached desired set still names the VM until the next pull. Without
			   the same stand-off the reconcile loop simply put it back: measured
			   2026-09-01, HVNew01 and HVNew02 both observed on their source host
			   AND on HVNEW06, twenty seconds apart, no replication relationship on
			   either. One of those copies reported Succeeded.

			   Two live registrations of one VM over one set of files is worse than
			   a failed copy: whichever is started second finds its disks in use,
			   and an operator looking at Failover Cluster Manager sees the VM
			   apparently still where it was. */
			if job.Kind == types.JobRemoveVM || job.Kind == types.JobCopyVM {
				if vmName := job.Params["vm"]; vmName != "" {
					r.noteVMRemoved(vmName)
					if derr := r.st.DropDesiredVM(vmName); derr != nil {
						r.log.Error("drop deleted vm from cached desired state", "vm", vmName, "err", derr)
					}
				}
			}
			r.reportJob(jctx, client, job.ID, types.JobSucceeded, msg)
		}(job)
	}
}

func (r *runner) reportJob(ctx context.Context, client ballastpb.AgentServiceClient, id string, state types.JobState, msg string) {
	_, err := client.ReportJobResult(ctx, &ballastpb.ReportJobResultRequest{
		HostName: r.cfg.hostName, Uid: r.uid, JobId: id, State: string(state), Message: msg,
	})
	if err == nil {
		return
	}
	r.log.Warn("report job result failed", "id", id, "state", state, "err", err)
	// A TERMINAL result that cannot be delivered is queued and replayed when the
	// centre returns — the same treatment the status journal has always had.
	// Without it an agent that finished work while the centre was unreachable lost
	// the outcome for good, and the job read Running in the console for ever.
	//
	// Progress notes are dropped instead: they describe a moment that has passed
	// by the time the centre is back, and replaying "20%" for a job that has since
	// finished would be worse than saying nothing.
	if !state.Terminal() {
		return
	}
	if serr := r.st.AppendPendingResult(id, state, msg); serr != nil {
		r.log.Error("queue undelivered job result", "id", id, "err", serr)
	}
}

// replayJobResults delivers terminal job results the agent could not report when
// they happened. Called on each cycle once the centre is reachable, before status,
// so a job that finished during an outage settles as soon as contact is back
// rather than waiting for an operator to notice and cancel it.
func (r *runner) replayJobResults(ctx context.Context, client ballastpb.AgentServiceClient) {
	pending, err := r.st.PendingResults()
	if err != nil {
		r.log.Error("read pending job results", "err", err)
		return
	}
	for _, p := range pending {
		if _, err := client.ReportJobResult(ctx, &ballastpb.ReportJobResultRequest{
			HostName: r.cfg.hostName, Uid: r.uid, JobId: p.JobID, State: string(p.State), Message: p.Message,
		}); err != nil {
			// Still unreachable: stop and keep the rest for the next cycle, in order.
			return
		}
		if derr := r.st.DropPendingResult(p.Seq); derr != nil {
			r.log.Error("drop delivered job result", "id", p.JobID, "err", derr)
		}
		r.log.Info("replayed a job result the centre missed", "id", p.JobID, "state", p.State, "queuedAt", p.Time)
	}
}

// reconcileVMs drives every cached VM towards desired and reports the resulting
// per-VM status. Each VM's ObservedGeneration advances only when its own
// reconcile was fully honoured, so the centre can see exactly which VMs are
// settled. When autonomous (centre unreachable) it still reconciles — that is
// the point — but does not attempt to report.
func (r *runner) reconcileVMs(ctx context.Context, client ballastpb.AgentServiceClient, autonomous, force bool) {
	cached, ok, err := r.st.LoadDesiredVMs()
	if err != nil {
		r.log.Error("read cached desired vms failed", "err", err)
		return
	}
	if !ok || len(cached) == 0 {
		return
	}

	if r.vmObservedGen == nil {
		r.vmObservedGen = make(map[string]int64)
	}

	// The expensive per-VM configuration read is owed when something could have
	// changed it, and on a slow sweep regardless.
	//
	// force covers a finished job and a desired-state change (requestNudge and
	// vmSetChanged both raise it). The sweep is the drift safety net: the agent
	// exists for when the centre is NOT the only writer, and without a periodic
	// full read an edit made in Hyper-V Manager on a VM nobody reboots would be
	// invisible for ever. The reconciler adds its own trigger — a VM whose power
	// state moved, which is when deferred config changes actually land.
	fullSweep := force || r.cycles%vmFullReadEvery == 0
	results := r.reconciler.ReconcileVMs(ctx, cached, fullSweep)
	byName := make(map[string]types.VM, len(cached))
	for _, vm := range cached {
		byName[vm.Meta.Name] = vm
	}

	var reports []*ballastpb.VMStatusReport
	for _, res := range results {
		gen := byName[res.Name].Meta.Generation
		if res.Honoured && r.vmObservedGen[res.Name] != gen {
			r.vmObservedGen[res.Name] = gen
			r.log.Info("vm generation honoured", "vm", res.Name, "generation", gen)
		}
		st := r.buildVMStatus(res)
		// Capture a console thumbnail for running VMs (best-effort, read-only).
		// Bounded so a wedged capture can't stall the whole status report.
		//
		// Throttled, because it is per running VM per cycle and it is the least
		// urgent thing the agent collects: a thumbnail is a preview of a screen
		// nobody may be looking at, and the live console is a separate path that
		// does not use it. On a host with many VMs this was a PowerShell
		// invocation each, every pass, for a picture that can be a minute stale
		// without anyone noticing.
		if res.PowerState == types.VMPowerRunning && (force || r.cycles%screenCaptureEvery == 0) {
			sctx, scancel := context.WithTimeout(ctx, collectTimeout)
			png, serr := r.hv.GetVMScreen(sctx, res.Name)
			scancel()
			if serr != nil {
				r.log.Warn("vm screen capture failed", "vm", res.Name, "err", serr)
			} else {
				st.ScreenPNG = png
				// A thumbnail of one flat colour means Windows has powered the
				// display down. Established HERE, where the pixels already are,
				// rather than left for the console to infer from a black rectangle
				// that looks exactly like a working session showing a dark desktop.
				st.ScreenBlank = screenIsBlank(png)
			}
		}
		reports = append(reports, &ballastpb.VMStatusReport{
			Name:   res.Name,
			Status: ballastpb.VMStatusToProto(st),
		})
	}

	if autonomous || len(reports) == 0 {
		return
	}
	if _, err := client.ReportStatus(ctx, &ballastpb.ReportStatusRequest{
		HostName:   r.cfg.hostName,
		Uid:        r.uid,
		VmStatuses: reports,
	}); err != nil {
		r.log.Warn("vm status report failed", "err", err)
	}
}

// buildVMStatus assembles a VM's reported status from its reconcile result. The
// reported ObservedGeneration is the last generation fully honoured for that VM,
// held in vmObservedGen so it survives an autonomy window.
func (r *runner) buildVMStatus(res reconcile.VMResult) types.VMStatus {
	return types.VMStatus{
		Phase:               res.Phase,
		ObservedGeneration:  r.vmObservedGen[res.Name],
		PowerState:          res.PowerState,
		VMID:                res.VMID,
		GuestOS:             res.GuestOS,
		IPAddress:           res.IPAddress,
		GuestFQDN:           res.GuestFQDN,
		Checkpoints:         res.Checkpoints,
		Observed:            res.Observed,
		AssignedMemoryBytes: res.AssignedMemoryBytes,
		MemoryDemandBytes:   res.MemoryDemandBytes,
		MemoryStatus:        res.MemoryStatus,
		CPUUsagePercent:     res.CPUUsagePercent,
		UptimeSeconds:       res.UptimeSeconds,
		Replication:         res.Replication,
		Conditions:          res.Conditions,
	}
}

// stampISCSINode names the member an iSCSI snapshot came from.
//
// The reconciler cannot: it would have to use the OS hostname, which need not
// match the name the centre keys this host by. Reporting a name the centre does
// not recognise would make a per-node fault impossible to attribute — which is
// the entire reason the status is per-node.
// An agent reports ONLY ITS OWN entry — a single-element list. It has no view of
// any other member's iSCSI state and must not appear to speak for one; the
// centre merges the entries into the cluster's list on receipt.
func (r *runner) stampISCSINode(st *types.ISCSIStatus) []types.ISCSIStatus {
	if st == nil {
		return nil
	}
	out := *st
	out.Node = r.cfg.hostName
	return []types.ISCSIStatus{out}
}

// observeISOLibrary probes the host's effective boot-media share, throttled.
//
// The probe reaches a file server and its computer-account half runs a scheduled
// task that can wait up to 45s, so it is the least appropriate thing to do every
// pass. Between probes the previous result is carried forward: a share's contents
// and permissions change on human timescales, and reporting nil on the skipped
// cycles would make the console's library flicker in and out of existence.
//
// A library that is no longer declared clears immediately, though — that is a
// decision, not a stale reading, and it must not linger.
// observeWindowsLicence reads the host's Windows edition and activation on a slow
// cadence, carrying the last answer forward in between.
//
// A FAILED read keeps the previous value rather than clearing it. Reporting no
// licence because one pass could not read it would make a host look unlicensed
// for a reason that has nothing to do with its licence — the same absent-versus-
// unknown confusion that has caused most of the damage elsewhere in this agent.
func (r *runner) observeWindowsLicence(ctx context.Context, force bool) *types.WindowsLicenceStatus {
	if !force && r.licenceState != nil && r.cycles%windowsLicenceEvery != 0 {
		return r.licenceState
	}
	lic, err := r.hv.GetWindowsLicence(ctx)
	if err != nil {
		r.log.Warn("read windows licence failed (keeping last known)", "err", err)
		return r.licenceState
	}
	r.licenceState = &types.WindowsLicenceStatus{
		Edition:            lic.Edition,
		Description:        lic.Description,
		Evaluation:         lic.Evaluation,
		Status:             lic.Status,
		GraceDaysRemaining: lic.GraceDaysRemaining,
		Channel:            lic.Channel,
		PartialProductKey:  lic.PartialProductKey,
		KMSServer:          lic.KMSServer,
		Message:            lic.Message,
		TargetEditions:     lic.TargetEditions,
	}
	return r.licenceState
}

func (r *runner) observeISOLibrary(ctx context.Context, force bool) *types.ISOLibraryStatus {
	if r.isoLib == nil || r.isoLib.Path == "" {
		r.isoLibState = nil
		return nil
	}
	// A changed share is a new question, so re-probe at once rather than showing
	// the old share's verdict against the new path.
	changed := r.isoLibState != nil && r.isoLibState.Path != r.isoLib.Path
	if !force && !changed && r.isoLibState != nil && r.cycles%isoLibraryEvery != 0 {
		return r.isoLibState
	}
	st, err := r.hv.CheckISOLibrary(ctx, r.isoLib.Path)
	if err != nil {
		// The probe itself failed, which says nothing about the share. Keep the
		// last real answer rather than replacing it with a verdict we do not have.
		r.log.Warn("iso library check failed", "path", r.isoLib.Path, "err", err)
		return r.isoLibState
	}
	r.isoLibState = &types.ISOLibraryStatus{
		Path:            st.Path,
		Readable:        st.Readable,
		MachineReadable: st.MachineReadable,
		Message:         st.Message,
		ISOs:            st.ISOs,
		CheckedAt:       time.Now().UTC(),
	}
	return r.isoLibState
}

// reportClusterStatus sends a cluster-only status report (no host status, so it
// does not disturb the host's own status record).
func (r *runner) reportClusterStatus(ctx context.Context, client ballastpb.AgentServiceClient, cluster types.Cluster, res reconcile.ClusterResult) {
	cs := types.ClusterStatus{
		Phase:              res.Phase,
		ObservedGeneration: cluster.Meta.Generation,
		FormedMembers:      res.FormedMembers,
		S2DEnabled:         res.S2DEnabled,
		Conditions:         res.Conditions,
		StateUnreadable:    res.StateUnreadable,
		Groups:             res.Groups,
		CSVs:               res.CSVs,
		VMs:                res.VMs,
		Nodes:              res.Nodes,
		Pool:               res.Pool,
		Networks:           res.Networks,
		Witness:            res.Witness,
		ReplicaBroker:      res.ReplicaBroker,
		FunctionalLevel:    res.FunctionalLevel,
		NodeOSBuild:        res.NodeOSBuild,
		ISCSI:              r.stampISCSINode(res.ISCSI),
	}
	if !res.Honoured {
		cs.ObservedGeneration = 0
	}
	_, err := client.ReportStatus(ctx, &ballastpb.ReportStatusRequest{
		HostName:      r.cfg.hostName,
		Uid:           r.uid,
		ClusterName:   cluster.Meta.Name,
		ClusterStatus: ballastpb.ClusterStatusToProto(cs),
	})
	if err != nil {
		r.log.Warn("cluster status report failed", "err", err)
	}
}

// vmSetChanged reports whether the desired VM set differs between two pulls, by
// name and generation — so an added, removed, or respecced VM triggers a fresh
// observe. Order-independent; generation catches an in-place spec change (e.g. a
// failover flip rewriting placement/replication) even when the name set is equal.
func vmSetChanged(a, b []types.VM) bool {
	if len(a) != len(b) {
		return true
	}
	seen := make(map[string]int64, len(a))
	for _, vm := range a {
		seen[vm.Meta.Name] = vm.Meta.Generation
	}
	for _, vm := range b {
		if g, ok := seen[vm.Meta.Name]; !ok || g != vm.Meta.Generation {
			return true
		}
	}
	return false
}

// observeVMs enumerates every VM on the host (on the observeVMsEvery cadence, or
// forced after a job) and marks the ones already in this host's desired state as
// managed, so the centre can list unmanaged VMs for discovery/adoption and show
// live roles/power. Best-effort: on error it reuses the last inventory rather
// than blanking discovery.
func (r *runner) observeVMs(ctx context.Context, force bool) []types.ObservedVM {
	vms := r.lastObservedVMs
	if force || !r.haveObservedVMs || r.cycles%observeVMsEvery == 0 {
		if got, err := r.hv.ListObservedVMs(ctx); err != nil {
			r.log.Warn("list observed vms failed; reusing last inventory", "err", err)
		} else {
			vms = got
			r.lastObservedVMs = got
			r.haveObservedVMs = true
		}
	}
	if len(vms) == 0 {
		return nil
	}
	// Mark VMs that are already Ballast-managed (in the cached desired set).
	managed := map[string]bool{}
	if cached, ok, err := r.st.LoadDesiredVMs(); err == nil && ok {
		for _, vm := range cached {
			managed[strings.ToLower(vm.Meta.Name)] = true
		}
	}
	out := make([]types.ObservedVM, len(vms))
	for i, v := range vms {
		v.Managed = managed[strings.ToLower(v.Name)]
		out[i] = v
	}
	return out
}

// buildStatus assembles the status to report. ObservedGeneration carries the
// last fully-honoured generation, so the centre can tell when the host is
// settled even across an autonomy window.
func (r *runner) buildStatus(inv types.HostInventory, metrics types.HostMetrics, resources types.HostResources, autonomous bool, phase types.Phase, conds []types.Condition, hyperVInstalled, rebootRequired, inMaintenance bool) types.HostStatus {
	// The last completed cycle's cost, carried here rather than raised inside the
	// host reconcile: it is the CYCLE that is timed, and the cluster reconcile is
	// part of one without being part of the other.
	r.statusMu.Lock()
	msg := r.lastPassMessage
	r.statusMu.Unlock()
	conds = passConditions(conds, msg)
	return types.HostStatus{
		Phase:              phase,
		ObservedGeneration: r.observedGen,
		HyperVInstalled:    hyperVInstalled,
		ClusterNode:        r.nodeSelf.State,
		ClusterService:     r.nodeSelf.Service,
		ClusterNodeOf:      r.nodeSelf.ClusterName,
		RebootRequired:     rebootRequired,
		InMaintenance:      inMaintenance,
		Autonomous:         autonomous,
		AgentVersion:       agentVersion,
		LastContact:        time.Now().UTC(),
		Inventory:          inv,
		Metrics:            metrics,
		Resources:          resources,
		Conditions:         conds,
	}
}

// reportStatus journals status locally first (so nothing is lost if the centre
// is down), then attempts delivery and replays any previously queued entries.
func (r *runner) reportStatus(ctx context.Context, client ballastpb.AgentServiceClient, st types.HostStatus) {
	// Journalling and delivering are timed apart because they fail for opposite
	// reasons and the fix for each is somewhere else entirely: the journal is a
	// local fsync, so it is slow when this host's DISK is busy, while delivery is
	// a round trip to the centre. Measured together they answer neither question.
	// On HVNEW04 — a healthy host, and a Hyper-V Replica target with a constantly
	// busy datastore — more than half of a 1m49s pass was outside any host call
	// at all, and this is one of the two places it can be.
	doneJournal := r.phase("journal")
	entry, err := r.st.AppendStatus(st, false)
	doneJournal()
	if err != nil {
		r.log.Error("journal status failed", "err", err)
		return
	}

	if st.Autonomous {
		// Centre was unreachable this cycle; leave the entry queued for replay.
		return
	}

	// Replay anything still queued, oldest first, including the entry just made.
	queued, err := r.st.Undelivered()
	if err != nil {
		r.log.Error("read journal failed", "err", err)
		queued = []store.JournalEntry{entry}
	}
	// The other half: the round trip to the centre, timed apart from the local
	// write above so a slow pass says which of the two it was.
	defer r.phase("deliver")()
	for _, e := range queued {
		resp, derr := client.ReportStatus(ctx, &ballastpb.ReportStatusRequest{
			HostName: r.cfg.hostName,
			Uid:      r.uid,
			Status:   ballastpb.StatusToProto(e.Status),
		})
		if derr != nil || !resp.GetAccepted() {
			r.log.Warn("status delivery failed; remains queued", "seq", e.Seq, "err", derr)
			return
		}
		if merr := r.st.MarkDelivered(e.Seq); merr != nil {
			r.log.Error("mark delivered failed", "seq", e.Seq, "err", merr)
		}
	}
}

// jobHoldsVM reports whether a job kind takes exclusive hold of a VM's files
// while it runs, so the reconcile loop must not touch that VM meanwhile.
//
// Only the kinds that actually conflict. A power action or a guest command runs
// happily alongside a reconcile and standing off for those would delay settling
// for no reason — the point is to stop the reconciler fighting an operation that
// owns the VM's disks, not to pause on any activity at all.
func jobHoldsVM(kind string) bool {
	switch kind {
	case types.JobVMMoveStorage, types.JobMigrateVM, types.JobClusterMoveVM,
		types.JobVMClone, types.JobVMCaptureTemplate, types.JobVMExport,
		types.JobVMDiscardSavedState, types.JobVMDiscardSavedStateAndStart,
		types.JobVMApplyCheck, types.JobVMRemoveCheck,
		// RemoveVM belongs here for the opposite reason to the rest: they hold the
		// VM's files, this one deletes them. Without the hold the reconcile loop
		// raced the delete — the job removed the cluster role and the VM, and the
		// pass still holding the VM in its cached desired set recreated it and
		// re-registered the role. Observed on 'Tes', 2026-08-20: gone from Failover
		// Cluster Manager, back in the cluster a couple of minutes later.
		types.JobRemoveVM,
		// And CopyVM, which ends as a delete on this host for exactly that
		// reason. It was absent, so the same race ran again on 2026-09-01 and
		// left HVNew01 and HVNew02 registered on BOTH hosts at once.
		types.JobCopyVM:
		return true
	}
	return false
}

// vmBusy reports whether an imperative job is currently holding a VM, and which.
//
// A VM a RemoveVM job has already deleted stays "busy" for vmRemovedHold. The
// job is over, but the reason to stand off is not: a reconcile pass that loaded
// its desired set before the deletion is still walking that set, and reaching
// this name would recreate the VM.
func (r *runner) vmBusy(name string) (string, bool) {
	r.jobsMu.Lock()
	defer r.jobsMu.Unlock()
	key := strings.ToLower(name)
	if kind, ok := r.jobsVMs[key]; ok {
		return kind, true
	}
	if at, ok := r.vmRemoved[key]; ok {
		if time.Since(at) < vmRemovedHold {
			return types.JobRemoveVM, true
		}
		delete(r.vmRemoved, key)
	}
	return "", false
}

// noteVMRemoved records that a RemoveVM job has deleted a VM from this host, so
// the reconciler stands off the name while a pass already in flight finishes.
func (r *runner) noteVMRemoved(name string) {
	r.jobsMu.Lock()
	defer r.jobsMu.Unlock()
	if r.vmRemoved == nil {
		r.vmRemoved = make(map[string]time.Time)
	}
	r.vmRemoved[strings.ToLower(name)] = time.Now()
}

// clearVMRemoved drops the stand-off for every VM the centre has just delivered.
// A VM back in the desired set is the centre asking for it again — an operator
// who re-created it under the same name, or a delete that was undone — and the
// agent honours the intent it was given rather than a hold it set itself.
func (r *runner) clearVMRemoved(vms []types.VM) {
	r.jobsMu.Lock()
	defer r.jobsMu.Unlock()
	for _, vm := range vms {
		delete(r.vmRemoved, strings.ToLower(vm.Meta.Name))
	}
}
