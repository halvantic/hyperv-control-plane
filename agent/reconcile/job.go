package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

// ExecuteJob runs one imperative Job locally and returns a short human-readable
// result detail (shown in the centre on success) plus any error. Unlike the
// reconcile loop, a Job is a one-shot action — it is not retried by the loop;
// the centre records the outcome and the operator re-issues on failure.
//
// onProgress (nil-safe) receives mid-flight progress notes for the long-running
// jobs that report them (live migration), which the caller surfaces as the job's
// Running message.
func (r *Reconciler) ExecuteJob(ctx context.Context, job types.Job, onProgress hyperv.ProgressFunc) (string, error) {
	p := job.Params
	switch job.Kind {
	case types.JobVMStart:
		_, err := r.hv.SetVMPowerState(ctx, p["vm"], types.VMPowerRunning)
		return done(err, "started "+p["vm"])
	case types.JobVMStop:
		_, err := r.hv.SetVMPowerState(ctx, p["vm"], types.VMPowerOff)
		return done(err, "stopped "+p["vm"])
	case types.JobVMRestart:
		return done(r.hv.RestartVM(ctx, p["vm"]), "restarted "+p["vm"])
	case types.JobVMCheckpoint:
		return done(r.hv.CreateVMCheckpoint(ctx, p["vm"], p["name"]), "checkpoint created")
	case types.JobVMExport:
		return done(r.hv.ExportVM(ctx, p["vm"], p["path"]), "exported to "+p["path"])
	case types.JobVMClone:
		return done(r.hv.CloneVM(ctx, p["vm"], p["name"], p["folder"]), "cloned "+p["vm"]+" to "+p["name"])
	case types.JobVMMoveStorage:
		return r.hv.MoveVMStorage(ctx, p["vm"], p["dest"], onProgress)
	case types.JobVMRename:
		return done(r.hv.RenameVM(ctx, p["vm"], p["newName"], p["cluster"]),
			"renamed "+p["vm"]+" to "+p["newName"])
	case types.JobVMDiscardSavedState:
		return done(r.hv.DiscardVMSavedState(ctx, p["vm"]), "discarded the saved state of "+p["vm"]+" (it is now Off)")
	case types.JobVMDiscardSavedStateAndStart:
		// Two steps, one intention. Reported as one job so a failure to start is
		// not mistaken for a failure to discard — by then the memory image is
		// already gone and repeating the discard would be a no-op, while the
		// operator still needs to know the VM did not come up.
		if err := r.hv.DiscardVMSavedState(ctx, p["vm"]); err != nil {
			return "", fmt.Errorf("discard saved state of %s: %w", p["vm"], err)
		}
		if _, err := r.hv.SetVMPowerState(ctx, p["vm"], types.VMPowerRunning); err != nil {
			return "", fmt.Errorf("saved state of %s was discarded, but it did not start: %w", p["vm"], err)
		}
		return "discarded the saved state of " + p["vm"] + " and started it", nil
	case types.JobVMCaptureTemplate:
		n, err := r.hv.CaptureTemplate(ctx, p["vm"], p["dest"], p["generalise"] == "true", p["discardSaved"] == "true", p["guestUser"], p["guestPass"], onProgress)
		// The size travels back inside the message; types owns that format, and
		// the centre records it on the template. See types.FormatCaptureResult.
		return done(err, types.FormatCaptureResult(p["template"], p["dest"], n))
	case types.JobVMDeployFromTemplate:
		return done(r.hv.DeployFromTemplate(ctx, p["source"], p["dest"], p["unattend"], onProgress), "deployed "+p["vm"]+"'s disk from template "+p["template"])
	case types.JobFetchISO:
		note, err := r.hv.FetchISO(ctx, p["url"], p["dest"])
		msg := "downloaded ISO " + p["name"]
		if note != "" {
			msg += " (" + note + ")"
		}
		return done(err, msg)
	case types.JobGuestActivateAVMA:
		out, err := r.hv.EnsureGuestAVMA(ctx, p["vm"], p["avmaKey"], p["guestUser"], p["guestPass"])
		if err != nil {
			return "", err
		}
		if out == hyperv.OutcomeUnchanged {
			return p["vm"] + " is already activated with that key", nil
		}
		return "activated " + p["vm"] + " against this host", nil
	case types.JobGuestJoinDomain:
		return done(r.hv.GuestJoinDomain(ctx, p["vm"], p["domain"], p["ou"], p["newName"], p["guestUser"], p["guestPass"], p["domainUser"], p["domainPass"]),
			"joined "+p["vm"]+" to "+p["domain"]+" (guest rebooting)")
	case types.JobGuestSetIP:
		return done(r.hv.GuestSetIP(ctx, p["vm"], p["interface"], p["address"], p["gateway"], p["dns"], p["guestUser"], p["guestPass"]),
			"set guest IP "+p["address"]+" on "+p["vm"])
	case types.JobVMApplyCheck:
		return done(r.hv.ApplyVMCheckpoint(ctx, p["vm"], p["name"]), "applied checkpoint "+p["name"])
	case types.JobVMRemoveCheck:
		return done(r.hv.RemoveVMCheckpoint(ctx, p["vm"], p["name"]), "removed checkpoint "+p["name"])
	case types.JobClusterAddNode:
		return done(r.hv.AddClusterNode(ctx, p["node"]), "added "+p["node"])
	case types.JobClusterEvict:
		return done(r.hv.EvictClusterNode(ctx, p["node"]), "evicted "+p["node"])
	case types.JobNodeDrain:
		return done(r.hv.DrainNode(ctx, p["node"]), "drained "+p["node"])
	case types.JobNodeResume:
		return done(r.hv.ResumeNode(ctx, p["node"]), "resumed "+p["node"])
	case types.JobDisableLSO:
		/* LSO off, so jumbo frames can actually cross.

		   LSO and not RSC: on the rig, RSC is enabled on every uplink of the
		   hosts where jumbo works, so it is not the blocker and changing it
		   would be churn on hosts that are already right.

		   A job rather than desired state: LSO earns its keep on a 1500 fabric
		   and turning it off costs throughput, so it is a decision an operator
		   makes for a reason — not something a reconcile does on their behalf
		   because an MTU was declared somewhere. */
		splitCSV := func(v string) []string {
			var out []string
			for _, a := range strings.Split(v, ",") {
				if a = strings.TrimSpace(a); a != "" {
					out = append(out, a)
				}
			}
			return out
		}
		// The switches as well as the uplinks: their management vNICs carry the
		// setting separately, and the operator's own working command had no
		// -Name and so hit both.
		return r.hv.DisableLSO(ctx, splitCSV(p["adapters"]), splitCSV(p["switches"]))
	case types.JobTestJumboPath:
		/* The only thing in Ballast that establishes jumbo frames actually work.

		   Deliberately a job and never part of a pass. It changes nothing and
		   asserts nothing about desired state, and running a ping storm from
		   every host on every 40-second cycle would be load, not diagnosis.

		   The tester is behind its own interface, so a backend that cannot do it
		   says so rather than silently reporting a clean test. */
		tester, ok := r.hv.(hyperv.JumboPathTester)
		if !ok {
			return "", fmt.Errorf("this agent's Hyper-V backend cannot run a jumbo path test")
		}
		mtu := hyperv.ParseMTUParam(p["mtu"])
		var targets []string
		for _, t := range strings.Split(p["targets"], ",") {
			if t = strings.TrimSpace(t); t != "" {
				targets = append(targets, t)
			}
		}
		probes, err := tester.TestJumboPath(ctx, r.lastStorageVNICs, targets, mtu)
		if err != nil {
			return "", err
		}
		msg := hyperv.DescribeJumboProbes(probes, mtu)
		// A genuine MTU fault fails the job; an unreachable peer does not. Both
		// are in the message either way — what changes is whether the console
		// shows this as something to act on.
		if hyperv.JumboProbesFailed(probes) {
			return "", fmt.Errorf("%s", msg)
		}
		return msg, nil
	case types.JobClearISCSIFavourites:
		var targets []string
		for _, t := range strings.Split(p["targets"], ",") {
			if t = strings.TrimSpace(t); t != "" {
				targets = append(targets, t)
			}
		}
		return r.hv.ClearISCSIFavourites(ctx, targets)
	case types.JobClusterVolumeOnline:
		out, note, err := r.hv.StartClusterVolume(ctx, p["volume"])
		if err != nil {
			return "", err
		}
		if out == hyperv.OutcomeUnchanged {
			return note, nil
		}
		return note, nil
	case types.JobClusterStartCoreGroup:
		out, note, err := r.hv.StartClusterCoreGroup(ctx)
		if err != nil {
			return "", err
		}
		if out == hyperv.OutcomeUnchanged {
			return note, nil
		}
		return "brought " + note + " online", nil
	case types.JobClusterClearQuarantine:
		out, note, err := r.hv.ClearNodeQuarantine(ctx, p["node"])
		if err != nil {
			return "", err
		}
		if out == hyperv.OutcomeUnchanged {
			return note, nil
		}
		return note, nil
	case types.JobClusterMoveGroup:
		return done(r.hv.MoveClusterGroup(ctx, p["group"], p["node"]), "moved "+p["group"]+" to "+p["node"])
	case types.JobClusterMoveCSV:
		return done(r.hv.MoveClusterSharedVolume(ctx, p["volume"], p["node"]), "moved "+p["volume"]+" to "+p["node"])
	case types.JobDestroyS2D:
		return r.hv.DestroyS2D(ctx, p["wipeDisks"] == "true")
	case types.JobResetISCSIInitiator:
		return r.hv.ResetISCSIInitiator(ctx)
	case types.JobDisconnectISCSITarget:
		return r.hv.DisconnectISCSITarget(ctx, p["target"])
	case types.JobRepairISCSIPortals:
		return r.hv.RepairISCSIPortals(ctx)
	case types.JobISCSIRediscover:
		return r.hv.RediscoverISCSI(ctx)
	case types.JobAdoptISCSIDisk:
		// The volume's source LUN comes from the CLUSTER spec, not the job: the job
		// says which volume and what to do about its contents, and the serial that
		// identifies the disk stays the single declared one. A job carrying its own
		// serial would be a second authority over which disk this is.
		vol, aerr := r.volumeSource(p["volume"])
		if aerr != nil {
			return "", aerr
		}
		mode := strings.ToLower(strings.TrimSpace(p["mode"]))
		if mode != "keep" && mode != "wipe" {
			return "", fmt.Errorf("adopt %q: mode must be keep or wipe, got %q", p["volume"], p["mode"])
		}
		return r.hv.AdoptISCSIDiskWithContents(ctx, hyperv.ISCSIAdoption{
			Name:   p["volume"],
			Source: vol,
			Keep:   mode == "keep",
			Wipe:   mode == "wipe",
		})
	case types.JobPruneISCSIPortals:
		// The declared list rides in the job rather than being read from the
		// cached spec: the operator is acting on what the console showed them,
		// and a spec that changed in between would silently prune something else.
		split := func(v string) []string {
			var out []string
			for _, x := range strings.Split(v, ",") {
				if t := strings.TrimSpace(x); t != "" {
					out = append(out, t)
				}
			}
			return out
		}
		declared := split(p["portals"])
		// The declared TARGETS matter as much as the portals now: the prune also
		// removes stale favourite targets, and without this list it would find
		// nothing declared and remove them all — every node losing its storage at
		// the next reboot. Refused rather than defaulted.
		targets := split(p["targets"])
		if len(targets) == 0 {
			return "", fmt.Errorf("prune iscsi: no declared targets were sent with this job, so every favourite target on the host would look undeclared. " +
				"Removing them all is what Reset initiator is for; this job only removes what the spec does not name")
		}
		return r.hv.PruneISCSIPortals(ctx, declared, targets)
	case types.JobScanImportableVMs:
		return r.scanImportableVMs(ctx, p["path"])
	case types.JobImportVM:
		path := strings.TrimSpace(p["path"])
		if path == "" {
			return "", fmt.Errorf("import vm: no configuration path was given. The path comes from the scan, which is the only thing that knows where the configuration actually is")
		}
		mode := strings.ToLower(strings.TrimSpace(p["mode"]))
		// No default. Register-in-place and copy fail in opposite, expensive
		// directions — one gives two clusters a claim on one set of files, the
		// other silently rewrites a 500GB VM onto the volume it is already on —
		// so an unset mode is a refusal, not a coin toss.
		if mode != "register" && mode != "copy" {
			return "", fmt.Errorf("import %s: mode must be register or copy, got %q", path, p["mode"])
		}
		out, err := r.hv.ImportVM(ctx, hyperv.VMImport{
			ConfigPath:        path,
			Copy:              mode == "copy",
			Cluster:           p["cluster"] == "true",
			ApplyFixes:        p["applyFixes"] == "true",
			DiscardSavedState: p["discardSavedState"] == "true",
		})
		if err != nil {
			return "", err
		}
		// The imported VM is no longer importable, and the console must stop
		// offering it immediately rather than at the next scan. Dropping it here
		// is cheap and truthful; leaving it would invite a second import, which
		// is the refusal this feature guards hardest.
		r.forgetImportable(path)
		return out, nil
	case types.JobMigrationEnableCBT:
		return r.migrationEnableCBT(ctx, p)
	case types.JobMigrationPass:
		return r.migrationPass(ctx, job, onProgress)
	case types.JobMigrationCleanup:
		return r.migrationCleanup(ctx, p)
	case types.JobRepairPool:
		return r.hv.RepairStoragePool(ctx)
	case types.JobClusterUpdateFunctionalLevel:
		return r.hv.UpdateClusterFunctionalLevel(ctx)
	case types.JobRebuildPool:
		return r.hv.RebuildStoragePool(ctx)
	case types.JobClusterMoveVM:
		return done(r.hv.MoveClusterVM(ctx, p["vm"], p["node"], onProgress), "live-migrated "+p["vm"]+" to "+p["node"])
	case types.JobMigrateVM, types.JobCopyVM:
		// Auto-provision Kerberos constrained delegation between this (source) host
		// and the destination so shared-nothing migration works without a manual AD
		// step. EnsureMigrationDelegation includes the local host, so passing just
		// the destination sets delegation both ways. The agent runs as a domain
		// admin; idempotent, and a no-op when it is already in place.
		if _, derr := r.hv.EnsureMigrationDelegation(ctx, []string{p["destHost"]}); derr != nil {
			return "", fmt.Errorf("ensure migration delegation to %s: %w", p["destHost"], derr)
		}
		/* The network map rides on the job as JSON. A source switch with no
		   entry arrives disconnected, which is what an empty map means and is a
		   deliberate answer rather than a missing one. */
		var netMap []types.EvacuationNIC
		if raw := p["networkMap"]; strings.TrimSpace(raw) != "" {
			if err := json.Unmarshal([]byte(raw), &netMap); err != nil {
				return "", fmt.Errorf("the network mapping on this job could not be read, so the VM would arrive on "+
					"whatever the destination happened to offer: %w", err)
			}
		}
		if job.Kind == types.JobCopyVM {
			return r.hv.CopyVM(ctx, p["vm"], p["destHost"], p["destPath"], p["sourceCluster"], p["targetCluster"], netMap, onProgress)
		}
		return r.hv.MigrateVM(ctx, p["vm"], p["destHost"], p["destPath"], p["sourceCluster"], p["targetCluster"], netMap, onProgress)
	case types.JobClearVMExport:
		return r.hv.ClearVMExport(ctx, p["vm"], p["destHost"], p["destPath"])
	case types.JobRemoveSwitch:
		return done(r.hv.RemoveSwitch(ctx, p["switch"]), "removed switch "+p["switch"])
	case types.JobRemoveMgmtVNIC:
		return done(r.hv.RemoveMgmtVNIC(ctx, p["vnic"]), "removed management vNIC "+p["vnic"])
	case types.JobRemoveVM:
		return done(r.hv.RemoveVM(ctx, p["vm"]), "removed VM "+p["vm"])
	case types.JobResync:
		// Deliberately does nothing. The value is in the nudge the runner fires
		// when any job completes: an immediate cycle, a full desired-state pull
		// and a fresh scan of everything the cadences otherwise throttle.
		return "reconcile requested", nil
	case types.JobRemoveReplicaBroker:
		return r.hv.RemoveReplicaBroker(ctx, p["group"])
	case types.JobRemoveCSV:
		// The agent's own words, not a sentence composed here from the fact that
		// no error came back. What it removed -- and what it could not -- depends
		// on what backed the volume, and only the agent knows that.
		return r.hv.RemoveCSV(ctx, p["volume"])
	case types.JobFormatDisk:
		return done(r.hv.FormatDisk(ctx, p["deviceId"]), "formatted disk "+p["deviceId"])
	case types.JobFormatDiskDrive:
		// No drive letter is a deliberate choice, not a missing parameter, so the
		// result says which was asked for rather than reporting "as :".
		if p["driveLetter"] == "" {
			return done(r.hv.FormatDiskDrive(ctx, p["deviceId"], ""), "formatted disk "+p["deviceId"]+" with no drive letter")
		}
		return done(r.hv.FormatDiskDrive(ctx, p["deviceId"], p["driveLetter"]), "formatted disk "+p["deviceId"]+" as "+p["driveLetter"]+":")
	case types.JobCreateVolumeInFreeSpace:
		/* A bad size must not become "the whole free extent".

		   0 means "take the rest" and is a deliberate choice an operator makes.
		   A size that will not parse is a different thing entirely — a typo, or a
		   console sending something unexpected — and quietly turning it into the
		   maximum would hand somebody the whole disk when they asked for 200GB.
		   Absent is not zero here either. */
		var size uint64
		if raw := strings.TrimSpace(p["sizeBytes"]); raw != "" {
			n, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				return done(fmt.Errorf("sizeBytes %q is not a number of bytes; leave it out to use the whole free extent", raw), "")
			}
			size = n
		}
		return done(r.hv.CreateVolumeInFreeSpace(ctx, p["deviceId"], p["driveLetter"], p["label"], size),
			"created a volume on disk "+p["deviceId"]+" as "+p["driveLetter"]+": from unallocated space")
	case types.JobRepairHostDNS:
		return r.hv.RepairHostDNS(ctx, p["dns"])
	case types.JobResetPoolDisks:
		return r.hv.ResetPoolDisks(ctx)
	case types.JobReleasePoolDisks:
		return r.hv.ReleasePoolDisks(ctx, p["deviceId"])
	case types.JobRepairNetworkProfile:
		return r.hv.RepairNetworkProfile(ctx)
	case types.JobRebootHost:
		return done(r.hv.RebootHost(ctx, p["drain"] == "true"), "reboot initiated")
	case types.JobShutdownHost:
		return done(r.hv.ShutdownHost(ctx, p["drain"] == "true"), "shutdown initiated")
	case types.JobEnableRDP:
		return done(r.hv.EnableRDP(ctx), "enabled Remote Desktop")
	case types.JobClusterDestroy:
		return done(r.hv.DestroyCluster(ctx), "destroyed cluster")
	case types.JobClusterValidate:
		return r.hv.ValidateCluster(ctx, splitList(p["nodes"]), splitList(p["include"]))
	case types.JobClusterLog:
		return r.hv.ClusterLog(ctx, p["span"], p["filter"])
	case types.JobMigrationDelegation:
		_, err := r.hv.EnsureMigrationDelegation(ctx, splitList(p["nodes"]))
		return done(err, "configured Kerberos live-migration delegation")
	case types.JobVMTestFailover:
		return r.hv.TestFailover(ctx, p["vm"], p["network"])
	case types.JobVMStopTestFailover:
		return done(r.hv.StopTestFailover(ctx, p["vm"]), "stopped test failover of "+p["vm"])
	case types.JobVMPlannedFailover:
		return r.hv.PlannedFailover(ctx, p["vm"], p["primaryHost"], p["network"])
	case types.JobVMFailover:
		return r.hv.Failover(ctx, p["vm"], p["recoveryPoint"], p["network"])
	case types.JobVMCancelFailover:
		return done(r.hv.CancelFailover(ctx, p["vm"]), "cancelled failover of "+p["vm"])
	case types.JobVMReverseReplication:
		return r.hv.ReverseReplication(ctx, p["vm"])
	case types.JobVMRemoveReplica:
		return done(r.hv.RemoveReplicaVM(ctx, p["vm"]), "removed replica copy of "+p["vm"])
	case types.JobEnsureTestSwitch:
		return r.hv.EnsureTestSwitch(ctx, p["switch"], p["type"])
	case types.JobRemoveTestSwitch:
		return done(r.hv.RemoveTestSwitch(ctx, p["switch"]), "removed the isolated switch "+p["switch"])
	case types.JobVMHealthProbe:
		return r.hv.HealthProbe(ctx, p["vm"], p["check"], p["address"], atoiParam(p["port"]), atoiParam(p["expectExit"]), p["script"])
	default:
		return "", fmt.Errorf("unknown job kind %q", job.Kind)
	}
}

// atoiParam reads a numeric job param, treating an absent or unparseable value
// as zero. Every caller here has a meaningful zero — no port, exit code 0 — and
// the agent-side validation refuses the ones where zero is not an answer, so a
// malformed param produces a named refusal rather than a silent default.
func atoiParam(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// splitList parses a comma-separated job param into a trimmed, non-empty slice.
// An empty param yields nil so callers can apply their own default.
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// done pairs an op error with its success message.
func done(err error, msg string) (string, error) {
	if err != nil {
		return "", err
	}
	return msg, nil
}
