//go:build windows

package main

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// runAgent runs the agent either under the Windows Service Control Manager
// (when started by the SCM) or directly in the console (for --debug or when
// launched interactively).
func runAgent(ctx context.Context, r *runner, debug bool) error {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("determine service context: %w", err)
	}
	if isSvc && !debug {
		return svc.Run(serviceName, &winService{r: r})
	}
	r.log.Info("running in console mode")
	return r.run(ctx)
}

// winService adapts the runner to the SCM lifecycle.
type winService struct {
	r *runner
}

// Execute is called by the SCM. It starts the runner and translates SCM
// control requests (Stop/Shutdown) into context cancellation.
func (w *winService) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.r.run(ctx) }()

	status <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				w.r.log.Info("service stop requested")
				status <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				status <- svc.Status{State: svc.Stopped}
				return false, 0
			default:
				w.r.log.Warn("unexpected service control request", "cmd", c.Cmd)
			}
		case err := <-done:
			// Runner exited on its own (e.g. fatal error); report stopped.
			if err != nil {
				w.r.log.Error("runner exited with error", "err", err)
				status <- svc.Status{State: svc.Stopped}
				return false, 1
			}
			status <- svc.Status{State: svc.Stopped}
			return false, 0
		}
	}
}

// installService registers the agent with the SCM. args are appended to the
// service binary path so the installed service starts with the same flags.
func installService(name, displayName, desc, exePath string, args []string, user, password string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	if s, err := m.OpenService(name); err == nil {
		s.Close()
		return fmt.Errorf("service %q already exists", name)
	}

	cfg := mgr.Config{
		DisplayName:  displayName,
		Description:  desc,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
	}
	// Run as a domain account when given, so cluster/domain operations have the
	// rights they need; otherwise LocalSystem.
	if user != "" {
		cfg.ServiceStartName = user
		cfg.Password = password
	}
	s, err := m.CreateService(name, exePath, cfg, args...)
	if err != nil {
		return err
	}
	defer s.Close()

	// Auto-restart on crash so a flaky reconcile or transient host fault does not
	// leave the host unmanaged — the centre depends on the agent always running.
	// The reset period counts a clean run; repeated crashes keep restarting.
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}, uint32((24 * time.Hour).Seconds())); err != nil {
		return fmt.Errorf("set recovery actions: %w", err)
	}
	return nil
}

// removeService deregisters the agent from the SCM.
func removeService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("service %q not installed: %w", name, err)
	}
	defer s.Close()
	return s.Delete()
}
