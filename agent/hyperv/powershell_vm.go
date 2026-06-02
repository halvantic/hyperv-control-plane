package hyperv

import (
	"context"
	"fmt"

	"github.com/joshua-fourie/ballast/api/types"
)

// This file is the PowerShell-module backing for VM lifecycle. As with the
// other adapters it shells out to the Hyper-V module: GetVMState observes,
// EnsureVM converges configuration, and SetVMPowerState drives power. Each
// script is idempotent and ends in a single-line ConvertTo-Json result the Go
// side decodes.

type vmObservation struct {
	Exists              bool   `json:"exists"`
	PowerState          string `json:"powerState"`
	AssignedMemoryBytes uint64 `json:"assignedMemoryBytes"`
	CPUUsagePercent     int    `json:"cpuUsagePercent"`
	UptimeSeconds       int64  `json:"uptimeSeconds"`
}

func (p *PowerShell) GetVMState(ctx context.Context, name string) (VMState, error) {
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$vm = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
if (-not $vm) { [pscustomobject]@{ exists = $false } | ConvertTo-Json -Compress; return }
[pscustomobject]@{
  exists              = $true
  powerState          = [string]$vm.State
  assignedMemoryBytes = [uint64]$vm.MemoryAssigned
  cpuUsagePercent     = [int]$vm.CPUUsage
  uptimeSeconds       = [int64]$vm.Uptime.TotalSeconds
} | ConvertTo-Json -Compress
`, psQuote(name))

	out, err := p.run(ctx, script)
	if err != nil {
		return VMState{}, fmt.Errorf("get vm state %q: %w", name, err)
	}
	var obs vmObservation
	if err := decodeJSON(out, &obs); err != nil {
		return VMState{}, fmt.Errorf("get vm state %q: %w", name, err)
	}
	return VMState{
		Exists:              obs.Exists,
		PowerState:          powerStateFromHyperV(obs.PowerState),
		AssignedMemoryBytes: obs.AssignedMemoryBytes,
		CPUUsagePercent:     obs.CPUUsagePercent,
		UptimeSeconds:       obs.UptimeSeconds,
	}, nil
}

// EnsureVM creates the VM if absent and then converges its processor count,
// memory, disks and network adapters. The script reports whether the VM was
// created and whether anything changed, which maps to the Outcome.
func (p *PowerShell) EnsureVM(ctx context.Context, vm types.VM) (Outcome, error) {
	gen := vm.Spec.HyperVGeneration
	if gen == 0 {
		gen = 2
	}
	script := p.ensureVMScript(vm, gen)

	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure vm %q: %w", vm.Meta.Name, err)
	}
	var res struct {
		Created bool `json:"created"`
		Changed bool `json:"changed"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure vm %q: %w", vm.Meta.Name, err)
	}
	switch {
	case res.Created:
		return OutcomeCreated, nil
	case res.Changed:
		return OutcomeUpdated, nil
	default:
		return OutcomeUnchanged, nil
	}
}

