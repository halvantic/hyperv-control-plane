package hyperv

import (
	"context"
	"fmt"
)

// roleObservation mirrors the JSON emitted by the host-role read script.
type roleObservation struct {
	HyperVInstalled bool `json:"hyperVInstalled"`
	RebootPending   bool `json:"rebootPending"`
}

const roleStateScript = `
$ErrorActionPreference = 'Stop'
$f = Get-WindowsFeature -Name Hyper-V -ErrorAction SilentlyContinue
$installed = $false
if ($f) { $installed = ($f.InstallState -eq 'Installed') }
$pending = $false
if (Test-Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending') { $pending = $true }
if (Test-Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\WindowsUpdate\Auto Update\RebootRequired') { $pending = $true }
[pscustomobject]@{ hyperVInstalled = $installed; rebootPending = $pending } | ConvertTo-Json -Compress
`

func (p *PowerShell) GetHostRoleState(ctx context.Context) (HostRoleState, error) {
	out, err := p.run(ctx, roleStateScript)
	if err != nil {
		return HostRoleState{}, fmt.Errorf("get host role state: %w", err)
	}
	var obs roleObservation
	if err := decodeJSON(out, &obs); err != nil {
		return HostRoleState{}, fmt.Errorf("get host role state: %w", err)
	}
	return HostRoleState{HyperVInstalled: obs.HyperVInstalled, RebootPending: obs.RebootPending}, nil
}

// installRoleScript installs the role without rebooting and reports whether it
// actually changed anything (FeatureResult is non-empty only when it did), so
// EnsureHyperVRole can distinguish a fresh install from an already-present role.
const installRoleScript = `
$ErrorActionPreference = 'Stop'
$r = Install-WindowsFeature -Name Hyper-V -IncludeManagementTools
$changed = (@($r.FeatureResult) | Measure-Object).Count -gt 0
[pscustomobject]@{ changed = $changed } | ConvertTo-Json -Compress
`

func (p *PowerShell) EnsureHyperVRole(ctx context.Context) (Outcome, error) {
	out, err := p.run(ctx, installRoleScript)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("install Hyper-V role: %w", err)
	}
	var res struct {
		Changed bool `json:"changed"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return OutcomeUnchanged, fmt.Errorf("install Hyper-V role: %w", err)
	}
	if res.Changed {
		return OutcomeCreated, nil
	}
	return OutcomeUnchanged, nil
}

func (p *PowerShell) RebootHost(ctx context.Context, drain bool) error {
	// The host restarts and the agent process is terminated; on boot the service
	// restarts and reconciles again. There is no meaningful return value.
	if drain {
		if err := p.drainNode(ctx); err != nil {
			return err
		}
	}
	if err := p.run2(ctx, "$ErrorActionPreference='Stop'; Restart-Computer -Force"); err != nil {
		return fmt.Errorf("reboot host: %w", err)
	}
	return nil
}

// EnableRDP turns on Remote Desktop: clear the deny-connections policy and enable
// the Remote Desktop firewall rule group. Idempotent — re-running is a no-op.
func (p *PowerShell) EnableRDP(ctx context.Context) error {
	script := `$ErrorActionPreference='Stop'
Set-ItemProperty -Path 'HKLM:\System\CurrentControlSet\Control\Terminal Server' -Name 'fDenyTSConnections' -Value 0
Enable-NetFirewallRule -DisplayGroup 'Remote Desktop' -ErrorAction SilentlyContinue`
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("enable RDP: %w", err)
	}
	return nil
}

func (p *PowerShell) ShutdownHost(ctx context.Context, drain bool) error {
	// The host powers off and the agent process is terminated; it returns only
	// when the host is powered back on and the service restarts.
	if drain {
		if err := p.drainNode(ctx); err != nil {
			return err
		}
	}
	if err := p.run2(ctx, "$ErrorActionPreference='Stop'; Stop-Computer -Force"); err != nil {
		return fmt.Errorf("shutdown host: %w", err)
	}
	return nil
}

// drainNode gracefully takes a cluster node out of service before power-off:
// Suspend-ClusterNode -Drain live-migrates its roles/VMs onto the other nodes,
// then S2D storage maintenance suspends its disks so the pool does not start a
// repair while the node is briefly away. Best-effort on the storage step (a
// standalone host has no cluster and the command is a no-op). Blocks until the
// drain completes so we never power off with roles still live on the node.
func (p *PowerShell) drainNode(ctx context.Context) error {
	script := `$ErrorActionPreference='Stop'; $n=$env:COMPUTERNAME; ` +
		`if (Get-Command Suspend-ClusterNode -ErrorAction SilentlyContinue) { ` +
		`try { Suspend-ClusterNode -Name $n -Drain -Wait } catch { try { Suspend-ClusterNode -Name $n -Drain } catch {} }; ` +
		`try { Get-StorageFaultDomain -Type StorageScaleUnit -ErrorAction Stop | Where-Object FriendlyName -EQ $n | Enable-StorageMaintenanceMode -ErrorAction Stop } catch {} }`
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("drain node before power action: %w", err)
	}
	return nil
}
