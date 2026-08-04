package main

import (
	"context"
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

// jobTimeoutFor returns the cap for a job kind. Most finish in seconds, but
// storage rebuilds/repairs and live migrations legitimately run for many minutes
// (disk rebalance, VM+storage copy), so they get a longer cap rather than being
// killed mid-operation and left in a half-applied state.
func jobTimeoutFor(kind string) time.Duration {
	switch kind {
	case types.JobRebuildPool, types.JobRepairPool, types.JobMigrateVM, types.JobClusterMoveVM, types.JobFetchISO, types.JobVMExport:
		return 30 * time.Minute
	default:
		return jobTimeout
	}
}

// fullResyncEvery forces a full desired-state pull (ignoring the cached
// generation) every N cycles, so the agent self-heals from a reused/colliding
// generation rather than trusting an "unchanged" answer indefinitely.
const fullResyncEvery = 20

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
)

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
	lastIdentity   hyperv.HostIdentity
	haveIdentity   bool

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
			r.statusMu.Unlock()
			if st == nil || r.uid == "" {
				continue
			}
			if _, err := client.ReportStatus(ctx, &ballastpb.ReportStatusRequest{
				HostName: r.cfg.hostName,
				Uid:      r.uid,
				Status:   ballastpb.StatusToProto(*st),
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
	}()
	// A finished job asks (via requestNudge) for a fresh scan of the throttled
	// observations so its effect shows up now; read-and-clear it once for the
	// whole cycle. observeForce is the lighter VM-only variant the state watcher
	// raises — it refreshes the VM view without the resource/inventory rescan.
	force := r.forceScan.Swap(false)
	observeForce := r.forceObserveVMs.Swap(false)

	// Metrics (CPU/memory/uptime) are live and cheap — collect every cycle.
	doneMetrics := t.mark("metrics")
	metrics, merr := r.hv.CollectMetrics(ctx)
	doneMetrics()
	if merr != nil {
		r.log.Error("collect metrics failed", "err", merr)
	}
	// Physical hardware inventory does not change without a reboot, so refresh it
	// on a slow cadence — but always collect it before we are registered, since
	// registration reports it.
	inv := r.lastInventory
	if !r.haveInventory || force || !r.registered || r.cycles%inventoryRefreshEvery == 0 {
		doneInv := t.mark("inventory")
		got, ierr := r.hv.CollectInventory(ctx)
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
		res, resErr := r.hv.CollectResources(ctx)
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
		if serr := r.st.SaveDesiredVMs(vms); serr != nil {
			r.log.Error("persist desired vms failed", "err", serr)
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
	if cached, ok, lerr := r.st.LoadDesiredHost(); lerr != nil {
		r.log.Error("read cached desired state failed", "err", lerr)
	} else if ok {
		doneHost := t.mark("hostReconcile")
		res, rerr := r.reconciler.Reconcile(ctx, cached, secrets)
		doneHost()
		phase, conds = res.Phase, res.Conditions
		hyperVInstalled, rebootRequired = res.HyperVInstalled, res.RebootRequired
		inMaintenance = res.InMaintenance
		if rerr != nil {
			r.log.Error("reconcile incomplete", "err", rerr)
		}
		if res.Honoured && r.observedGen != cached.Meta.Generation {
			r.observedGen = cached.Meta.Generation
			r.log.Info("generation honoured", "generation", r.observedGen)
		}
	}

	st := r.buildStatus(inv, metrics, resources, autonomous, phase, conds, hyperVInstalled, rebootRequired, inMaintenance)
	st.ObservedVMs = r.observeVMs(ctx, force || observeForce)
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
			doneCluster := t.mark("clusterReconcile")
			cres, cerr := r.reconciler.ReconcileCluster(ctx, *assignment)
			doneCluster()
			if cerr != nil {
				r.log.Error("cluster reconcile incomplete", "err", cerr)
			}
			r.lastClusterGen = assignment.Cluster.Meta.Generation
			if assignment.IsFormer {
				r.reportClusterStatus(ctx, client, assignment.Cluster, cres)
			}
		}
	}

	// VM reconcile, against the cached set. This runs whether or not the centre
	// was reachable — the agent enforces the VMs it was last given, same as the
	// host spec. Reporting is best-effort and skipped when autonomous.
	doneVMs := t.mark("vmReconcile")
	r.reconcileVMs(ctx, client, autonomous)
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
			jctx, cancel := context.WithTimeout(ctx, jobTimeoutFor(job.Kind))
			defer cancel()
			r.reportJob(jctx, client, job.ID, types.JobRunning, "")
			// A long-running job (live migration) streams progress notes; surface
			// each as the job's Running message so the console shows a live %.
			onProgress := func(note string) {
				r.reportJob(jctx, client, job.ID, types.JobRunning, note)
			}
			msg, jerr := r.reconciler.ExecuteJob(jctx, job, onProgress)
			if jerr != nil {
				r.log.Error("job failed", "id", job.ID, "kind", job.Kind, "err", jerr)
				r.reportJob(jctx, client, job.ID, types.JobFailed, jerr.Error())
				return
			}
			r.log.Info("job done", "id", job.ID, "kind", job.Kind, "result", msg)
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
func (r *runner) reconcileVMs(ctx context.Context, client ballastpb.AgentServiceClient, autonomous bool) {
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

	results := r.reconciler.ReconcileVMs(ctx, cached)
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
		if res.PowerState == types.VMPowerRunning {
			sctx, scancel := context.WithTimeout(ctx, collectTimeout)
			png, serr := r.hv.GetVMScreen(sctx, res.Name)
			scancel()
			if serr != nil {
				r.log.Warn("vm screen capture failed", "vm", res.Name, "err", serr)
			} else {
				st.ScreenPNG = png
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
		CPUUsagePercent:     res.CPUUsagePercent,
		UptimeSeconds:       res.UptimeSeconds,
		Replication:         res.Replication,
		Conditions:          res.Conditions,
	}
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
		Groups:             res.Groups,
		CSVs:               res.CSVs,
		VMs:                res.VMs,
		Nodes:              res.Nodes,
		Pool:               res.Pool,
		Networks:           res.Networks,
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
	return types.HostStatus{
		Phase:              phase,
		ObservedGeneration: r.observedGen,
		HyperVInstalled:    hyperVInstalled,
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
	entry, err := r.st.AppendStatus(st, false)
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
		types.JobVMDiscardSavedState,
		types.JobVMApplyCheck, types.JobVMRemoveCheck:
		return true
	}
	return false
}

// vmBusy reports whether an imperative job is currently holding a VM, and which.
func (r *runner) vmBusy(name string) (string, bool) {
	r.jobsMu.Lock()
	defer r.jobsMu.Unlock()
	kind, ok := r.jobsVMs[strings.ToLower(name)]
	return kind, ok
}
