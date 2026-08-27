// Package reconcile drives actual host state towards the cached desired Host.
//
// It is the agent-side embodiment of the kubelet pattern from CLAUDE.md: it
// compares desired to actual and emits idempotent ensure-operations to close
// the gap, holding no host knowledge of its own — every host action goes
// through hyperv.Interface. Running it repeatedly with the same desired state
// converges and then no-ops, which is what lets the agent keep enforcing cached
// intent on a tight loop, including while the centre is offline.
//
// For this slice it reconciles networking: SET-backed virtual switches and the
// management OS vNICs layered on them. The host role, storage and cluster
// reconcilers follow the same shape.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"log/slog"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

// Reconciler converges a host towards desired state via hyperv.Interface.
type Reconciler struct {
	hv  hyperv.Interface
	log *slog.Logger

	// now is injectable so tests can pin condition timestamps.
	now func() time.Time

	// phase records how long a named stage of the cycle took, so a slow one can
	// name its own slowest part rather than leaving it to be guessed at.
	//
	// The cycle's phases are timed by the runner, which can only see the calls it
	// makes: hostReconcile, clusterReconcile, journal, deliver. That was enough
	// until clusterReconcile itself became the slow one — on the rig 2026-08-24
	// four of five hosts were CUT OFF at the 5-minute cap with clusterReconcile
	// taking 3m20s-3m48s of it, of which named host calls explained barely 90
	// seconds. Two and a half minutes a pass, invisible, on every host.
	//
	// So the reconciler records its own sub-phases through this. Nil is a no-op,
	// which is what every test and the stub get; the runner supplies the real one.
	phase PhaseRecorder

	// lastStorageAddresses is the storage vNIC addresses from the most recent
	// host desired state, kept so the CLUSTER pass can bind its iSCSI sessions.
	//
	// The two passes are separate calls with separate inputs — the cluster
	// assignment carries a Cluster, not the Host — and the runner always runs the
	// host pass first in a cycle, so this is last-pass state rather than a
	// guess. Empty until a host spec has been seen, which reads as "bind
	// nothing" and is the behaviour every host had before pinning existed.
	lastStorageAddresses []string

	// volumeSources is the declared LUN behind each cluster volume, from the most
	// recent cluster pass. The ADOPT job needs it: the reconcile refuses a LUN
	// that has contents, and the operator's decision arrives later as a job that
	// names only the volume. Keeping the serial here rather than in the job is
	// what stops the job becoming a second authority over which disk is meant.
	volumeSources map[string]types.CSVSourceSpec

	/* importScan is the last scan for unregistered VMs on this host's storage,
	   held so the reconcile can REPORT it without re-running it.

	   The scan is a job, not a step: Compare-VM loads every configuration it
	   finds, so the cost scales with somebody else's data rather than with
	   anything Ballast controls. Running it each pass would put an unbounded
	   cost in the loop for an answer that changes only when a volume is adopted
	   or a VM is imported.

	   So the job scans and stores; every pass afterwards reports what it found,
	   with the scan record saying when. A result presented without its age is
	   this codebase's most expensive bug class, which is why the record is a
	   sibling field and not an afterthought. */
	// migrationPasses is the last VMware copy pass this host ran for each
	// migration in flight on it, reported in status so the centre can resume the
	// next pass from the per-disk change markers.
	migrationMu     sync.Mutex
	migrationPasses []types.MigrationPassResult

	importScanMu     sync.Mutex
	importVMs        []types.ImportableVM
	importScan       *types.ImportScanStatus
	importRootsCache []string

	// iscsiReadBudget overrides the cap on the iSCSI step, for tests. Zero uses
	// iscsiStepBudget.
	iscsiReadBudget time.Duration

	// transients tracks how long each condition has been failing with a known
	// in-flight signature, so a settling operation reads as progressing but a
	// stuck one still escalates to a real failure. See transient.go.
	transients *transientTracker

	// vmBusy reports that an imperative job is operating on a VM right now, and
	// with what. The reconciler stands off that VM until the job finishes.
	//
	// A job that moves or copies a VM's files takes exclusive hold of it: Hyper-V
	// refuses Set-VM during a storage migration, a clone holds the VHDX, a
	// capture needs the disk quiet. Reconciling regardless produced ApplyFailed
	// every 15 seconds and marked the VM Degraded for the whole of an operation
	// that was succeeding — red for work in progress, which is how people learn
	// to ignore red.
	//
	// Deliberately not another transient error signature: those match on error
	// text and expire after two minutes, while a storage migration is a
	// half-hour of legitimate work. The reconciler knowing what its own agent
	// started beats teaching it to recognise how each conflict happens to fail.
	vmBusy func(name string) (kind string, busy bool)

	// vmFull caches the last complete observation per VM (keyed lower-cased), so
	// a pass that only took the cheap live reading still reports full status.
	//
	// The full read — guest OS via KVP/XML, checkpoints, a Get-VHD per disk, a
	// VLAN query per adapter — is ~1.4s per VM and almost all of what it returns
	// cannot change while the VM runs. See shouldReadFullVMState for when it is
	// worth spending.
	vmFull map[string]hyperv.VMState
	// vmFullPower is the power state observed at the last full read. A power
	// cycle is when deferred configuration changes actually land, so a change
	// here is the signal to look again.
	vmFullPower map[string]types.VMPowerState

	// passes counts completed reconcile passes, so an observation that is
	// expensive and rarely changes can run on a slow cadence instead of every
	// cycle. See maintenanceDeepEvery.
	passes uint64

	// hostDraining is what the last host pass observed about this node: the
	// cluster is moving its roles off. VMs are reconciled in a separate call, and
	// a VM that has already left is not readable here — which is the drain
	// working, not a fault. See the drain branch in reconcileVM.
	hostDraining bool

	// replicaSettled is set once EnsureReplicaServer has reported a pass with
	// nothing to change. While it holds, the step runs on replicaServerEvery
	// instead of every pass; anything that could make it wrong clears it. See
	// the call site.
	replicaSettled bool
	// replicaCond is the last condition the step produced, replayed on the
	// passes it is skipped. A step that stops reporting because it was skipped
	// would read as a step that vanished, and absent is not the same as settled.
	replicaCond types.Condition
	// replicaGen is the host generation the last real run honoured, so an
	// operator edit puts the step back on every pass.
	replicaGen int64

	// clusterReadBudget overrides how long a cluster STATE read may take before
	// it is abandoned. Zero uses the package default; tests shorten it so they do
	// not wait a real minute to prove a hang is given up on.
	clusterReadBudget time.Duration

	// roleInstalled caches that the Hyper-V role is present, so the expensive
	// feature read is not repeated every pass to re-learn a fact that cannot
	// change on its own. See hostRoleEvery.
	roleInstalled bool

	// vnicsSettled, vnicConds and vnicGen throttle the management vNIC step once
	// every declared vNIC is already right. Same rule as the replica server: the
	// cadence applies only to a step that reported nothing to change.
	vnicsSettled bool
	vnicConds    []types.Condition
	vnicGen      int64
}

