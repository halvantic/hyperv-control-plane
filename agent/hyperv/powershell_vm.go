package hyperv

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/joshua-fourie/ballast/api/types"
)

// This file is the PowerShell-module backing for VM lifecycle. As with the
// other adapters it shells out to the Hyper-V module: GetVMState observes,
// EnsureVM converges configuration, and SetVMPowerState drives power. Each
// script is idempotent and ends in a single-line ConvertTo-Json result the Go
// side decodes.

type vmObservation struct {
	Exists              bool   `json:"exists"`
	ID                  string `json:"id"`
	PowerState          string `json:"powerState"`
	AssignedMemoryBytes uint64 `json:"assignedMemoryBytes"`
	CPUUsagePercent     int    `json:"cpuUsagePercent"`
	UptimeSeconds       int64  `json:"uptimeSeconds"`
	GuestOS             string `json:"guestOS"`
	IPAddress           string `json:"ipAddress"`
	GuestFQDN           string `json:"guestFQDN"`
}

func (p *PowerShell) GetVMState(ctx context.Context, name string) (VMState, error) {
	// Single-quoted strings only (-Command quoting); the CIM filter's quotes are
	// built with [char]39. Guest OS comes from the integration-services KVP
	// exchange (Msvm_KvpExchangeComponent), IPs from the VM's network adapters.
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$vm = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
if (-not $vm) { [pscustomobject]@{ exists = $false } | ConvertTo-Json -Compress; return }
$ips = ''
try { $ips = (@($vm | Get-VMNetworkAdapter | Select-Object -ExpandProperty IPAddresses | Where-Object { $_ -and $_ -notlike 'fe80*' -and $_ -ne '127.0.0.1' }) -join ', ') } catch {}
$os = ''
$fqdn = ''
try {
  $ns = 'root\virtualization\v2'; $q = [char]39
  $cs = Get-CimInstance -Namespace $ns -ClassName Msvm_ComputerSystem -Filter ('ElementName=' + $q + %[1]s + $q)
  if ($cs) {
    $kvp = Get-CimAssociatedInstance -InputObject $cs -ResultClassName Msvm_KvpExchangeComponent
    foreach ($item in @($kvp.GuestIntrinsicExchangeItems)) {
      $xml = [xml]$item
      $nm = ($xml.INSTANCE.PROPERTY | Where-Object { $_.NAME -eq 'Name' }).VALUE
      if ($nm -eq 'OSName') { $os = ($xml.INSTANCE.PROPERTY | Where-Object { $_.NAME -eq 'Data' }).VALUE }
      if ($nm -eq 'FullyQualifiedDomainName') { $fqdn = ($xml.INSTANCE.PROPERTY | Where-Object { $_.NAME -eq 'Data' }).VALUE }
    }
  }
} catch {}
[pscustomobject]@{
  exists              = $true
  id                  = [string]$vm.Id
  powerState          = [string]$vm.State
  assignedMemoryBytes = [uint64]$vm.MemoryAssigned
  cpuUsagePercent     = [int]$vm.CPUUsage
  uptimeSeconds       = [int64]$vm.Uptime.TotalSeconds
  guestOS             = [string]$os
  ipAddress           = [string]$ips
  guestFQDN           = [string]$fqdn
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
		ID:                  obs.ID,
		PowerState:          powerStateFromHyperV(obs.PowerState),
		AssignedMemoryBytes: obs.AssignedMemoryBytes,
		CPUUsagePercent:     obs.CPUUsagePercent,
		UptimeSeconds:       obs.UptimeSeconds,
		GuestOS:             obs.GuestOS,
		IPAddress:           obs.IPAddress,
		GuestFQDN:           obs.GuestFQDN,
	}, nil
}

