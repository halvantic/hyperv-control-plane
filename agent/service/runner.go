package main

import (
	"context"
	"sync"
	"time"

	"log/slog"

	"google.golang.org/grpc"

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

// resourceRefreshEvery throttles the expensive host-resource observation (switch
// details, volumes, ISO scan, management-vNIC topology with cluster-IP
// classification) to every N cycles instead of every cycle. These change rarely,
// so re-scanning them each 15s cycle is wasted work that inflates cycle time; the
// last result is reused in between. A job completion nudges an immediate cycle, so
// operator-driven changes still surface promptly.
const resourceRefreshEvery = 8

// netProfileRefreshEvery likewise throttles the network-location query (weakest
// connection-profile category), which changes only when domain reachability does.
const netProfileRefreshEvery = 4

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

	// lastResources / lastNetProfile cache the expensive observations that are
	// refreshed only every few cycles (see resourceRefreshEvery / netProfileRefreshEvery)
	// and reused in between, so a 15s cycle is not dominated by rescanning state
	// that rarely changes.
	lastResources  types.HostResources
	lastNetProfile string
	haveResources  bool
}

// requestNudge asks the main loop for an immediate follow-up cycle (non-blocking:
// if one is already queued, this is a no-op).
func (r *runner) requestNudge() {
	if r.nudge == nil {
		return
	}
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

	conn, err := grpc.NewClient(r.cfg.centreAddr, transportCreds(r.cfg.tlsDir, r.log))
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

// cycle is one heartbeat. It collects inventory once, then in order:
// (best-effort) registers if not yet registered, pulls and caches desired
// state, reconciles (next step), and reports status. Every centre interaction
// is best-effort: a failure flips the agent to Autonomous and journals status
// locally for later replay, but the cycle still completes.
func (r *runner) cycle(ctx context.Context, client ballastpb.AgentServiceClient) {
	r.cycles++
	inv, err := r.hv.CollectInventory(ctx)
	if err != nil {
		r.log.Error("collect inventory failed", "err", err)
	}
	metrics, merr := r.hv.CollectMetrics(ctx)
	if merr != nil {
		r.log.Error("collect metrics failed", "err", merr)
	}
	// Resources (switch details, volumes, ISO scan, vNIC topology) change rarely
	// and are the heaviest observation, so refresh them only every few cycles and
	// reuse the last result in between. A job nudge forces a fresh cycle anyway.
	resources := r.lastResources
	if !r.haveResources || r.cycles%resourceRefreshEvery == 0 {
		if res, resErr := r.hv.CollectResources(ctx); resErr != nil {
			r.log.Error("collect resources failed", "err", resErr)
		} else {
			resources = res
			r.lastResources = res
			r.haveResources = true
		}
	}
	identity, iderr := r.hv.GetHostIdentity(ctx)
	if iderr != nil {
		r.log.Error("get host identity failed", "err", iderr)
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
	if r.cycles%fullResyncEvery == 0 {
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
	var hyperVInstalled, rebootRequired bool
	if cached, ok, lerr := r.st.LoadDesiredHost(); lerr != nil {
		r.log.Error("read cached desired state failed", "err", lerr)
	} else if ok {
		res, rerr := r.reconciler.Reconcile(ctx, cached, secrets)
		phase, conds = res.Phase, res.Conditions
		hyperVInstalled, rebootRequired = res.HyperVInstalled, res.RebootRequired
		if rerr != nil {
			r.log.Error("reconcile incomplete", "err", rerr)
		}
		if res.Honoured && r.observedGen != cached.Meta.Generation {
			r.observedGen = cached.Meta.Generation
			r.log.Info("generation honoured", "generation", r.observedGen)
		}
	}

	st := r.buildStatus(inv, metrics, resources, autonomous, phase, conds, hyperVInstalled, rebootRequired)
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
	r.reportStatus(ctx, client, st)

	// Cluster reconcile, only when the centre gave us a current assignment. Every
	// member reconciles (so the clustering feature is ensured on all of them), but
	// only the former REPORTS cluster status: it is the single authority, which
	// avoids a last-writer-wins race between members clobbering each other's view
	// (e.g. one member observing groups, another reporting none).
	if assignment != nil {
		cres, cerr := r.reconciler.ReconcileCluster(ctx, *assignment)
		if cerr != nil {
			r.log.Error("cluster reconcile incomplete", "err", cerr)
		}
		if assignment.IsFormer {
			r.reportClusterStatus(ctx, client, assignment.Cluster, cres)
		}
	}

	// VM reconcile, against the cached set. This runs whether or not the centre
	// was reachable — the agent enforces the VMs it was last given, same as the
	// host spec. Reporting is best-effort and skipped when autonomous.
	r.reconcileVMs(ctx, client, autonomous)

	// Imperative jobs the centre queued for this host. Only run when the centre
	// is reachable — jobs are one-shot actions, never cached or replayed.
	if err == nil {
		r.runJobs(ctx, client, resp.GetJobs())
	}
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
		r.jobsMu.Unlock()

		go func(job types.Job) {
			defer func() {
				r.jobsMu.Lock()
				delete(r.jobsInflight, job.ID)
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
	if _, err := client.ReportJobResult(ctx, &ballastpb.ReportJobResultRequest{
		HostName: r.cfg.hostName, Uid: r.uid, JobId: id, State: string(state), Message: msg,
	}); err != nil {
		r.log.Warn("report job result failed", "id", id, "err", err)
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
		if res.PowerState == types.VMPowerRunning {
			if png, serr := r.hv.GetVMScreen(ctx, res.Name); serr != nil {
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

// buildStatus assembles the status to report. ObservedGeneration carries the
// last fully-honoured generation, so the centre can tell when the host is
// settled even across an autonomy window.
func (r *runner) buildStatus(inv types.HostInventory, metrics types.HostMetrics, resources types.HostResources, autonomous bool, phase types.Phase, conds []types.Condition, hyperVInstalled, rebootRequired bool) types.HostStatus {
	return types.HostStatus{
		Phase:              phase,
		ObservedGeneration: r.observedGen,
		HyperVInstalled:    hyperVInstalled,
		RebootRequired:     rebootRequired,
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