// mgmtVNICEvery is how often a SETTLED set of management vNICs is re-observed.
//
// Observing them is the single largest consistent cost in the fleet: 7.3-8.6s of
// every pass on every host, measured 2026-08-13, which is about a quarter of a
// 32s pass. It is already batched into one PowerShell invocation — that work is
// done and the comments on vnicsBatchScript record it — and the residue is not
// the loop. Cold-versus-warm in one process was Get-ClusterResource 1976ms then
// 31ms, Get-NetIPAddress 654ms then 24ms: what costs is the FIRST call into each
// module (Hyper-V, NetTCPIP, DnsClient, FailoverClusters), and one invocation
// already pays that exactly once. Hoisting the four remaining per-vNIC cmdlets
// out of the loop would save roughly 24ms x 3 vNICs x 4 calls — a third of a
// second out of seven.
//
// So the lever is not making the observation cheaper, it is not making it as
// often. A management vNIC's switch, VLAN and IP change when an operator edits
// them or when something drifts, and an edit bumps the generation, which resets
// this. What remains is drift caught within ~4 minutes instead of ~30s, on a
// fleet where every other rarely-changing observation is already tiered the same
// way.
const mgmtVNICEvery = 8

// hostRoleEvery is how often an already-installed Hyper-V role is re-checked.
// Deliberately the same slow cadence as the maintenance storage read: both
// answer a question that only changes when something deliberate happens, and
// both cost a great deal to ask.
const hostRoleEvery = 40

// replicaServerEvery is how often a SETTLED replica-server configuration is
// re-checked.
//
// The step is a write path that almost always writes nothing, and finding that
// out is expensive: on a cluster member it runs Get-Service ClusSvc, then
// Get-ClusterResource to find the Replica Broker, then Get-VMReplicationServer,
// then a Get-ClusterSharedVolume scan when the storage path is on a CSV. Cluster
// cmdlets are the slow kind. Measured across the rig it was 4.4-8.3s of every
// pass on every host, and 2m44s of a 4m13s pass on a member whose cluster was
// busy — two thirds of a pass, against a five-minute cycle cap, on the one class
// of host whose readings the whole cluster view depends on.
//
// It is NOT a plain cadence, because the comment this replaces is load-bearing:
// on a member waiting for its cluster's Replica Broker, retrying every pass IS
// the convergence mechanism. So the cadence applies only once the step has
// reported that it changed nothing. An error, a change, or an edit puts it back
// on every pass immediately, which is exactly where the every-pass retry was
// earning its cost.
const replicaServerEvery = 8

// maintenanceDeepEvery is how often the node-maintenance check does its full
// STORAGE read rather than only reading the node's cluster state.
//
// The storage half asks whether this node's physical disks are still marked out
// of the pool, and answering it means Get-StorageFaultDomain plus Get-PhysicalDisk
// — which in an S2D cluster enumerates every disk in the CLUSTER, not this host's.
// It ran on every pass, for every member, at the heartbeat. Measured on HVNEW02
// after a reboot it was 3m53s of a 4m27s pass: 87% of the pass spent asking a
// question whose answer had not changed and, for an unpaused node with no
// maintenance declared, could not have.
//
// It is not dropped, because it is how wreckage from the old manual
// Enable-StorageMaintenanceMode is found on a node that resumed long ago. It is
// simply asked at a rate that matches how often it can change. The cases where the
// answer is live — entering maintenance, a paused node, or a pass that just
// resumed one — still read it every time, so nothing that acts on storage acts on
// a cached answer.
const maintenanceDeepEvery = 40

// SetVMBusy wires the "is a job operating on this VM" lookup. Without one the
// reconciler behaves as before and reconciles everything.
func (r *Reconciler) SetVMBusy(f func(name string) (string, bool)) { r.vmBusy = f }

// New returns a Reconciler driving the given host interface.
func New(hv hyperv.Interface, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{hv: hv, log: log, now: func() time.Time { return time.Now().UTC() }, transients: newTransientTracker()}
}

// Result is the outcome of one reconcile pass, for the agent to fold into the
// status it reports.
type Result struct {
	// Phase is Ready when every piece of desired state was honoured, Degraded
	// when an ensure-operation failed, Progressing when the host is mid-change
	// (for example awaiting a reboot to complete a role install).
	Phase types.Phase

	// Honoured is true only when the whole desired spec was applied successfully.
	// The agent advances ObservedGeneration to the desired Generation only then;
	// a failed pass must not claim intent it did not achieve.
	Honoured bool

	// Changed is true when this pass actually mutated host state (a create or
	// update). A converged host produces Changed == false, the steady state.
	Changed bool

	// HyperVInstalled reflects the observed Hyper-V role state.
	HyperVInstalled bool

	// ISCSI is this standalone host's own array connection, nil when it declares
	// none or is a cluster member (whose iSCSI is reported on the cluster).
	ISCSI *types.ISCSIStatus

	// RebootRequired is true when the spec cannot be fully honoured until a
	// reboot and RebootPolicy forbids the agent rebooting autonomously.
	RebootRequired bool

	// InMaintenance is true when the host is actually out of service: the
	// declared intent is honoured AND, for a cluster member, its roles have
	// finished moving off. Draining says the move is still under way — paused but
	// not yet safe to reboot.
	InMaintenance bool
	Draining      bool

	// Conditions records one machine-readable fact per reconciled resource.
	Conditions []types.Condition
}

