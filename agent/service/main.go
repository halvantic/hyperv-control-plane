// Command agent is the Ballast host agent. It runs as a Windows service on each
// Hyper-V host: it registers with the centre, reports inventory, pulls and
// caches its desired Host state, and journals status — keeping the last-honoured
// state when the centre is unreachable. The reconcile loop that drives actual
// host state towards desired is the next step and is not yet wired in.
package main

import (
	"context"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"log/slog"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/agent/reconcile"
	"github.com/joshua-fourie/ballast/agent/store"
)

const (
	serviceName        = "BallastAgent"
	serviceDisplayName = "Ballast Host Agent"
	serviceDescription = "Reconciles this Hyper-V host towards its Ballast desired state and keeps honouring it when the control plane is offline."
)

func main() {
	var (
		centreAddr = flag.String("centre", "127.0.0.1:9443", "centre gRPC address")
		hostName   = flag.String("host", defaultHostName(), "this host's name (must match its desired-state object)")
		storePath  = flag.String("store", defaultStorePath(), "path to the agent's embedded store")
		heartbeat  = flag.Duration("heartbeat", 15*time.Second, "interval between pull/report cycles")
		hypervKind = flag.String("hyperv", "stub", "host backend: stub|powershell (powershell requires a real Hyper-V host)")
		debug      = flag.Bool("debug", false, "run in the console even when started by the SCM")
		install    = flag.Bool("install", false, "install the Windows service and exit")
		uninstall  = flag.Bool("uninstall", false, "remove the Windows service and exit")
		svcUser    = flag.String("service-user", "", "run the service as this account (e.g. DOMAIN\\user); empty = LocalSystem. Cluster/domain operations need a domain admin.")
		svcPass    = flag.String("service-password", "", "password for -service-user")
		logPath    = flag.String("log", defaultLogPath(), "agent log file (stdout is discarded when run as a service)")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	switch {
	case *install:
		exe, err := os.Executable()
		if err != nil {
			log.Error("resolve executable path", "err", err)
			os.Exit(1)
		}
		args := []string{
			"-centre", *centreAddr,
			"-host", *hostName,
			"-store", *storePath,
			"-heartbeat", heartbeat.String(),
			"-hyperv", *hypervKind,
			"-log", *logPath,
		}
		if err := installService(serviceName, serviceDisplayName, serviceDescription, exe, args, *svcUser, *svcPass); err != nil {
			log.Error("install service failed", "err", err)
			os.Exit(1)
		}
		log.Info("service installed", "name", serviceName)
		return
	case *uninstall:
		if err := removeService(serviceName); err != nil {
			log.Error("remove service failed", "err", err)
			os.Exit(1)
		}
		log.Info("service removed", "name", serviceName)
		return
	}

	// Switch to the file logger for the running agent: as a Windows service its
	// stdout is discarded, so without this there is no diagnosis. In console /
	// debug runs it still mirrors to stdout.
	log = slog.New(slog.NewTextHandler(openLog(*logPath, *debug), &slog.HandlerOptions{Level: slog.LevelInfo}))

	st, err := store.Open(*storePath)
	if err != nil {
		log.Error("open store failed", "path", *storePath, "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// The rest of the agent depends only on hyperv.Interface, so the backend is
	// a runtime choice. Default is the stub; powershell selects the real host
	// implementation and is intended for a Hyper-V host.
	var hv hyperv.Interface
	switch *hypervKind {
	case "stub":
		hv = &hyperv.Stub{}
	case "powershell":
		hv = hyperv.NewPowerShell(log)
	default:
		log.Error("unknown -hyperv backend", "value", *hypervKind, "valid", "stub|powershell")
		os.Exit(1)
	}

	r := &runner{
		cfg: runnerConfig{
			centreAddr: *centreAddr,
			hostName:   *hostName,
			storePath:  *storePath,
			heartbeat:  *heartbeat,
		},
		log:        log,
		hv:         hv,
		st:         st,
		reconciler: reconcile.New(hv, log),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := runAgent(ctx, r, *debug); err != nil {
		log.Error("agent exited with error", "err", err)
		os.Exit(1)
	}
	log.Info("agent stopped")
}

func defaultHostName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "host01"
}

func defaultStorePath() string {
	// ProgramData on Windows; the working directory elsewhere.
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, "Ballast", "agent.db")
	}
	return "ballast-agent.db"
}

func defaultLogPath() string {
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, "Ballast", "agent.log")
	}
	return "ballast-agent.log"
}

// openLog returns the writer for the running agent's logs: a log file (created,
// appended), with a simple size-based rotation, mirrored to stdout in console/
// debug runs. On any failure it falls back to stdout so logging never blocks
// the agent from starting.
func openLog(path string, debug bool) io.Writer {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return os.Stdout
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > 10<<20 {
		_ = os.Rename(path, path+".1") // keep one previous file
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return os.Stdout
	}
	if debug {
		return io.MultiWriter(os.Stdout, f)
	}
	return f
}
