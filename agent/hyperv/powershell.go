package hyperv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"log/slog"

	"github.com/joshua-fourie/ballast/api/types"
)

// PowerShell is the v1 host implementation of Interface. It shells out to the
// Windows PowerShell management modules (Hyper-V, NetAdapter, NetTCPIP) per the
// stack decision in CLAUDE.md: every operation is an explicit, documented
// cmdlet sequence, easy to target, with hot paths to be migrated to direct
// WMI/CIM later.
//
// Each operation is split into a read script (observe actual state) and, only
// when needed, an act script (create or adjust). The decision between them is
// pure Go (see plan* functions) so it is unit-tested without a host; the
// scripts themselves require a real Hyper-V host to validate.
type PowerShell struct {
	run runFunc
	log *slog.Logger
}

// runFunc executes a PowerShell script and returns its stdout. It is a field so
// tests can inject canned output in place of a real shell-out.
type runFunc func(ctx context.Context, script string) ([]byte, error)

// NewPowerShell returns a host implementation backed by powershell.exe.
func NewPowerShell(log *slog.Logger) *PowerShell {
	if log == nil {
		log = slog.Default()
	}
	return &PowerShell{run: execPowerShell, log: log}
}

// execPowerShell runs a script under Windows PowerShell. It uses powershell.exe
// (5.1) rather than pwsh because the Hyper-V and NetAdapter modules target it.
func execPowerShell(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("powershell: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// runWithEnv executes a script via powershell.exe with extra environment
// variables (used to pass secrets without putting them on the command line). It
// bypasses the injectable run field — only the real host implementation needs
// it, and it is never unit-tested through the stub.
func (p *PowerShell) runWithEnv(ctx context.Context, script string, extraEnv []string) error {
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("powershell: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// psQuote renders s as a PowerShell single-quoted string literal, escaping any
// embedded single quotes. All host/switch/adapter names pass through this
// before being interpolated into a script.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// decodeJSON unmarshals PowerShell JSON output into v. Empty output (a cmdlet
// that produced nothing) is treated as the zero value rather than an error.
//
// Some cluster cmdlets (e.g. Get-ClusterStorageSpacesDirect) emit warning lines
// ahead of their result. Since every script here ends with
// `ConvertTo-Json -Compress` (single-line output), we take the last non-empty
// line as the JSON, tolerating any such preamble.
func decodeJSON(out []byte, v any) error {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil
	}
	// Strip any leading warning/preamble lines: the JSON begins at the first
	// line starting with { or [. This keeps multi-line JSON intact.
	if !startsWithJSON(out) {
		lines := bytes.Split(out, []byte("\n"))
		for i, line := range lines {
			if startsWithJSON(bytes.TrimSpace(line)) {
				out = bytes.TrimSpace(bytes.Join(lines[i:], []byte("\n")))
				break
			}
		}
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("decode powershell output: %w (output: %q)", err, string(out))
	}
	return nil
}

func startsWithJSON(b []byte) bool {
	return len(b) > 0 && (b[0] == '{' || b[0] == '[')
}

// inventoryScript collects physical adapters, physical disks, memory and CPU in
// one invocation. The JSON keys match the api/types json tags so the result
// unmarshals straight into types.HostInventory.
//
// A physical adapter is reported as management when it carries a statically
// configured (Manual) IPv4 address — that is the host's management identity (the
// address it is known by in DNS), and such a NIC must never be teamed into a
// vSwitch. A DHCP-assigned address does not make a NIC management: it is free to
// assign to a switch, so its IP is not reported (the frontend treats a NIC with
// no host IP and no switch as free). A NIC bound to a vSwitch carries no IP here
// (the address lives on its management-OS vNIC), so it is naturally not flagged.
func inventoryScript() string {
	return `
$ErrorActionPreference = 'Stop'
$adapters = Get-NetAdapter -Physical -ErrorAction SilentlyContinue | ForEach-Object {
  $a = Get-NetIPAddress -InterfaceIndex $_.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -notlike '169.254.*' } | Select-Object -First 1
  $static = [bool]($a -and $a.PrefixOrigin -eq 'Manual')
  $ip = ''
  if ($static) { $ip = [string]$a.IPAddress }
  $dns = @((Get-DnsClientServerAddress -InterfaceIndex $_.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue).ServerAddresses)
  $reg = [bool](Get-DnsClient -InterfaceIndex $_.ifIndex -ErrorAction SilentlyContinue).RegisterThisConnectionsAddress
  [pscustomobject]@{ name = $_.Name; mac = $_.MacAddress; linkSpeedBps = [uint64]$_.Speed; up = ($_.Status -eq 'Up'); isManagement = $static; ipv4 = $ip; dnsServers = @($dns); registersDNS = $reg }
}
$osIds = @()
try { $osIds = @(Get-Disk -ErrorAction SilentlyContinue | Where-Object { $_.IsBoot -or $_.IsSystem } | Get-PhysicalDisk -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.DeviceId }) } catch {}
# In an S2D cluster Get-PhysicalDisk returns the whole cluster pool, so a host
# would report every node's disks. Keep only the disks whose physically-connected
# storage node is this host (mapping per disk, since the node->disk direction
# omits pool-eligible disks); disks with no node info (standalone) are kept too.
# Fall back to all disks if the filter yields nothing.
$cn = $env:COMPUTERNAME
$pdisks = @(Get-PhysicalDisk -ErrorAction SilentlyContinue | Where-Object {
  $nodes = @($_ | Get-StorageNode -PhysicallyConnected -ErrorAction SilentlyContinue | ForEach-Object { $_.Name })
  (-not $nodes) -or (@($nodes | Where-Object { $_ -like ($cn + '*') }).Count -gt 0)
})
if (-not $pdisks -or $pdisks.Count -eq 0) { $pdisks = @(Get-PhysicalDisk -ErrorAction SilentlyContinue) }
$disks = $pdisks | ForEach-Object {
  [pscustomobject]@{ deviceId = [string]$_.DeviceId; sizeBytes = [uint64]$_.Size; mediaType = [string]$_.MediaType; canPool = [bool]$_.CanPool; isOSDisk = ([string]$_.DeviceId -in $osIds) }
}
$cs = Get-CimInstance Win32_ComputerSystem
$os = Get-CimInstance Win32_OperatingSystem
[pscustomobject]@{
  physicalAdapters = @($adapters)
  physicalDisks    = @($disks)
  totalMemoryBytes = [uint64]$cs.TotalPhysicalMemory
  logicalCPUs      = [int]$cs.NumberOfLogicalProcessors
  osVersion        = [string]($os.Caption + ' ' + $os.Version).Trim()
} | ConvertTo-Json -Depth 5 -Compress
`
}

// CollectInventory observes host hardware via Get-NetAdapter, Get-PhysicalDisk
// and Win32_ComputerSystem. It is a pure read.
func (p *PowerShell) CollectInventory(ctx context.Context) (types.HostInventory, error) {
	out, err := p.run(ctx, inventoryScript())
	if err != nil {
		return types.HostInventory{}, fmt.Errorf("collect inventory: %w", err)
	}
	var inv types.HostInventory
	if err := decodeJSON(out, &inv); err != nil {
		return types.HostInventory{}, fmt.Errorf("collect inventory: %w", err)
	}
	return inv, nil
}

// metricsScript reads live host utilisation: overall CPU load (averaged across
// processors), physical memory in use (total visible minus free), and uptime
// since last boot. JSON keys match types.HostMetrics.
const metricsScript = `
$ErrorActionPreference = 'Stop'
$os = Get-CimInstance Win32_OperatingSystem
$cpu = (Get-CimInstance Win32_Processor | Measure-Object -Property LoadPercentage -Average).Average
[pscustomobject]@{
  cpuUsagePercent  = [int]$cpu
  memoryInUseBytes = [uint64]((($os.TotalVisibleMemorySize - $os.FreePhysicalMemory)) * 1024)
  uptimeSeconds    = [int64]((Get-Date) - $os.LastBootUpTime).TotalSeconds
} | ConvertTo-Json -Compress
`

// CollectMetrics observes live CPU load, memory in use and uptime via CIM. It is
// a pure read.
func (p *PowerShell) CollectMetrics(ctx context.Context) (types.HostMetrics, error) {
	out, err := p.run(ctx, metricsScript)
	if err != nil {
		return types.HostMetrics{}, fmt.Errorf("collect metrics: %w", err)
	}
	var m types.HostMetrics
	if err := decodeJSON(out, &m); err != nil {
		return types.HostMetrics{}, fmt.Errorf("collect metrics: %w", err)
	}
	return m, nil
}

// resourcesScript observes existing vSwitches, storage volumes (CSV mount points
// when clustered, else fixed local volumes) and ISO files under each volume's
// ISOs folder and C:\ISOs. JSON keys match types.HostResources. All lookups are
// best-effort so a non-clustered or sparse host still returns what it can.
const resourcesScript = `
$ErrorActionPreference = 'SilentlyContinue'
$switchDetails = @(Get-VMSwitch | ForEach-Object {
  $sw = $_
  $descs = @()
  if ($sw.NetAdapterInterfaceDescriptions) { $descs = @($sw.NetAdapterInterfaceDescriptions) }
  elseif ($sw.NetAdapterInterfaceDescription) { $descs = @($sw.NetAdapterInterfaceDescription) }
  $nics = @($descs | ForEach-Object { (Get-NetAdapter -InterfaceDescription $_ -ErrorAction SilentlyContinue).Name } | Where-Object { $_ })
  if (-not $nics) { $nics = @($descs | Where-Object { $_ }) }
  $vlan = 0
  $mgmt = @(Get-VMNetworkAdapter -ManagementOS -ErrorAction SilentlyContinue | Where-Object { $_.SwitchName -eq $sw.Name })
  if ($mgmt.Count -gt 0) {
    $v = $mgmt | Get-VMNetworkAdapterVlan -ErrorAction SilentlyContinue | Where-Object { $_.OperationMode -eq 'Access' } | Select-Object -First 1
    if ($v) { $vlan = [int]$v.AccessVlanId }
  }
  [pscustomobject]@{ name = [string]$sw.Name; netAdapters = @($nics); allowManagementOS = [bool]$sw.AllowManagementOS; vlanId = [int]$vlan }
})
$switches = @($switchDetails | ForEach-Object { $_.name } | Where-Object { $_ })
$vols = @()
$csv = Get-ClusterSharedVolume 2>$null
if ($csv) {
  $vols = @($csv | ForEach-Object {
    $p = $_.SharedVolumeInfo.Partition
    [pscustomobject]@{ name = [string]$_.Name; path = [string]$_.SharedVolumeInfo.FriendlyVolumeName; sizeBytes = [uint64]$p.Size; usedBytes = [uint64]($p.Size - $p.FreeSpace) }
  })
} else {
  $vols = @(Get-Volume | Where-Object { $_.DriveType -eq 'Fixed' -and $_.DriveLetter } | ForEach-Object {
    [pscustomobject]@{ name = "$($_.DriveLetter):"; path = "$($_.DriveLetter):\"; sizeBytes = [uint64]$_.Size; usedBytes = [uint64]($_.Size - $_.SizeRemaining) }
  })
}
$roots = @($vols | ForEach-Object { Join-Path $_.path 'ISOs' }) + 'C:\ISOs'
$isos = @()
foreach ($r in $roots) {
  if (Test-Path $r) { $isos += @((Get-ChildItem -Path $r -Filter *.iso -File -Recurse -Depth 1).FullName) }
}
[pscustomobject]@{ switches = @($switches); switchDetails = @($switchDetails); volumes = @($vols); isos = @($isos) } | ConvertTo-Json -Depth 4 -Compress
`

// CollectResources observes existing switches, storage volumes and ISO files.
func (p *PowerShell) CollectResources(ctx context.Context) (types.HostResources, error) {
	out, err := p.run(ctx, resourcesScript)
	if err != nil {
		return types.HostResources{}, fmt.Errorf("collect resources: %w", err)
	}
	var r types.HostResources
	if err := decodeJSON(out, &r); err != nil {
		return types.HostResources{}, fmt.Errorf("collect resources: %w", err)
	}
	return r, nil
}

// compile-time assertion that PowerShell satisfies the interface.
var _ Interface = (*PowerShell)(nil)