// Reconcile makes the host match desired and returns what it observed. Switches
// are ensured before the management vNICs that depend on them. It does not stop
// at the first failure: it attempts every resource so status reflects the whole
// host, and reports Honoured == false if any failed.
func (r *Reconciler) Reconcile(ctx context.Context, desired types.Host, secrets map[string]types.Secret) (res Result, err error) {
	// Pass timing is NOT done here. It used to be, and the timer covered this
	// function while the drain also swept up the cluster reconcile the runner runs
	// afterwards — so one cycle's GetClusterState was reported as the next cycle's
	// cost. The timer and the drain now sit together at the cycle boundary in the
	// runner, which is the only place that spans everything a pass does. See
	// timing.go for the invariant.
	defer func() { r.passes++ }()
	// Recorded before anything is applied, so the cluster pass that follows binds
	// its sessions to what the host was TOLD to have. Recording it after would
	// make a vNIC that failed to apply silently stop being bound to, which is the
	// pass where the operator most needs to see the path missing.
	r.lastStorageAddresses = desired.Spec.Networking.StorageAddresses()
	var (
		conds           []types.Condition
		changed         bool
		failures        int
		firstErr        error
		hyperVInstalled bool
		inMaintenance   bool
		draining        bool
	)

	// DNS comes before EVERYTHING, because the domain join in the identity step
	// cannot work without it and cannot fix it either.
	//
	// A host joins a domain by resolving its SRV records, so DNS has to point at
	// the DC first. This step used to sit below the identity step, where it was
	// unreachable in exactly the case that needed it: identity attempts the join,
	// the join fails because DHCP handed the host the firewall as its resolver,
	// identity returns early with a condition — and DNS is never applied. The next
	// pass does the same, and the one after that. A freshly onboarded host on DHCP
	// could never join, and the only way out was an operator running Fix DNS by
	// hand, which is the intervention Ballast exists to remove.
	//
	// The old comment on this block already said "applied before a join". The
	// intent was right; the placement made it false.
	//
	// Best-effort: a failure surfaces as a condition without degrading the host.
	if dns := desired.Spec.Networking.DNSServers; len(dns) > 0 {
		out, err := r.hv.EnsureHostDNS(ctx, dns)
		conds = append(conds, r.advisoryCondition("HostDNS", out, err))
		if err != nil {
			r.log.Warn("set host dns failed (best-effort, not degrading)", "err", err)
		} else if out != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("host dns reconciled", "dns", dns, "outcome", out)
		}
	}

	// The Windows EDITION comes before everything else that configures the host.
	//
	// A staged conversion replaces the operating system on the next boot, so
	// configuring a host and then converting it underneath means reconciling
	// against something about to be replaced. It is also the step most likely to
	// be refused outright — an evaluation converts only to its own retail or
	// volume equivalent — and finding that out first is cheaper than finding it
	// out after a domain join.
	if edRes, done, err := r.reconcileWindowsEdition(ctx, desired, secrets); done || err != nil || len(edRes.Conditions) > 0 {
		conds = append(conds, edRes.Conditions...)
		changed = changed || edRes.Changed
		if done || err != nil {
			edRes.Conditions = conds
			return edRes, err
		}
	}

	// Activation follows the edition, and only on a pass the edition did not stop.
	// A host with a conversion staged is about to become a different edition, and
	// activating the one it is leaving would spend a MAK seat on an installation
	// that ceases to exist at the next restart.
	conds = append(conds, r.reconcileWindowsActivation(ctx, desired, secrets)...)

	// Identity comes next: the host should have its final name, management IP
	// and domain before the role and networking are configured. Rename/domain
	// changes need a reboot governed by RebootPolicy, so like the role step this
	// may stop the pass early and resume after the host comes back.
	if idRes, done, err := r.reconcileIdentity(ctx, desired, secrets); done || err != nil || len(idRes.Conditions) > 0 {
		conds = append(conds, idRes.Conditions...)
		changed = changed || idRes.Changed
		if done || err != nil {
			idRes.Conditions = conds
			return idRes, err
		}
	}

	// Host role comes first: virtual switches cannot exist without the Hyper-V
	// role, and installing it may need a reboot governed by RebootPolicy. If the
	// role is not yet active we cannot honour networking this pass, so we return
	// early rather than letting downstream operations fail.
	if desired.Spec.EnableHyperVRole {
		res, done, err := r.reconcileHostRole(ctx, desired)
		conds = append(conds, res.Conditions...)
		if done || err != nil {
			res.Conditions = conds
			return res, err
		}
		hyperVInstalled = res.HyperVInstalled
		changed = changed || res.Changed
	}

	// Default VM/VHD storage paths. Pointing these at a CSV makes VMs land on
	// shared storage so they can migrate. Needs the Hyper-V role, so it follows
	// the role step.
	if st := desired.Spec.Storage; st.DefaultVMPath != "" || st.DefaultVHDPath != "" {
		out, err := r.hv.EnsureVMHostPaths(ctx, st.DefaultVMPath, st.DefaultVHDPath)
		conds = append(conds, r.advisoryCondition("VMHostPaths", out, err))
		if err != nil {
			// Best-effort: the default VM/VHD path is a convenience for where new
			// VMs land — VMs themselves carry explicit paths. Some hosts can't set
			// it to a CSV they don't coordinate (Set-VMHost rejects the path on a
			// non-owner node), so a failure is surfaced as a condition but does NOT
			// degrade the host or hold its generation back.
			r.log.Warn("set vm host paths failed (best-effort, not degrading)", "err", err)
		} else if out != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("vm host paths reconciled", "vmPath", st.DefaultVMPath, "vhdPath", st.DefaultVHDPath, "outcome", out)
		}
	}

	// This host's own iSCSI array, for a standalone host backing its VMs with
	// LUNs. A cluster member is refused inside rather than skipped here, so an
	// operator who declares both is told why one is ignored instead of watching
	// an edit do nothing.
	iscsiStatus, iscsiConds := r.reconcileHostISCSI(ctx, desired, secrets)
	conds = append(conds, iscsiConds...)

	// Live-migration configuration (enable/auth/networks). A VM only migrates
	// cleanly when this is set up on every host.
	if lm := desired.Spec.LiveMigration; lm != nil {
		out, err := r.hv.EnsureLiveMigration(ctx, *lm)
		conds = append(conds, r.condition("LiveMigration", out, err))
		if err != nil {
			failures++
			if firstErr == nil {
				firstErr = fmt.Errorf("configure live migration: %w", err)
			}
			r.log.Error("configure live migration failed", "err", err)
		} else if out != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("live migration reconciled", "outcome", out)
		}
	}

	// Hyper-V Replica server: accept inbound replica traffic when declared
	// (fanned from a cluster's ReplicaBroker, or set directly on a standalone
	// replica target host).
	// A failure here is usually a wait (the cluster's Replica Broker still
	// provisioning), so it surfaces on the condition and retries each pass
	// without degrading the host.
	if rs := desired.Spec.ReplicaServer; rs != nil {
		// An operator edit can change what this step must write, so a new
		// generation always reads for real.
		if desired.Meta.Generation != r.replicaGen {
			r.replicaSettled = false
		}
		if r.replicaSettled && r.passes%replicaServerEvery != 0 {
			// Skipped, not silent: replay the last condition so the step keeps its
			// place and its state in the console. A step that disappears on the
			// passes it was skipped reads as a step that stopped being reconciled.
			conds = append(conds, r.replicaCond)
		} else {
			out, err := r.hv.EnsureReplicaServer(ctx, *rs)
			c := r.condition("ReplicaServer", out, err)
			conds = append(conds, c)
			r.replicaCond, r.replicaGen = c, desired.Meta.Generation
			// Settled only on a clean pass that changed nothing. A wait for the
			// cluster's Replica Broker is an error here, and that is exactly the case
			// the every-pass retry exists for — so it stays on every pass until it
			// stops failing.
			r.replicaSettled = err == nil && out == hyperv.OutcomeUnchanged
			if err != nil {
				r.log.Warn("configure replica server failed (retries next pass)", "err", err)
			} else if out != hyperv.OutcomeUnchanged {
				changed = true
				r.log.Info("replica server reconciled", "outcome", out)
			}
		}
	}

	net := desired.Spec.Networking

	for _, sw := range net.Switches {
		out, err := r.hv.EnsureSwitch(ctx, sw)
		conds = append(conds, r.condition("Switch/"+sw.Name, out, err))
		if err != nil {
			failures++
			if firstErr == nil {
				firstErr = fmt.Errorf("ensure switch %q: %w", sw.Name, err)
			}
			r.log.Error("ensure switch failed", "switch", sw.Name, "err", err)
			continue
		}
		if out != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("switch reconciled", "switch", sw.Name, "outcome", out)
		}
	}

	// Resolve each vNIC's effective spec first, then reconcile the whole set in
	// one call. The vNICs are observed together (one PowerShell invocation for
	// all of them instead of two each) while applies stay per vNIC, so a bad spec
	// on one still fails on its own and reports against its own condition.
	vnics := make([]types.ManagementVNICSpec, 0, len(net.ManagementVNICs))
	for _, v := range net.ManagementVNICs {
		// DNS belongs on the management vNIC — the NIC that carries the routable
		// static IP. If the vNIC declares no DNS of its own, apply the host-level
		// DNS servers so the reconcile SETS DNS on it instead of clearing it (which
		// would strand DNS on stray no-IP vNICs). Copy the config so the shared
		// cached desired is not mutated.
		//
		// A DECLARED GATEWAY is what makes a vNIC routable, and inheriting must be
		// limited to those. An isolated fabric vNIC (storage, live migration)
		// declares an address and nothing else, which is indistinguishable here
		// from "management vNIC that just did not spell its DNS out" unless the
		// gateway is tested — and EnsureHostDNS independently CLEARS DNS on any
		// interface with no default route, because DNS there publishes an
		// unreachable A record and makes clustering treat the network as
		// client-facing.
		//
		// Without this test the two reconcilers contradict each other on the same
		// interface every pass: this one sets DNS (and to do so runs applyIPScript,
		// which removes and re-adds the address), EnsureHostDNS clears it, and both
		// truthfully report Updated for ever. Observed on the rig 2026-08-06: the
		// storage vNIC had NO address for ~30s of every ~45s cycle on all three
		// nodes, which partitioned the cluster network, paused the CSVs and failed
		// the pool resources. Idempotency is the non-negotiable that was breached;
		// the cluster damage was the symptom.
		if v.IPConfig != nil && v.IPConfig.Gateway != "" && len(v.IPConfig.DNSServers) == 0 && len(net.DNSServers) > 0 {
			cfg := *v.IPConfig
			cfg.DNSServers = append([]string(nil), net.DNSServers...)
			v.IPConfig = &cfg
		}
		vnics = append(vnics, v)
	}
	if len(vnics) > 0 {
		if desired.Meta.Generation != r.vnicGen {
			r.vnicsSettled = false
		}
		if r.vnicsSettled && r.passes%mgmtVNICEvery != 0 {
			// Settled: replay the last conditions rather than dropping the steps.
			conds = append(conds, r.vnicConds...)
		} else {
			outs, errs := r.hv.EnsureMgmtVNICs(ctx, vnics)
			settled := true
			r.vnicConds = r.vnicConds[:0]
			for i, v := range vnics {
				out, err := outs[i], errs[i]
				c := r.condition("ManagementVNIC/"+v.Name, out, err)
				conds = append(conds, c)
				r.vnicConds = append(r.vnicConds, c)
				if err != nil {
					settled = false
					failures++
					if firstErr == nil {
						firstErr = fmt.Errorf("ensure vNIC %q: %w", v.Name, err)
					}
					r.log.Error("ensure management vNIC failed", "vnic", v.Name, "err", err)
					continue
				}
				if out != hyperv.OutcomeUnchanged {
					settled = false
					changed = true
					r.log.Info("management vNIC reconciled", "vnic", v.Name, "outcome", out)
				}
			}
			// Settled only when EVERY vNIC was already right. One that still needs
			// work keeps the whole set on every pass — they are observed together,
			// so there is nothing to gain from throttling part of it.
			r.vnicsSettled, r.vnicGen = settled, desired.Meta.Generation
		}
	}

	// Whether the redundancy is real. Reported even on a settled pass, because a
	// vNIC that has been correctly applied to the wrong uplink is settled and
	// wrong, and nothing else on the host will ever say so.
	conds = append(conds, storageNetworkCondition(net, wantsMultipathStorage(desired), r.now)...)

	// Prune stray management-OS vNICs: on a switch we manage, remove any management
	// vNIC not in the declared set that carries no real IPv4 at all (APIPA only) —
	// an auto or leftover vNIC from an earlier switch/cluster iteration. A vNIC
	// holding ANY non-APIPA address survives, whatever its origin: manual IPs are
	// the operator's, DHCP leases are live, and the failover cluster's IP resource
	// lands on a vNIC with PrefixOrigin 'Other'. Only runs where we manage a switch.
	if len(net.Switches) > 0 {
		switches := make([]string, 0, len(net.Switches))
		for _, sw := range net.Switches {
			switches = append(switches, sw.Name)
		}
		keep := make([]string, 0, len(net.ManagementVNICs))
		for _, v := range net.ManagementVNICs {
			keep = append(keep, v.Name)
		}
		out, dups, err := r.hv.PruneManagementVNICs(ctx, switches, keep)
		conds = append(conds, r.advisoryCondition("PruneVNICs", out, err))
		// A DUPLICATE of a declared vNIC is invisible to everything else.
		//
		// Hyper-V allows two management OS vNICs with the same name on one switch;
		// only the network adapter alias is disambiguated ("vEthernet (Name) 2").
		// So the prune above skips the duplicate — its name is in the keep set —
		// and every alias built by concatenation addresses whichever one Windows
		// lists first. HVNEW02 on 2026-08-24 carried a second ConvergedSwitch vNIC
		// on a DHCP lease of 192.168.1.177 while the declared one held .72, the
		// cluster used the .177 interface, and Ballast reported the host settled.
		//
		// Reported, never removed automatically: both carry a real address, and
		// deleting the one the cluster is actually using takes the node off its
		// heartbeat network. That is an operator's call.
		if len(dups) > 0 {
			conds = append(conds, types.Condition{
				Type: "DuplicateVNICs", Status: true, Reason: "Duplicated",
				Message: "this host has more than one management vNIC with the same name on a managed switch, which Hyper-V allows and nothing else reports: " +
					strings.Join(dups, "; ") +
					". Only the network adapter name is disambiguated, so the declared address and the one the cluster uses can be on different adapters. Remove whichever is not carrying the declared address",
				LastTransitionTime: r.now(),
			})
		}
		if err != nil {
			r.log.Warn("prune stray management vNICs failed (best-effort)", "err", err)
		} else if out != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("pruned stray management vNIC(s)")
		}
	}

	// Keep managed host NICs off the Public network profile. Creating or adjusting
	// a vSwitch/vNIC (just done above) frequently strands an interface on Public,
	// which silently blocks inbound WinRM and failover clustering. Running this on
	// every pass — not just as an operator-triggered job — makes the host heal
	// itself, and it works while the centre is offline. This is host hygiene, not
	// desired spec, so it is best-effort: surface a condition, never degrade the
	// host or hold its generation back.
	//
	// It runs where the agent manages networking (a switch or management vNIC is
	// declared) AND on any cluster member. A member's NIC on Public silently
	// blocks the cluster and SMB traffic that carries CSV I/O, whoever created
	// the adapter — observed live as storage and live-migration vNICs flipping to
	// Public, which took a CSV degraded and surfaced as four unrelated-looking
	// failures (an undreadable VHDX path, a provider error, a denied directory
	// creation). Gating that hygiene on Ballast having authored the switch left
	// the hosts that most need it doing nothing.
	if len(net.Switches) > 0 || len(net.ManagementVNICs) > 0 || desired.Spec.ClusterMembership != nil {
		out, err := r.hv.EnsureNetworkProfilesPrivate(ctx)
		conds = append(conds, r.advisoryCondition("NetworkProfile", out, err))
		if err != nil {
			r.log.Warn("ensure network profiles private failed (best-effort, not degrading)", "err", err)
		} else if out != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("network profile reconciled (Public -> Private)", "outcome", out)
		}
	}

	// Build the set of NICs that are teamed into a vSwitch. Once a NIC is a SET
	// team member it has no independent IP interface — the IP lives on the
	// management OS vNIC on that switch. Trying to call New-NetIPAddress on a
	// teamed NIC returns "Element not found" (error 1168).
	teamedNICs := make(map[string]bool)
	for _, sw := range net.Switches {
		for _, m := range sw.TeamMembers {
			teamedNICs[m] = true
		}
	}

	for _, c := range net.NICConfigs {
		if teamedNICs[c.AdapterName] {
			r.log.Info("skipping NIC IP config: adapter is a SET team member", "adapter", c.AdapterName)
			conds = append(conds, r.condition("NICConfig/"+c.AdapterName, hyperv.OutcomeUnchanged, nil))
			continue
		}
		out, err := r.hv.EnsureHostIP(ctx, c)
		conds = append(conds, r.condition("NICConfig/"+c.AdapterName, out, err))
		if err != nil {
			failures++
			if firstErr == nil {
				firstErr = fmt.Errorf("ensure NIC IP %q: %w", c.AdapterName, err)
			}
			r.log.Error("ensure NIC IP failed", "adapter", c.AdapterName, "err", err)
			continue
		}
		if out != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("NIC IP reconciled", "adapter", c.AdapterName, "outcome", out)
		}
	}

	// Maintenance is desired state, so it is re-asserted every pass rather than
	// applied once by a job. That is what makes it survive: a node that reboots
	// and rejoins its cluster comes back paused if the operator still wants it
	// paused, and it stays paused while the centre is unreachable.
	//
	// Draining is not instant, and the difference matters. The node is paused the
	// moment it is asked, but it is not OUT OF SERVICE until its roles have moved.
	// Reporting maintenance before then would tell an operator it is safe to
	// reboot a node still running their VMs.
	// Desired state decides, on every pass, with nothing remembered between them.
	//
	// This used to consult a REMEMBERED "did the last pass want maintenance", so a
	// resume only happened if the same agent process had seen the drain. Clearing
	// maintenance while the agent was restarting — an agent update, a reboot —
	// left the node paused for ever, because nothing afterwards knew to undo it.
	// Any state that has to survive a restart belongs in desired state, and it
	// already does.
	//
	// So: declared means Enter, not declared means Exit. Exit is a no-op on a node
	// that is not paused, which makes it safe to send every cycle.
	//
	// The corollary is that a node paused outside Ballast IS resumed, and that is
	// the right call however uncomfortable. The alternative — leaving it paused
	// and labelling it "paused outside Ballast" — asserted something the centre
	// cannot know (Ballast's own drain JOB paused nodes without recording it, and
	// so did a failed resume), and it left the operator with a node nothing would
	// ever fix. Desired state wins here exactly as it does for a switch or an IP;
	// the way to keep a node paused is to declare maintenance.
	wantMaintenance := desired.Spec.Maintenance != nil && desired.Spec.Maintenance.Enabled
	intent := hyperv.MaintenanceExit
	if wantMaintenance {
		intent = hyperv.MaintenanceEnter
	}
	{
		// The storage half of this check is a cluster-wide read; ask for it when it
		// can matter. Entering maintenance always reads it (the script needs the
		// truth before it acts), a paused node is read inside the script regardless,
		// and otherwise it runs on the slow cadence — see maintenanceDeepEvery.
		// Never on pass 0. A rebooted host is a fresh agent process, so pass 0 is
		// the FIRST pass after boot — the one moment the cluster's storage is
		// busiest and the one pass an operator is waiting on. Measured on HVNEW02:
		// 1m43s of a 3m54s first pass, spent on the read this gate exists to
		// avoid. Wreckage detection is looking for damage left days ago; it can
		// wait for pass 40.
		deep := wantMaintenance || (r.passes > 0 && r.passes%maintenanceDeepEvery == 0)
		out, ms, err := r.hv.EnsureNodeMaintenance(ctx, desired.Meta.Name, intent, deep)
		// Only report when something actually happened or is wrong. Exit runs every
		// cycle on every host and is a no-op almost always; a condition each time
		// would be pure noise.
		if out != hyperv.OutcomeUnchanged || err != nil || ms.StorageError != "" || ms.Blocked != "" {
			c := r.condition("Maintenance", out, err)
			// The node is back in service but its disks are still marked out of the
			// pool, and Ballast could not put them back. The cluster releases them
			// on resume, so this is wreckage from the old manual enable — and it
			// matters, because it holds every space degraded, and a degraded space
			// is what makes the cluster refuse to pause the node at all. Retried
			// every pass, but the operator needs to know why the next drain will
			// not move. Never set for a node that is merely paused: disks out is
			// the normal drained state.
			if err == nil && ms.StorageError != "" {
				c.Status = false
				c.Reason = "StorageStranded"
				c.Message = ms.StorageError +
					" — retrying each pass. Clearing it by hand is Disable-StorageMaintenanceMode on the affected physical disks."
			}
			// The cluster refused the drain, and it was right to. Reported as
			// WaitingForStorage rather than as a failure: the request stands, the
			// node is healthy, nothing is wrong, and the drain begins by itself when
			// the pool is. "ApplyFailed" on a healthy node invites forcing it, and
			// forcing it removes a copy the pool still needs.
			if err == nil && ms.Blocked != "" {
				c.Status = false
				c.Reason = "WaitingForStorage"
				c.Message = ms.Blocked
			}
			conds = append(conds, c)
		}
		switch {
		case err != nil:
			failures++
			if firstErr == nil {
				firstErr = fmt.Errorf("ensure maintenance: %w", err)
			}
			r.log.Error("ensure node maintenance failed", "want", wantMaintenance, "err", err)
		default:
			if out != hyperv.OutcomeUnchanged {
				changed = true
				r.log.Info("node maintenance reconciled", "want", wantMaintenance, "outcome", out)
			}
			// Report what is TRUE, not what was asked. A standalone host has
			// nothing to pause, so holding the intent is honouring it; a member is
			// out of service only once its roles have actually gone. A node paused
			// by someone else still reports paused — the console shows that as a
			// divergence rather than pretending it is in service.
			// On an S2D cluster the node is only really out of service once its
			// STORAGE is out too — until then the pool is repairing around disks
			// that have not actually gone, and rebooting would make that real.
			inMaintenance = (!ms.IsMember && wantMaintenance) ||
				(ms.Paused && !ms.Draining && (ms.StorageOut || !wantMaintenance))
			draining = ms.Draining
			// VMs reconcile in a separate call, after this one, and need to know
			// their roles are being moved off — a VM that has already gone is not
			// a VM that failed.
			r.hostDraining = ms.Draining
		}
	}
	// What this pass actually cost, reported only when it cost enough to matter.
	//
	// The pass's own cost is appended by the deferred timer at the top, so it is
	// recorded on every return path rather than only this one. See there for why.
	res = Result{
		InMaintenance:   inMaintenance,
		Draining:        draining,
		Conditions:      conds,
		Changed:         changed,
		Honoured:        failures == 0,
		HyperVInstalled: hyperVInstalled,
		ISCSI:           iscsiStatus,
	}
	if failures == 0 {
		res.Phase = types.PhaseReady
	} else {
		res.Phase = types.PhaseDegraded
	}
	return res, firstErr
}

