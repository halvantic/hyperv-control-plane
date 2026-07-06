package reconcile

import (
	"context"
	"fmt"
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
	case types.JobFetchISO:
		return done(r.hv.FetchISO(ctx, p["url"], p["dest"]), "downloaded ISO "+p["name"])
	case types.JobGuestJoinDomain:
		return done(r.hv.GuestJoinDomain(ctx, p["vm"], p["domain"], p["ou"], p["guestUser"], p["guestPass"], p["domainUser"], p["domainPass"]),
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
	case types.JobClusterMoveGroup:
		return done(r.hv.MoveClusterGroup(ctx, p["group"], p["node"]), "moved "+p["group"]+" to "+p["node"])
	case types.JobClusterMoveCSV:
		return done(r.hv.MoveClusterSharedVolume(ctx, p["volume"], p["node"]), "moved "+p["volume"]+" to "+p["node"])
	case types.JobClusterMoveVM:
		return done(r.hv.MoveClusterVM(ctx, p["vm"], p["node"], onProgress), "live-migrated "+p["vm"]+" to "+p["node"])
	case types.JobMigrateVM:
		// Auto-provision Kerberos constrained delegation between this (source) host
		// and the destination so shared-nothing migration works without a manual AD
		// step. EnsureMigrationDelegation includes the local host, so passing just
		// the destination sets delegation both ways. The agent runs as a domain
		// admin; idempotent, and a no-op when it is already in place.
		if _, derr := r.hv.EnsureMigrationDelegation(ctx, []string{p["destHost"]}); derr != nil {
			return "", fmt.Errorf("ensure migration delegation to %s: %w", p["destHost"], derr)
		}
		return r.hv.MigrateVM(ctx, p["vm"], p["destHost"], p["destPath"], onProgress)
	case types.JobRemoveSwitch:
		return done(r.hv.RemoveSwitch(ctx, p["switch"]), "removed switch "+p["switch"])
	case types.JobRemoveVM:
		return done(r.hv.RemoveVM(ctx, p["vm"]), "removed VM "+p["vm"])
	case types.JobRemoveCSV:
		return done(r.hv.RemoveCSV(ctx, p["volume"]), "removed volume "+p["volume"])
	case types.JobFormatDisk:
		return done(r.hv.FormatDisk(ctx, p["deviceId"]), "formatted disk "+p["deviceId"])
	case types.JobFormatDiskDrive:
		return done(r.hv.FormatDiskDrive(ctx, p["deviceId"], p["driveLetter"]), "formatted disk "+p["deviceId"]+" as "+p["driveLetter"]+":")
	case types.JobRepairHostDNS:
		return r.hv.RepairHostDNS(ctx, p["dns"])
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
	default:
		return "", fmt.Errorf("unknown job kind %q", job.Kind)
	}
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
