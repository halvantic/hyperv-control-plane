// Command agent is the Ballast host agent. It runs as a Windows service on each
// Hyper-V host: it registers with the centre, reports inventory, pulls and
// caches its desired Host state, and journals status — keeping the last-honoured
// state when the centre is unreachable. The reconcile loop that drives actual
// host state towards desired is the next step and is not yet wired in.
package main

import (
	"context"
	"flag"
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
		}
		if err := installService(serviceName, serviceDisplayName, serviceDescription, exe, args); err != nil {
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