// reconcileIdentity drives the host's day-0 identity towards desired: a static
// management IP on a physical adapter (no reboot), then the computer name (a
// reboot governed by RebootPolicy). It returns done == true when the caller
// should stop and report the Result without proceeding to role/networking —
// either a rename is awaiting/performing a reboot, or an error occurred.
// done == false with no error means identity is settled.
func (r *Reconciler) reconcileIdentity(ctx context.Context, desired types.Host, secrets map[string]types.Secret) (Result, bool, error) {
	spec := desired.Spec
	var conds []types.Condition
	var changed bool

	if spec.ManagementNIC != nil {
		// Once the management NIC is teamed into a SET switch its IP is
		// re-homed to a management OS vNIC. Skip here to avoid "Element not
		// found" (error 1168) on New-NetIPAddress.
		adaptorTeamed := false
		for _, sw := range spec.Networking.Switches {
			for _, m := range sw.TeamMembers {
				if m == spec.ManagementNIC.AdapterName {
					adaptorTeamed = true
					break
				}
			}
		}
		if !adaptorTeamed {
			out, err := r.hv.EnsureHostIP(ctx, *spec.ManagementNIC)
			conds = append(conds, r.condition("ManagementNIC/"+spec.ManagementNIC.AdapterName, out, err))
			if err != nil {
				return Result{Phase: types.PhaseDegraded, Conditions: conds}, true, fmt.Errorf("ensure management IP: %w", err)
			}
			if out != hyperv.OutcomeUnchanged {
				changed = true
				r.log.Info("management IP reconciled", "adapter", spec.ManagementNIC.AdapterName, "outcome", out)
			}
		}
	}

	// Identity reads are needed for both rename and domain join.
	var id hyperv.HostIdentity
	if spec.ComputerName != "" || spec.DomainJoin != nil {
		got, err := r.hv.GetHostIdentity(ctx)
		if err != nil {
			conds = append(conds, r.condition("HostIdentity", hyperv.OutcomeUnchanged, err))
			return Result{Phase: types.PhaseDegraded, Conditions: conds}, true, fmt.Errorf("get host identity: %w", err)
		}
		id = got
	}

	// Rename first; it reboots, after which a later pass proceeds to domain join.
	if spec.ComputerName != "" {
		if !strings.EqualFold(id.ComputerName, spec.ComputerName) {
			if err := r.hv.RenameComputer(ctx, spec.ComputerName); err != nil {
				conds = append(conds, r.condition("ComputerName", hyperv.OutcomeUnchanged, err))
				return Result{Phase: types.PhaseDegraded, Conditions: conds}, true, fmt.Errorf("rename computer: %w", err)
			}
			conds = append(conds, r.condition("ComputerName", hyperv.OutcomeUpdated, nil))
			r.log.Info("computer renamed; reboot required to apply", "to", spec.ComputerName, "rebootPolicy", spec.RebootPolicy)
			return r.rebootResult(ctx, spec.RebootPolicy, conds, "reboot after rename")
		}
		conds = append(conds, r.condition("ComputerName", hyperv.OutcomeUnchanged, nil))
	}

	// Domain join, using the delivered credential.
	if dj := spec.DomainJoin; dj != nil && dj.DomainName != "" {
		ct := "DomainJoin/" + dj.DomainName
		if !strings.EqualFold(id.Domain, dj.DomainName) {
			sec, ok := secrets[dj.CredentialSecret]
			if !ok {
				// No credential delivered (centre offline, or secret not authored):
				// can't join yet. Surface and wait — not an error.
				conds = append(conds, types.Condition{
					Type: ct, Status: false, Reason: "AwaitingCredential",
					Message:            "waiting for credential " + dj.CredentialSecret,
					LastTransitionTime: r.now(),
				})
				return Result{Phase: types.PhaseProgressing, Changed: changed, Conditions: conds}, true, nil
			}
			if err := r.hv.JoinDomain(ctx, dj.DomainName, dj.OUPath, sec.Data["username"], sec.Data["password"]); err != nil {
				conds = append(conds, r.condition(ct, hyperv.OutcomeUnchanged, err))
				return Result{Phase: types.PhaseDegraded, Conditions: conds}, true, fmt.Errorf("join domain: %w", err)
			}
			conds = append(conds, r.condition(ct, hyperv.OutcomeUpdated, nil))
			r.log.Info("domain joined; reboot required to apply", "domain", dj.DomainName, "rebootPolicy", spec.RebootPolicy)
			return r.rebootResult(ctx, spec.RebootPolicy, conds, "reboot after domain join")
		}
		conds = append(conds, r.condition(ct, hyperv.OutcomeUnchanged, nil))
	}

	return Result{Conditions: conds, Changed: changed}, false, nil
}