// ensureVMScript builds the convergence script for one VM. It is split out so it
// stays readable; the logic is in-script (like EnsureCSV) since it is a sequence
// of idempotent cmdlet checks rather than a single decision.
func (p *PowerShell) ensureVMScript(vm types.VM, gen int) string {
	name := psQuote(vm.Meta.Name)
	s := vm.Spec

	// Memory: static unless dynamic memory is configured.
	memScript := fmt.Sprintf("Set-VMMemory -VMName %[1]s -DynamicMemoryEnabled $false -StartupBytes %[2]d", name, s.MemoryStartupBytes)
	if s.DynamicMemory != nil {
		memScript = fmt.Sprintf("Set-VMMemory -VMName %[1]s -DynamicMemoryEnabled $true -StartupBytes %[2]d -MinimumBytes %[3]d -MaximumBytes %[4]d",
			name, s.MemoryStartupBytes, s.DynamicMemory.MinBytes, s.DynamicMemory.MaxBytes)
	}

	// Set-VM only when there is an automatic-start-action to apply; calling it
	// with just -Name is rejected.
	startAction := ""
	if arg := automaticStartActionArg(s.AutomaticStartAction); arg != "" {
		startAction = fmt.Sprintf("Set-VM -Name %s %s\n", name, arg)
	}

	disks := ""
	for _, d := range s.Disks {
		path := psQuote(d.Path)
		create := ""
		if d.SizeBytes > 0 {
			sizeFlag := fmt.Sprintf("-Fixed -SizeBytes %d", d.SizeBytes)
			if d.Dynamic {
				sizeFlag = fmt.Sprintf("-Dynamic -SizeBytes %d", d.SizeBytes)
			}
			create = fmt.Sprintf("if (-not (Test-Path %[1]s)) { New-VHD -Path %[1]s %[2]s | Out-Null; $changed = $true }\n", path, sizeFlag)
		}
		disks += create + fmt.Sprintf(`if (-not (Get-VMHardDiskDrive -VMName %[1]s | Where-Object { $_.Path -eq %[2]s })) { Add-VMHardDiskDrive -VMName %[1]s -Path %[2]s; $changed = $true }
`, name, path)
	}

	adapters := ""
	for _, a := range s.NetworkAdapters {
		an := psQuote(a.Name)
		sw := psQuote(a.SwitchName)
		adapters += fmt.Sprintf(`$ad = Get-VMNetworkAdapter -VMName %[1]s | Where-Object { $_.Name -eq %[2]s }
if (-not $ad) { Add-VMNetworkAdapter -VMName %[1]s -Name %[2]s -SwitchName %[3]s; $changed = $true }
else { if ($ad.SwitchName -ne %[4]s) { Connect-VMNetworkAdapter -VMName %[1]s -Name %[2]s -SwitchName %[3]s; $changed = $true } }
`, name, an, sw, psQuote(a.SwitchName))
		if a.VLANID > 0 {
			adapters += fmt.Sprintf("Set-VMNetworkAdapterVlan -VMName %[1]s -VMNetworkAdapterName %[2]s -Access -VlanId %[3]d\n", name, an, a.VLANID)
		}
		if a.MACAddress != "" {
			adapters += fmt.Sprintf("Set-VMNetworkAdapter -VMName %[1]s -Name %[2]s -StaticMacAddress %[3]s\n", name, an, psQuote(a.MACAddress))
		}
	}

	return fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$created = $false
$changed = $false
$vm = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
if (-not $vm) {
  New-VM -Name %[1]s -Generation %[2]d -MemoryStartupBytes %[3]d -NoVHD | Out-Null
  $created = $true
}
Set-VMProcessor -VMName %[1]s -Count %[4]d
%[5]s
%[6]s%[7]s%[8]s
[pscustomobject]@{ created = $created; changed = $changed } | ConvertTo-Json -Compress
`, name, gen, s.MemoryStartupBytes, s.ProcessorCount, memScript, startAction, disks, adapters)
}

// SetVMPowerState drives the VM to Running (Start-VM) or Off (Stop-VM). It reads
// current state first so a no-op returns OutcomeUnchanged.
func (p *PowerShell) SetVMPowerState(ctx context.Context, name string, desired types.VMPowerState) (Outcome, error) {
	var verb string
	switch desired {
	case types.VMPowerRunning:
		verb = fmt.Sprintf("Start-VM -Name %s", psQuote(name))
	case types.VMPowerOff:
		verb = fmt.Sprintf("Stop-VM -Name %s -Force", psQuote(name))
	default:
		// Paused/Saved are observed, never requested; treat as no-op.
		return OutcomeUnchanged, nil
	}

	target := "Running"
	if desired == types.VMPowerOff {
		target = "Off"
	}
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$vm = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
if (-not $vm) { throw 'VM %[1]s does not exist' }
if ([string]$vm.State -eq '%[2]s') { [pscustomobject]@{ changed = $false } | ConvertTo-Json -Compress; return }
%[3]s | Out-Null
[pscustomobject]@{ changed = $true } | ConvertTo-Json -Compress
`, psQuote(name), target, verb)

	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("set vm power %q: %w", name, err)
	}
	var res struct {
		Changed bool `json:"changed"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return OutcomeUnchanged, fmt.Errorf("set vm power %q: %w", name, err)
	}
	if res.Changed {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
}

// powerStateFromHyperV maps the Hyper-V VMState enum string to the schema power
// state. Hyper-V reports "Running", "Off", "Paused", "Saved" among others.
func powerStateFromHyperV(s string) types.VMPowerState {
	switch s {
	case "Running":
		return types.VMPowerRunning
	case "Off":
		return types.VMPowerOff
	case "Paused":
		return types.VMPowerPaused
	case "Saved":
		return types.VMPowerSaved
	default:
		return types.VMPowerState(s)
	}
}

// automaticStartActionArg renders the -AutomaticStartAction flag for Set-VM, or
// empty when no action is specified (leaving the Hyper-V default).
func automaticStartActionArg(a types.VMStartAction) string {
	switch a {
	case types.VMStartNothing:
		return "-AutomaticStartAction Nothing"
	case types.VMStartIfWasRunning:
		return "-AutomaticStartAction StartIfRunning"
	case types.VMStartAlways:
		return "-AutomaticStartAction Start"
	default:
		return ""
	}
}
