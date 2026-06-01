package hyperv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// psQuote renders s as a PowerShell single-quoted string literal, escaping any
// embedded single quotes. All host/switch/adapter names pass through this
// before being interpolated into a script.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// decodeJSON unmarshals PowerShell JSON output into v. Empty output (a cmdlet
// that produced nothing) is treated as the zero value rather than an error.
func decodeJSON(out []byte, v any) error {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("decode powershell output: %w (output: %q)", err, string(out))
	}
	return nil
}

// inventoryScript collects physical adapters, physical disks, memory and CPU in
// one invocation. The JSON keys match the api/types json tags so the result
// unmarshals straight into types.HostInventory.
const inventoryScript = `
$ErrorActionPreference = 'Stop'
$adapters = Get-NetAdapter -Physical -ErrorAction SilentlyContinue | ForEach-Object {
  [pscustomobject]@{ name = $_.Name; mac = $_.MacAddress; linkSpeedBps = [uint64]$_.Speed; up = ($_.Status -eq 'Up') }
}
$disks = Get-PhysicalDisk -ErrorAction SilentlyContinue | ForEach-Object {
  [pscustomobject]@{ deviceId = [string]$_.DeviceId; sizeBytes = [uint64]$_.Size; mediaType = [string]$_.MediaType; canPool = [bool]$_.CanPool }
}
$cs = Get-CimInstance Win32_ComputerSystem
[pscustomobject]@{
  physicalAdapters = @($adapters)
  physicalDisks    = @($disks)
  totalMemoryBytes = [uint64]$cs.TotalPhysicalMemory
  logicalCPUs      = [int]$cs.NumberOfLogicalProcessors
} | ConvertTo-Json -Depth 5 -Compress
`

// CollectInventory observes host hardware via Get-NetAdapter, Get-PhysicalDisk
// and Win32_ComputerSystem. It is a pure read.
func (p *PowerShell) CollectInventory(ctx context.Context) (types.HostInventory, error) {
	out, err := p.run(ctx, inventoryScript)
	if err != nil {
		return types.HostInventory{}, fmt.Errorf("collect inventory: %w", err)
	}
	var inv types.HostInventory
	if err := decodeJSON(out, &inv); err != nil {
		return types.HostInventory{}, fmt.Errorf("collect inventory: %w", err)
	}
	return inv, nil
}

// compile-time assertion that PowerShell satisfies the interface.
var _ Interface = (*PowerShell)(nil)