// rebootResult applies RebootPolicy after an identity change that needs a
// reboot: IfNeeded reboots now (Progressing), Never surfaces RebootRequired and
// waits. Either way the pass stops (done == true).
func (r *Reconciler) rebootResult(ctx context.Context, policy types.RebootPolicy, conds []types.Condition, what string) (Result, bool, error) {
	if policy == types.RebootIfNeeded {
		if err := r.hv.RebootHost(ctx, false); err != nil {
			return Result{Phase: types.PhaseDegraded, Changed: true, Conditions: conds}, true, fmt.Errorf("%s: %w", what, err)
		}
		return Result{Phase: types.PhaseProgressing, Changed: true, Conditions: conds}, true, nil
	}
	return Result{Phase: types.PhaseProgressing, Changed: true, RebootRequired: true, Conditions: conds}, true, nil
}

// reconcileHostRole drives the Hyper-V role towards desired. It returns
// done == true when the caller should stop and report the returned Result
// without touching networking: either the role is being installed/awaiting a
// reboot, or an error occurred. done == false means the role is installed and
// active and networking reconciliation may proceed.
func (r *Reconciler) reconcileHostRole(ctx context.Context, desired types.Host) (Result, bool, error) {
	// An installed Hyper-V role stays installed. Nothing in Ballast uninstalls it,
	// and the read that answers the question is Get-WindowsFeature, which loads
	// ServerManager and walks the component store: measured at 1m4s on a host
	// after a reboot and 2m6s on a busy one, against a five-minute cycle cap. It
	// was doing that every pass to re-learn a boolean that had not changed since
	// the host was built, and to collect RebootPending, which nothing reads.
	//
	// So it is cached once true and re-read on a slow cadence — the same rule as
	// maintenanceDeepEvery, and for the same reason. While the role is NOT
	// installed the read happens every pass: that is the converging case, where
	// the answer is live and the whole reconcile is waiting on it.
	if r.roleInstalled && r.passes%hostRoleEvery != 0 {
		return Result{HyperVInstalled: true}, false, nil
	}
	state, err := r.hv.GetHostRoleState(ctx)
	if err != nil {
		c := r.condition("HyperVRole", hyperv.OutcomeUnchanged, err)
		return Result{Phase: types.PhaseDegraded, Conditions: []types.Condition{c}}, true, err
	}
	r.roleInstalled = state.HyperVInstalled

	if state.HyperVInstalled {
		// Already active; let networking proceed.
		return Result{HyperVInstalled: true}, false, nil
	}

	// Role absent: install it (this never reboots).
	out, err := r.hv.EnsureHyperVRole(ctx)
	c := r.condition("HyperVRole", out, err)
	if err != nil {
		return Result{Phase: types.PhaseDegraded, Conditions: []types.Condition{c}}, true, fmt.Errorf("ensure Hyper-V role: %w", err)
	}
	r.log.Info("Hyper-V role installed; reboot required to activate", "rebootPolicy", desired.Spec.RebootPolicy)

	// The install only takes effect after a reboot. Whether the agent performs
	// it is governed strictly by RebootPolicy.
	if desired.Spec.RebootPolicy == types.RebootIfNeeded {
		r.log.Info("rebooting to activate Hyper-V role (RebootPolicy=IfNeeded)")
		if rerr := r.hv.RebootHost(ctx, false); rerr != nil {
			return Result{Phase: types.PhaseDegraded, Conditions: []types.Condition{c}}, true, fmt.Errorf("reboot host: %w", rerr)
		}
		// Host is restarting; not honoured yet, will resume after boot.
		return Result{Phase: types.PhaseProgressing, Changed: true, Conditions: []types.Condition{c}}, true, nil
	}

	// RebootNever: surface the requirement and wait for an operator.
	return Result{
		Phase:          types.PhaseProgressing,
		Changed:        out != hyperv.OutcomeUnchanged,
		RebootRequired: true,
		Conditions:     []types.Condition{c},
	}, true, nil
}