// EnsureVM creates the VM if absent and then converges its processor count,
// memory, disks and network adapters. The script reports whether the VM was
// created and whether anything changed, which maps to the Outcome.
func (p *PowerShell) EnsureVM(ctx context.Context, vm types.VM) (VMEnsureResult, error) {
	gen := vm.Spec.HyperVGeneration
	if gen == 0 {
		gen = 2
	}
	script := p.ensureVMScript(vm, gen)

	out, err := p.run(ctx, script)
	if err != nil {
		return VMEnsureResult{}, fmt.Errorf("ensure vm %q: %w", vm.Meta.Name, err)
	}
	var res struct {
		Created         bool `json:"created"`
		Changed         bool `json:"changed"`
		PendingPowerOff bool `json:"pendingPowerOff"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return VMEnsureResult{}, fmt.Errorf("ensure vm %q: %w", vm.Meta.Name, err)
	}
	r := VMEnsureResult{PendingPowerOff: res.PendingPowerOff}
	switch {
	case res.Created:
		r.Outcome = OutcomeCreated
	case res.Changed:
		r.Outcome = OutcomeUpdated
	default:
		r.Outcome = OutcomeUnchanged
	}
	return r, nil
}

// ensureVMScript builds the convergence script for one VM. It is split out so it
// stays readable; the logic is in-script (like EnsureCSV) since it is a sequence
// of idempotent cmdlet checks rather than a single decision.
func (p *PowerShell) ensureVMScript(vm types.VM, gen int) string {
	name := psQuote(vm.Meta.Name)
	s := vm.Spec

	// Processor count and static startup memory cannot change on a running VM, so
	// each is applied only when it differs from actual, and skipped (flagging
	// $pending) when the VM is running. The set commands run against $name. A
	// ProcessorCount of 0 means "do not manage the processor" — used when adopting
	// an existing VM whose CPU/memory the operator has not (yet) declared, so the
	// reconcile never reconfigures what was not asked for.
	procScript := ""
	if s.ProcessorCount > 0 {
		procScript = fmt.Sprintf(`if ((Get-VMProcessor -VMName %[1]s).Count -ne %[2]d) {
  if ($running) { $pending = $true } else { Set-VMProcessor -VMName %[1]s -Count %[2]d; $changed = $true }
}`, name, s.ProcessorCount)
	}

	// Memory diff and apply: static unless dynamic memory is configured. A
	// startup of 0 with no dynamic-memory block means "do not manage memory"
	// (same adoption semantics as ProcessorCount above).
	var memDiff, memApply string
	memScript := ""
	if s.DynamicMemory == nil && s.MemoryStartupBytes == 0 {
		// unmanaged memory: leave memScript empty
	} else if s.DynamicMemory != nil {
		memDiff = fmt.Sprintf("(-not $m.DynamicMemoryEnabled) -or ($m.Startup -ne %d) -or ($m.Minimum -ne %d) -or ($m.Maximum -ne %d)",
			s.MemoryStartupBytes, s.DynamicMemory.MinBytes, s.DynamicMemory.MaxBytes)
		memApply = fmt.Sprintf("Set-VMMemory -VMName %[1]s -DynamicMemoryEnabled $true -StartupBytes %[2]d -MinimumBytes %[3]d -MaximumBytes %[4]d",
			name, s.MemoryStartupBytes, s.DynamicMemory.MinBytes, s.DynamicMemory.MaxBytes)
	} else {
		memDiff = fmt.Sprintf("$m.DynamicMemoryEnabled -or ($m.Startup -ne %d)", s.MemoryStartupBytes)
		memApply = fmt.Sprintf("Set-VMMemory -VMName %[1]s -DynamicMemoryEnabled $false -StartupBytes %[2]d", name, s.MemoryStartupBytes)
	}
	if memDiff != "" {
		memScript = fmt.Sprintf(`$m = Get-VMMemory -VMName %[1]s
if (%[2]s) {
  if ($running) { $pending = $true } else { %[3]s; $changed = $true }
}`, name, memDiff, memApply)
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
		// Consider the desired disk present if it is the attached file OR an
		// ancestor of an attached differencing disk — when the VM has a
		// checkpoint it runs off an .avhdx whose parent chain leads back to this
		// VHDX, so a naive exact-path match would wrongly try to re-attach the
		// (locked) base. Paths are normalised so forward/backslash and casing
		// differences still match.
		disks += create + fmt.Sprintf(`$want = [IO.Path]::GetFullPath(%[2]s)
$present = $false
foreach ($d in (Get-VMHardDiskDrive -VMName %[1]s)) {
  $p = $d.Path
  while ($p) {
    if ([IO.Path]::GetFullPath($p) -ieq $want) { $present = $true; break }
    $vhd = Get-VHD -Path $p -ErrorAction SilentlyContinue
    if ($vhd) { $p = $vhd.ParentPath } else { $p = $null }
  }
  if ($present) { break }
}
if (-not $present) {
  try { Add-VMHardDiskDrive -VMName %[1]s -Path %[2]s; $changed = $true }
  catch {
    # During a live migration the VHDX is held open by the running VM, so a node
    # reconciling mid-migration can transiently see the disk as unattached and
    # fail to (re)attach it with "being used by another process". That is expected
    # and settles once migration completes — treat it as transient, not a failure.
    if ($_.Exception.Message -notlike '*another process*' -and $_.Exception.Message -notlike '*being used*') { throw }
  }
}
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
		// Always drive the VLAN to the desired value: a positive VLAN tags the port
		// (Access mode); VLAN 0 means native/untagged, which must actively clear any
		// existing tag (e.g. when a VM moves from a VLAN-100 dvport to a VLAN-0 one).
		// Both are idempotent no-ops when already in that state.
		if a.VLANID > 0 {
			adapters += fmt.Sprintf("Set-VMNetworkAdapterVlan -VMName %[1]s -VMNetworkAdapterName %[2]s -Access -VlanId %[3]d\n", name, an, a.VLANID)
		} else {
			adapters += fmt.Sprintf("Set-VMNetworkAdapterVlan -VMName %[1]s -VMNetworkAdapterName %[2]s -Untagged\n", name, an)
		}
		if a.MACAddress != "" {
			adapters += fmt.Sprintf("Set-VMNetworkAdapter -VMName %[1]s -Name %[2]s -StaticMacAddress %[3]s\n", name, an, psQuote(a.MACAddress))
		}
	}
	// Remove any adapter not in the declared set — New-VM always creates a default
	// "Network Adapter" (unconnected) and the operator should see only the declared
	// NICs (e.g. in Failover Cluster Manager). Idempotent: steady state has exactly
	// the declared adapters, so nothing is removed. Only done when at least one
	// adapter is declared, so a VM with unmanaged networking is left untouched.
	if len(s.NetworkAdapters) > 0 {
		keep := make([]string, len(s.NetworkAdapters))
		for i, a := range s.NetworkAdapters {
			keep[i] = a.Name
		}
		adapters += fmt.Sprintf(`$keep = @(%[1]s)
foreach ($ad in (Get-VMNetworkAdapter -VMName %[2]s)) {
  if ($keep -notcontains $ad.Name) { Remove-VMNetworkAdapter -VMNetworkAdapter $ad; $changed = $true }
}
`, psStringList(keep), name)
	}

	// Boot ISO: ensure a DVD drive backed by the ISO exists (path-normalised so
	// forward/backslash differences don't re-add it). Hot-pluggable, so safe
	// while running.
	iso := ""
	if s.ISOPath != "" {
		ip := psQuote(s.ISOPath)
		iso = fmt.Sprintf(`$wantIso = [IO.Path]::GetFullPath(%[2]s)
if (-not (Get-VMDvdDrive -VMName %[1]s | Where-Object { $_.Path -and ([IO.Path]::GetFullPath($_.Path) -ieq $wantIso) })) {
  Add-VMDvdDrive -VMName %[1]s -Path %[2]s; $changed = $true
}
`, name, ip)
	} else {
		// No ISO desired: eject any media from the DVD drives (declarative eject;
		// clearing the VM's isoPath removes the disc on the next reconcile). The
		// drive is kept, just emptied. Idempotent — only ejects when a disc is in.
		iso = fmt.Sprintf(`foreach ($dvd in (Get-VMDvdDrive -VMName %[1]s)) {
  if ($dvd.Path) { Set-VMDvdDrive -VMName %[1]s -ControllerNumber $dvd.ControllerNumber -ControllerLocation $dvd.ControllerLocation -Path $null; $changed = $true }
}
`, name)
	}

	// For a clustered VM, never create it locally if it already exists as a cluster
	// role: during a migration the centre may deliver the VM to a node that does not
	// currently hold it (ownership is mid-transition), and an unconditional New-VM
	// there spawns an empty orphan (new GUID, no disk, not clustered). If the role
	// exists, the VM lives on its owner — this node has nothing to do, so bail out.
	clusterGuard := ""
	if vm.Spec.Placement.ClusterName != "" {
		clusterGuard = fmt.Sprintf("  try { if (Get-ClusterGroup -Name %[1]s -ErrorAction SilentlyContinue) { $ownedElsewhere = $true } } catch {}\n", name)
	}

	return fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$created = $false
$changed = $false
$pending = $false
$vm = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
if (-not $vm) {
  $ownedElsewhere = $false
%[10]s  if ($ownedElsewhere) {
    [pscustomobject]@{ created = $false; changed = $false; pendingPowerOff = $false } | ConvertTo-Json -Compress
    return
  }
  New-VM -Name %[1]s -Generation %[2]d -MemoryStartupBytes %[3]d -NoVHD | Out-Null
  $created = $true
}
$running = $false
$cur = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
if ($cur -and $cur.State -ne 'Off') { $running = $true }
%[4]s
%[5]s
%[6]s%[7]s%[8]s%[9]s
[pscustomobject]@{ created = $created; changed = $changed; pendingPowerOff = $pending } | ConvertTo-Json -Compress
`, name, gen, s.MemoryStartupBytes, procScript, memScript, startAction, disks, adapters, iso, clusterGuard)
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
if (-not $vm) { throw 'VM does not exist' }
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

// screenScript captures the VM's console thumbnail via WMI
// (Msvm_VirtualSystemManagementService.GetVirtualSystemThumbnailImage), which
// returns a 320x240 RGB565 buffer, and converts it to a PNG with System.Drawing,
// returned base64. Empty output when the VM has no capturable screen.
const screenW, screenH = 320, 240

func (p *PowerShell) GetVMScreen(ctx context.Context, name string) ([]byte, error) {
	// Note: this script must use only single-quoted strings — the agent runs
	// scripts via powershell.exe -Command, where embedded double quotes get
	// mangled by Windows argument parsing. The CIM filter's required single
	// quotes around the value are built with [char]39.
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$ns = 'root\virtualization\v2'
$q = [char]39
$vm = Get-CimInstance -Namespace $ns -ClassName Msvm_ComputerSystem -Filter ('ElementName=' + $q + %[1]s + $q)
if (-not $vm -or $vm.EnabledState -ne 2) { return }  # 2 = Enabled (running)
$vmms = Get-CimInstance -Namespace $ns -ClassName Msvm_VirtualSystemManagementService
$sd = Get-CimAssociatedInstance -InputObject $vm -ResultClassName Msvm_VirtualSystemSettingData -Association Msvm_SettingsDefineState
$res = Invoke-CimMethod -InputObject $vmms -MethodName GetVirtualSystemThumbnailImage -Arguments @{ TargetSystem = $sd; WidthPixels = [uint16]%[2]d; HeightPixels = [uint16]%[3]d }
if ($res.ReturnValue -ne 0 -or -not $res.ImageData) { return }
Add-Type -AssemblyName System.Drawing
$w = %[2]d; $h = %[3]d
$bmp = New-Object System.Drawing.Bitmap($w, $h, [System.Drawing.Imaging.PixelFormat]::Format16bppRgb565)
$rect = New-Object System.Drawing.Rectangle(0, 0, $w, $h)
$bd = $bmp.LockBits($rect, [System.Drawing.Imaging.ImageLockMode]::WriteOnly, $bmp.PixelFormat)
[System.Runtime.InteropServices.Marshal]::Copy([byte[]]$res.ImageData, 0, $bd.Scan0, ($w * $h * 2))
$bmp.UnlockBits($bd)
$ms = New-Object System.IO.MemoryStream
$bmp.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
$bmp.Dispose()
[Convert]::ToBase64String($ms.ToArray())
`, psQuote(name), screenW, screenH)

	out, err := p.run(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("get vm screen %q: %w", name, err)
	}
	b64 := strings.TrimSpace(string(out))
	if b64 == "" {
		return nil, nil // no screen (VM off or no image)
	}
	png, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode vm screen %q: %w", name, err)
	}
	return png, nil
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
