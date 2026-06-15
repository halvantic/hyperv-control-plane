//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// grantLogonRightScript grants the SeServiceLogonRight to an account via the LSA
// API. Run with `powershell.exe -File` (a temp file) so its embedded C# double
// quotes survive — the agent's usual -Command path would mangle them.
const grantLogonRightScript = `
param([Parameter(Mandatory=$true)][string]$Account)
$ErrorActionPreference = 'Stop'
# ".\user" (local-machine notation) is valid for the service logon account but
# NTAccount cannot translate the literal ".\"; rewrite it to "<COMPUTERNAME>\user"
# so the SID lookup resolves the local account.
if ($Account -like '.\*') { $Account = "$env:COMPUTERNAME\" + $Account.Substring(2) }
$sid = (New-Object System.Security.Principal.NTAccount($Account)).Translate([System.Security.Principal.SecurityIdentifier])
$sidBytes = New-Object byte[] $sid.BinaryLength
$sid.GetBinaryForm($sidBytes, 0)
$src = @'
using System;
using System.Runtime.InteropServices;
public class BallastLsa {
  [StructLayout(LayoutKind.Sequential)] struct LSA_UNICODE_STRING { public ushort Length; public ushort MaximumLength; public IntPtr Buffer; }
  [StructLayout(LayoutKind.Sequential)] struct LSA_OBJECT_ATTRIBUTES { public uint Length; public IntPtr RootDirectory; public IntPtr ObjectName; public uint Attributes; public IntPtr SecurityDescriptor; public IntPtr SecurityQualityOfService; }
  [DllImport("advapi32.dll", SetLastError=true)] static extern uint LsaOpenPolicy(IntPtr SystemName, ref LSA_OBJECT_ATTRIBUTES oa, uint access, out IntPtr handle);
  [DllImport("advapi32.dll", SetLastError=true)] static extern uint LsaAddAccountRights(IntPtr handle, byte[] sid, LSA_UNICODE_STRING[] rights, uint count);
  [DllImport("advapi32.dll")] static extern uint LsaClose(IntPtr handle);
  [DllImport("advapi32.dll")] static extern int LsaNtStatusToWinError(uint status);
  public static void Add(byte[] sid, string right) {
    var oa = new LSA_OBJECT_ATTRIBUTES();
    IntPtr h;
    uint st = LsaOpenPolicy(IntPtr.Zero, ref oa, 0x00000810u, out h);
    if (st != 0) throw new System.ComponentModel.Win32Exception(LsaNtStatusToWinError(st));
    try {
      var r = new LSA_UNICODE_STRING[1];
      r[0].Buffer = Marshal.StringToHGlobalUni(right);
      r[0].Length = (ushort)(right.Length * 2);
      r[0].MaximumLength = (ushort)((right.Length + 1) * 2);
      st = LsaAddAccountRights(h, sid, r, 1);
      if (st != 0) throw new System.ComponentModel.Win32Exception(LsaNtStatusToWinError(st));
    } finally { LsaClose(h); }
  }
}
'@
Add-Type -TypeDefinition $src
[BallastLsa]::Add($sidBytes, 'SeServiceLogonRight')
Write-Output 'granted'
`

// grantServiceLogonRight gives account the right to run as a Windows service, so
// the agent can be installed under a domain account (needed for cluster/domain
// operations). Without it CreateService succeeds but the service fails to start
// with a logon error.
func grantServiceLogonRight(account string) error {
	f, err := os.CreateTemp("", "ballast-grant-*.ps1")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(grantLogonRightScript); err != nil {
		f.Close()
		return err
	}
	f.Close()
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path, account)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("grant SeServiceLogonRight to %q: %w: %s", account, err, strings.TrimSpace(string(out)))
	}
	return nil
}

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
	// rights they need; otherwise LocalSystem. The account must hold the
	// SeServiceLogonRight, which we grant here so install is self-sufficient.
	if user != "" {
		if err := grantServiceLogonRight(user); err != nil {
			return err
		}
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