// condition turns an ensure outcome into a status Condition. condType is the
// stable per-resource key (e.g. "Switch/ConvergedSwitch").
func (r *Reconciler) condition(condType string, out hyperv.Outcome, err error) types.Condition {
	c := types.Condition{
		Type:               condType,
		LastTransitionTime: r.now(),
	}
	if err != nil {
		// A failure with a known cause and a known remedy is reported with a
		// machine-readable Reason, so the console can name the cause and offer
		// the action instead of showing a cmdlet error. This must come BEFORE the
		// transient check: an incompatible saved state never settles, and calling
		// it "Settling" would promise a recovery that cannot happen.
		if errors.Is(err, hyperv.ErrSavedStateIncompatible) {
			c.Status = false
			c.Reason = types.ReasonSavedStateIncompatible
			c.Message = err.Error()
			return c
		}
		// The cycle ran out of time (or was cancelled by a service stop) before
		// this operation ran. Reporting it as ApplyFailed says Ballast tried and
		// the host refused, which is false and actively misdirecting: one stall
		// early in a pass makes every later operation inherit the dead context and
		// return instantly, so a single hung call was being reported as eight
		// separate failures against healthy objects. An operator reading that goes
		// to investigate a switch that is fine.
		//
		// Checked before the transient window, because this is not a signature in
		// the error text — it is the pass itself being over, and it says nothing at
		// all about the object.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			c.Status = false
			c.Reason = types.ReasonNotAttempted
			c.Message = "not attempted — the reconcile pass ran out of time before reaching this. Something earlier in the pass is slow or hung; this object was not read or changed."
			return c
		}
		// Hyper-V did not answer, so this was not attempted either — and crucially
		// nothing was created on the strength of an unreadable observation.
		if errors.Is(err, hyperv.ErrHyperVUnavailable) {
			c.Status = false
			c.Reason = types.ReasonHyperVUnavailable
			c.Message = err.Error()
			return c
		}
		// A failure with a known in-flight signature is reported as SETTLING
		// rather than failed — the operation is expected to succeed on a later
		// pass with no operator action, and a red condition for a few seconds of
		// normal churn teaches people to ignore red.
		//
		// But only for a bounded time. Past transientWindow the original error is
		// reported as the failure it has become. Without that escalation this
		// would be the very defect the rest of this package exists to prevent: a
		// condition that reads calm while nothing works.
		if what := transientSignature(err); what != "" {
			elapsed, within := r.transients.observe(condType, r.now())
			if within {
				c.Status = true
				c.Reason = "Settling"
				c.Message = what + " — retrying (" + elapsed.Round(time.Second).String() + ")"
				return c
			}
			c.Status = false
			c.Reason = "ApplyFailed"
			c.Message = err.Error() + " (still failing after " + elapsed.Round(time.Second).String() + "; no longer treated as transient)"
			return c
		}
		r.transients.clear(condType)
		c.Status = false
		c.Reason = "ApplyFailed"
		c.Message = err.Error()
		return c
	}
	// Succeeded: forget any transient history, or a later unrelated one would
	// inherit this start time and escalate early.
	r.transients.clear(condType)
	c.Status = true
	switch out {
	case hyperv.OutcomeCreated:
		c.Reason = "Created"
		c.Message = "created to match desired state"
	case hyperv.OutcomeUpdated:
		c.Reason = "Updated"
		c.Message = "adjusted to match desired state"
	default:
		c.Reason = "AlreadyConfigured"
		c.Message = "already matches desired state"
	}
	return c
}

