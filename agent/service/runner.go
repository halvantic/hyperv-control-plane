package main

import (
	"context"
	"time"

	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/agent/reconcile"
	"github.com/joshua-fourie/ballast/agent/store"
	ballastpb "github.com/joshua-fourie/ballast/api/proto"
	"github.com/joshua-fourie/ballast/api/types"
)

// runnerConfig is the agent's runtime configuration, independent of how the
// process was started (Windows service or console).
type runnerConfig struct {
	centreAddr string
	hostName   string
	storePath  string
	heartbeat  time.Duration
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
}

const agentVersion = "0.1.0-slice"

// run drives the agent until ctx is cancelled.
func (r *runner) run(ctx context.Context) error {
	// Adopt last-honoured state before touching the network, so a host that
	// reboots while the centre is offline comes straight back up enforcing the
	// intent it was last given.
	r.adoptCachedState()

	conn, err := grpc.NewClient(r.cfg.centreAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := ballastpb.NewAgentServiceClient(conn)

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

// cycle is one heartbeat. It collects inventory once, then in order:
// (best-effort) registers if not yet registered, pulls and caches desired
// state, reconciles (next step), and reports status. Every centre interaction
// is best-effort: a failure flips the agent to Autonomous and journals status
// locally for later replay, but the cycle still completes.
func (r *runner) cycle(ctx context.Context, client ballastpb.AgentServiceClient) {
	inv, err := r.hv.CollectInventory(ctx)
	if err != nil {
		r.log.Error("collect inventory failed", "err", err)
	}
	metrics, merr := r.hv.CollectMetrics(ctx)
	if merr != nil {
		r.log.Error("collect metrics failed", "err", merr)
	}
	resources, resErr := r.hv.CollectResources(ctx)
	if resErr != nil {
		r.log.Error("collect resources failed", "err", resErr)
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
	// can answer "unchanged" cheaply.
	known, _, _ := r.st.LoadDesiredHost()
	resp, err := client.PullDesiredState(ctx, &ballastpb.PullDesiredStateRequest{
		HostName:        r.cfg.hostName,
		Uid:             r.uid,
		KnownGeneration: known.Meta.Generation,
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
	r.reportStatus(ctx, client, st)

	// Cluster reconcile, only when the centre gave us a current assignment.
	if assignment != nil {
		cres, cerr := r.reconciler.ReconcileCluster(ctx, *assignment)
		if cerr != nil {
			r.log.Error("cluster reconcile incomplete", "err", cerr)
		}
		r.reportClusterStatus(ctx, client, assignment.Cluster, cres)
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

// runJobs executes each pending job locally and reports its outcome. It marks
// the job Running before executing so the centre stops re-delivering it.
func (r *runner) runJobs(ctx context.Context, client ballastpb.AgentServiceClient, jobs []*ballastpb.Job) {
	for _, pj := range jobs {
		job := ballastpb.JobFromProto(pj)
		r.reportJob(ctx, client, job.ID, types.JobRunning, "")
		msg, jerr := r.reconciler.ExecuteJob(ctx, job)
		if jerr != nil {
			r.log.Error("job failed", "id", job.ID, "kind", job.Kind, "err", jerr)
			r.reportJob(ctx, client, job.ID, types.JobFailed, jerr.Error())
			continue
		}
		r.log.Info("job done", "id", job.ID, "kind", job.Kind, "result", msg)
		r.reportJob(ctx, client, job.ID, types.JobSucceeded, msg)
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
		AssignedMemoryBytes: res.AssignedMemoryBytes,
		CPUUsagePercent:     res.CPUUsagePercent,
		UptimeSeconds:       res.UptimeSeconds,
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
