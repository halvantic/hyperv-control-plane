package hyperv

import (
	"context"
	"fmt"
	"strings"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

// The live half of VM observation: everything that can change while a VM keeps
// running, read for the WHOLE host in one PowerShell invocation.
//
// GetVMState reads everything about one VM — power, memory, CPU, IPs, guest OS
// via KVP/XML, checkpoints, disks (a Get-VHD per disk), adapters (a
// Get-VMNetworkAdapterVlan per adapter), memory config and replication. That is
// ~1.4s per VM, and it ran per VM per pass. Most of what it returns cannot have
// changed: processor count, memory configuration, generation, disks, adapters
// and the VM's own ID settle on a power cycle or an explicit job, not
// spontaneously.
//
// So the cheap, genuinely-live fields are split out and batched. Everything here
// comes from three host-wide queries — Get-VM, Get-VMReplication and one pass of
// Get-VMNetworkAdapter — rather than three per VM, so the cost is flat in the
// number of VMs instead of linear.
//
// Guest OS and FQDN are deliberately NOT here. They come from the
// integration-services KVP exchange, which is the expensive part of the full
// read (a CIM query plus XML parsing per VM) and changes about as often as the
// guest's operating system does.

// VMLive is what a running VM can change about itself between passes.
type VMLive struct {
	Exists              bool
	ID                  string
	PowerState          types.VMPowerState
	AssignedMemoryBytes uint64
	MemoryDemandBytes   uint64
	MemoryStatus        string
	CPUUsagePercent     int
	UptimeSeconds       int64
	IPAddress           string
	Replication         *types.VMReplicationStatus
}

type vmLiveObs struct {
	Exists              bool       `json:"exists"`
	ID                  string     `json:"id"`
	PowerState          string     `json:"powerState"`
	AssignedMemoryBytes uint64     `json:"assignedMemoryBytes"`
	MemoryDemandBytes   uint64     `json:"memoryDemandBytes"`
	MemoryStatus        string     `json:"memoryStatus"`
	CPUUsagePercent     int        `json:"cpuUsagePercent"`
	UptimeSeconds       int64      `json:"uptimeSeconds"`
	IPAddress           string     `json:"ipAddress"`
	Repl                *vmReplObs `json:"repl"`
}

// vmLiveScript is built by a pure function so its content is pinned by tests
// without a host, like vnicsBatchScript and maintenanceScript.
func vmLiveScript(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, psQuote(n))
	}
	return fmt.Sprintf(`$ErrorActionPreference='Stop'
$names = @(%[1]s)
$want = @{}
foreach ($n in $names) { $want[([string]$n).ToLower()] = $true }
# Three host-wide queries, not three per VM. Each is one module load and one
# CIM round trip however many VMs there are.
$repl = @{}
try { foreach ($r in @(Get-VMReplication -ErrorAction SilentlyContinue)) { $repl[([string]$r.VMName).ToLower()] = $r } } catch {}
$adapters = @{}
try { foreach ($a in @(Get-VM -ErrorAction SilentlyContinue | Get-VMNetworkAdapter -ErrorAction SilentlyContinue)) {
  $k = ([string]$a.VMName).ToLower()
  if (-not $adapters.ContainsKey($k)) { $adapters[$k] = @() }
  $adapters[$k] += @($a.IPAddresses)
} } catch {}
$out = @{}
foreach ($vm in @(Get-VM -ErrorAction SilentlyContinue)) {
  $key = ([string]$vm.Name).ToLower()
  if (-not $want.ContainsKey($key)) { continue }
  # Uptime is null for a VM that has never run, and link-local/loopback
  # addresses are noise — the same filter the full read applies.
  $up = 0
  try { $up = [int64]$vm.Uptime.TotalSeconds } catch {}
  $ips = ''
  try { $ips = ((@($adapters[$key]) | Where-Object { $_ -and $_ -notlike 'fe80*' -and $_ -ne '127.0.0.1' }) -join ', ') } catch {}
  $r = $null
  if ($repl.ContainsKey($key)) {
    $rr = $repl[$key]
    $lt = ''
    try { if ($rr.LastReplicationTime) { $lt = $rr.LastReplicationTime.ToUniversalTime().ToString('o') } } catch {}
    $r = [pscustomobject]@{
      mode          = [string]$rr.ReplicationMode
      state         = [string]$rr.State
      health        = [string]$rr.Health
      primaryServer = [string]$rr.PrimaryServer
      replicaServer = [string]$rr.ReplicaServer
      lastRepl      = $lt
      frequencySec  = [int]$rr.FrequencySec
    }
  }
  $out[$key] = [pscustomobject]@{
    exists              = $true
    id                  = [string]$vm.Id
    powerState          = [string]$vm.State
    assignedMemoryBytes = [uint64]$vm.MemoryAssigned
    memoryDemandBytes   = [uint64]$vm.MemoryDemand
    memoryStatus        = [string]$vm.MemoryStatus
    cpuUsagePercent     = [int]$vm.CPUUsage
    uptimeSeconds       = $up
    ipAddress           = [string]$ips
    repl                = $r
  }
}
$out | ConvertTo-Json -Compress -Depth 6
`, strings.Join(quoted, ","))
}

