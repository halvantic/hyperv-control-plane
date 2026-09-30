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

/* Two host-wide WMI reads that replace per-object cmdlet work, shared by the
   live read and the VM inventory. Measured on HVNEW06 (14 VMs, 31 NICs):

   - Guest IPs. Each adapter's IPAddresses is fetched on demand, ~92ms apiece,
     2.9s for the host. Msvm_GuestNetworkAdapterConfiguration holds the same
     addresses for every guest NIC in one query, 1.8s, and returned exactly the
     cmdlet's IPs on all fourteen VMs.
   - Replication. Get-VMReplication cost 2.5s to report that nothing on the
     host was replicated. Msvm_ComputerSystem carries each VM's ReplicationMode
     in one query, 0.23s, so Get-VMReplication runs only for VMs that have any.

   Both fall back to the cmdlet rather than guess. A NIC with no WMI record
   reads its own IPAddresses, and replication is read as before if the query
   fails, or any VM's mode is missing, or a VM is not in the answer. So the worst
   case is the old cost, never a missing or wrong reading.

   The IPs keep the ADAPTER order, not WMI's: a runbook gate probes the first
   routable IPv4 (firstGuestIP in centre/controllers/runbook.go), and a
   multi-homed guest's order decides which network that is. */

// guestIPsScript defines __ips: the guest IPs of one adapter object, from the
// WMI map keyed "<VM GUID>\<NIC GUID>", which is the adapter's Id without its
// "Microsoft:" prefix.
const guestIPsScript = `$__gip = @{}
try { foreach ($__g in @(Get-CimInstance -Namespace 'root\virtualization\v2' -ClassName Msvm_GuestNetworkAdapterConfiguration -ErrorAction Stop)) {
  $__p = ([string]$__g.InstanceID) -split '\\'
  if ($__p.Count -ge 3) { $__gip[$__p[1] + '\' + $__p[2]] = @($__g.IPAddresses) }
} } catch {}
function __ips($a) {
  $__key = ([string]$a.Id) -replace '^Microsoft:', ''
  if ($__key -and $__gip.ContainsKey($__key)) { $__gip[$__key] } else { @($a.IPAddresses) }
}
`

// replicationModeScript defines __replicated: whether a VM (by Id) may have a
// replication relationship worth asking Get-VMReplication about. True unless
// WMI said, for that VM, that its mode is None.
const replicationModeScript = `$__replMode = $null
try {
  $__cs = @(Get-CimInstance -Namespace 'root\virtualization\v2' -ClassName Msvm_ComputerSystem -ErrorAction Stop | Where-Object { [string]$_.Name -match '^[0-9A-Fa-f]{8}-' })
  if (@($__cs | Where-Object { $null -eq $_.ReplicationMode }).Count -eq 0) {
    $__replMode = @{}
    foreach ($__c in $__cs) { $__replMode[[string]$__c.Name] = [int]$__c.ReplicationMode }
  }
} catch {}
function __replicated($id) {
  $__i = [string]$id
  ($null -eq $__replMode) -or (-not $__replMode.ContainsKey($__i)) -or ($__replMode[$__i] -ne 0)
}
`

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
$__vms = @(Get-VM -ErrorAction SilentlyContinue)
# Host-wide queries, not per VM: see guestIPsScript and replicationModeScript.
%[2]s$repl = @{}
if (@($__vms | Where-Object { $want.ContainsKey(([string]$_.Name).ToLower()) -and (__replicated $_.Id) }).Count -gt 0) {
  try { foreach ($r in @(Get-VMReplication -ErrorAction SilentlyContinue)) { $repl[([string]$r.VMName).ToLower()] = $r } } catch {}
}
%[3]s# -VMName * and not Get-VM piped in: the pipeline makes a round trip per VM.
# 3.6s against 0.48s for the same fourteen VMs on HVNEW06.
$adapters = @{}
try { foreach ($a in @(Get-VMNetworkAdapter -VMName * -ErrorAction SilentlyContinue)) {
  $k = ([string]$a.VMName).ToLower()
  if (-not $adapters.ContainsKey($k)) { $adapters[$k] = @() }
  $adapters[$k] += @(__ips $a)
} } catch {}
$out = @{}
foreach ($vm in $__vms) {
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
`, strings.Join(quoted, ","), replicationModeScript, guestIPsScript)
}

// GetVMLiveStates observes the live state of every named VM in one invocation,
// keyed by lower-cased name. A VM absent from the map is not present on the
// host — the caller treats that the same way it treats Exists false.
func (p *PowerShell) GetVMLiveStates(ctx context.Context, names []string) (map[string]VMLive, error) {
	if len(names) == 0 {
		return map[string]VMLive{}, nil
	}
	// Pooled: read-only, and held by a test to nothing unsafe in a shared process.
	out, err := p.runPooled(ctx, vmLiveScript(names))
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
