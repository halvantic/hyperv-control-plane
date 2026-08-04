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
	"fmt"
	"strings"
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
}

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
func (r *Reconciler) Reconcile(ctx context.Context, desired types.Host, secrets map[string]types.Secret) (Result, error) {
	var (
		conds           []types.Condition
		changed         bool
		failures        int
		firstErr        error
		hyperVInstalled bool
		inMaintenance   bool
		draining        bool
	)

	// Identity comes first: the host should have its final name, management IP
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
		out, err := r.hv.EnsureReplicaServer(ctx, *rs)
		conds = append(conds, r.condition("ReplicaServer", out, err))
		if err != nil {
			r.log.Warn("configure replica server failed (retries next pass)", "err", err)
		} else if out != hyperv.OutcomeUnchanged {
			changed = true
			r.log.Info("replica server reconciled", "outcome", out)
		}
	}

	// Host DNS servers (typically the DCs), fanned from the centre's Domain & DNS
	// setting. Best-effort and applied before a join so the host can resolve the
	// domain's SRV records; a failure surfaces as a condition without degrading.
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
		if v.IPConfig != nil && len(v.IPConfig.DNSServers) == 0 && len(net.DNSServers) > 0 {
			cfg := *v.IPConfig
			cfg.DNSServers = append([]string(nil), net.DNSServers...)
			v.IPConfig = &cfg
		}
		vnics = append(vnics, v)
	}
	if len(vnics) > 0 {
		outs, errs := r.hv.EnsureMgmtVNICs(ctx, vnics)
		for i, v := range vnics {
			out, err := outs[i], errs[i]
			conds = append(conds, r.condition("ManagementVNIC/"+v.Name, out, err))
			if err != nil {
				failures++
				if firstErr == nil {
					firstErr = fmt.Errorf("ensure vNIC %q: %w", v.Name, err)
				}
				r.log.Error("ensure management vNIC failed", "vnic", v.Name, "err", err)
				continue
			}
			if out != hyperv.OutcomeUnchanged {
				changed = true
				r.log.Info("management vNIC reconciled", "vnic", v.Name, "outcome", out)
			}
		}
	}

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
		out, err := r.hv.PruneManagementVNICs(ctx, switches, keep)
		conds = append(conds, r.advisoryCondition("PruneVNICs", out, err))
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
		out, ms, err := r.hv.EnsureNodeMaintenance(ctx, desired.Meta.Name, intent)
		// Only report when something actually happened or is wrong. Exit runs every
		// cycle on every host and is a no-op almost always; a condition each time
		// would be pure noise.
		if out != hyperv.OutcomeUnchanged || err != nil || ms.StorageError != "" {
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
		}
	}
	res := Result{
		InMaintenance:   inMaintenance,
		Draining:        draining,
		Conditions:      conds,
		Changed:         changed,
		Honoured:        failures == 0,
		HyperVInstalled: hyperVInstalled,
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
	state, err := r.hv.GetHostRoleState(ctx)
	if err != nil {
		c := r.condition("HyperVRole", hyperv.OutcomeUnchanged, err)
		return Result{Phase: types.PhaseDegraded, Conditions: []types.Condition{c}}, true, err
	}

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