// GetVMLiveStates observes the live state of every named VM in one invocation,
// keyed by lower-cased name. A VM absent from the map is not present on the
// host — the caller treats that the same way it treats Exists false.
func (p *PowerShell) GetVMLiveStates(ctx context.Context, names []string) (map[string]VMLive, error) {
	if len(names) == 0 {
		return map[string]VMLive{}, nil
	}
	out, err := p.run(ctx, vmLiveScript(names))
	if err != nil {
		return nil, fmt.Errorf("get vm live states: %w", err)
	}
	var obs map[string]vmLiveObs
	if err := decodeJSON(out, &obs); err != nil {
		return nil, fmt.Errorf("get vm live states: %w", err)
	}
	live := make(map[string]VMLive, len(obs))
	for k, o := range obs {
		var repl *types.VMReplicationStatus
		if o.Repl != nil {
			repl = &types.VMReplicationStatus{
				Mode:                o.Repl.Mode,
				State:               o.Repl.State,
				Health:              o.Repl.Health,
				PrimaryServer:       o.Repl.PrimaryServer,
				ReplicaServer:       o.Repl.ReplicaServer,
				LastReplicationTime: o.Repl.LastRepl,
				FrequencySeconds:    o.Repl.FrequencySec,
			}
		}
		live[strings.ToLower(k)] = VMLive{
			Exists:              true,
			ID:                  o.ID,
			PowerState:          powerStateFromHyperV(o.PowerState),
			AssignedMemoryBytes: o.AssignedMemoryBytes,
			MemoryDemandBytes:   o.MemoryDemandBytes,
			MemoryStatus:        o.MemoryStatus,
			CPUUsagePercent:     o.CPUUsagePercent,
			UptimeSeconds:       o.UptimeSeconds,
			IPAddress:           o.IPAddress,
			Replication:         repl,
		}
	}
	return live, nil
}

// MergeLive overlays a live reading onto a cached full observation, so a pass
// that skipped the expensive config read still reports complete status.
//
// The live reading wins for everything it carries; the cached state supplies
// only what the live read deliberately does not collect — guest OS and FQDN,
// checkpoints, and the VM's configuration.
func (s VMState) MergeLive(live VMLive) VMState {
	s.Exists = true
	s.ID = live.ID
	s.PowerState = live.PowerState
	s.AssignedMemoryBytes = live.AssignedMemoryBytes
	s.MemoryDemandBytes = live.MemoryDemandBytes
	s.MemoryStatus = live.MemoryStatus
	s.CPUUsagePercent = live.CPUUsagePercent
	s.UptimeSeconds = live.UptimeSeconds
	s.IPAddress = live.IPAddress
	s.Replication = live.Replication
	return s
}
