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

	// Establish identity if we have not confirmed a registration yet. This never
	// blocks: on failure we proceed on cached state and retry next cycle.
	if !r.registered {
		r.tryRegister(ctx, client, inv)
	}

	autonomous := false

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

	// Reconcile against the cached desired state. This runs whether or not the
	// centre was reachable above: the agent enforces the last intent it was
	// given. ObservedGeneration only advances when the spec is fully honoured.
	phase := types.PhasePending
	var conds []types.Condition
	if cached, ok, lerr := r.st.LoadDesiredHost(); lerr != nil {
		r.log.Error("read cached desired state failed", "err", lerr)
	} else if ok {
		res, rerr := r.reconciler.Reconcile(ctx, cached)
		phase, conds = res.Phase, res.Conditions
		if rerr != nil {
			r.log.Error("reconcile incomplete", "err", rerr)
		}
		if res.Honoured && r.observedGen != cached.Meta.Generation {
			r.observedGen = cached.Meta.Generation
			r.log.Info("generation honoured", "generation", r.observedGen)
		}
	}

	st := r.buildStatus(inv, autonomous, phase, conds)
	r.reportStatus(ctx, client, st)
}

// buildStatus assembles the status to report. ObservedGeneration carries the
// last fully-honoured generation, so the centre can tell when the host is
// settled even across an autonomy window.
func (r *runner) buildStatus(inv types.HostInventory, autonomous bool, phase types.Phase, conds []types.Condition) types.HostStatus {
	return types.HostStatus{
		Phase:              phase,
		ObservedGeneration: r.observedGen,
		Autonomous:         autonomous,
		LastContact:        time.Now().UTC(),
		Inventory:          inv,
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