// advisoryCondition is condition() for a best-effort step (host DNS, default
// VM/VHD paths): a failure is recorded as a non-blocking advisory — Reason
// "NotApplied" rather than "ApplyFailed" — so it neither degrades the host nor
// holds back its generation. The host stays Ready/settled and the UI surfaces
// it as "settled, with advisories" rather than a hard error. Success is
// identical to condition().
func (r *Reconciler) advisoryCondition(condType string, out hyperv.Outcome, err error) types.Condition {
	c := r.condition(condType, out, err)
	if err != nil {
		c.Reason = "NotApplied"
		c.Message = err.Error() + " (best-effort; not blocking)"
	}
	return c
}

// PhaseRecorder starts timing a named stage and returns the function that stops
// it. Same shape as the runner's own phase timer, so the runner can hand its
// marker straight in.
type PhaseRecorder func(name string) func()

// SetPhaseRecorder wires the cycle's phase timer into the reconciler, so stages
// inside a reconcile are timed by the same instrument as the stages around it —
// one clock, one list, no second thing to keep honest.
func (r *Reconciler) SetPhaseRecorder(p PhaseRecorder) { r.phase = p }

// mark times a named stage. A reconciler with no recorder — every unit test, and
// any caller that does not care — gets a no-op, so instrumentation can be added
// to a step without every construction site having to know about it.
//
// Names are prefixed by their parent ("cluster/csv"), so the list reads as a
// drill-down rather than as a flat set of unrelated numbers.
func (r *Reconciler) mark(name string) func() {
	if r.phase == nil {
		return func() {}
	}
	return r.phase(name)
}
