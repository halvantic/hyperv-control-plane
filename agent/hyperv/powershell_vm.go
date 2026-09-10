package hyperv

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/joshua-fourie/ballast/api/types"
)

// This file is the PowerShell-module backing for VM lifecycle. As with the
// other adapters it shells out to the Hyper-V module: GetVMState observes,
// EnsureVM converges configuration, and SetVMPowerState drives power. Each
// script is idempotent and ends in a single-line ConvertTo-Json result the Go
// side decodes.

type vmCheckpointObs struct {
	Name       string `json:"name"`
	ParentName string `json:"parentName"`
	Type       string `json:"type"`
	CreatedAt  string `json:"createdAt"`
	IsCurrent  bool   `json:"isCurrent"`
}

type vmDiskObs struct {
	Path      string `json:"path"`
	SizeBytes uint64 `json:"sizeBytes"`
}

type vmNicObs struct {
	Name       string `json:"name"`
	SwitchName string `json:"switchName"`
	VLANID     int    `json:"vlanID"`
}

type vmObservation struct {
	Exists              bool              `json:"exists"`
	ID                  string            `json:"id"`
	PowerState          string            `json:"powerState"`
	AssignedMemoryBytes uint64            `json:"assignedMemoryBytes"`
	MemoryDemandBytes   uint64            `json:"memoryDemandBytes"`
	MemoryStatus        string            `json:"memoryStatus"`
	Heartbeat           string            `json:"heartbeat"`
	CPUUsagePercent     int               `json:"cpuUsagePercent"`
	UptimeSeconds       int64             `json:"uptimeSeconds"`
	GuestOS             string            `json:"guestOS"`
	IPAddress           string            `json:"ipAddress"`
	GuestFQDN           string            `json:"guestFQDN"`
	Checkpoints         []vmCheckpointObs `json:"checkpoints"`
	ProcessorCount      int               `json:"processorCount"`
	MemoryStartup       uint64            `json:"memoryStartup"`
	DynamicMemory       bool              `json:"dynamicMemory"`
	MemMin              uint64            `json:"memMin"`
	MemMax              uint64            `json:"memMax"`
	Generation          int               `json:"generation"`
	Disks               []vmDiskObs       `json:"disks"`
	Nics                []vmNicObs        `json:"nics"`
	// Integ is what the guest services are ACTUALLY set to. Read with the rest
	// of the configuration rather than on demand: nearly every VM declares none
	// of them, so the declared value alone tells an operator nothing about the
	// machine in front of them.
	Integ []vmIntegObs `json:"integ"`
	Repl  *vmReplObs   `json:"repl"`
}

// vmIntegObs is one guest integration service as the host reports it.
type vmIntegObs struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type vmReplObs struct {
	Mode          string `json:"mode"`
	State         string `json:"state"`
	Health        string `json:"health"`
	PrimaryServer string `json:"primaryServer"`
	ReplicaServer string `json:"replicaServer"`
	LastRepl      string `json:"lastRepl"`
	FrequencySec  int    `json:"frequencySec"`
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
$cps = @()
try {
  $cur = [string]$vm.ParentSnapshotName
  foreach ($s in @(Get-VMSnapshot -VMName %[1]s -ErrorAction SilentlyContinue)) {
    $cps += [pscustomobject]@{
      name       = [string]$s.Name
      parentName = [string]$s.ParentSnapshotName
      type       = [string]$s.SnapshotType
      createdAt  = $s.CreationTime.ToString('o')
      isCurrent  = ([string]$s.Name -eq $cur)
    }
  }
} catch {}
# Actual VM configuration — disks (with paths), adapters and memory — so the
# centre can show and adopt VMs Ballast did not create.
$disks = @()
try { foreach ($d in @(Get-VMHardDiskDrive -VMName %[1]s -ErrorAction SilentlyContinue)) {
  $sz = 0; try { $sz = [uint64]((Get-VHD -Path $d.Path -ErrorAction Stop).Size) } catch {}
  $disks += [pscustomobject]@{ path = [string]$d.Path; sizeBytes = [uint64]$sz }
} } catch {}
$nics = @()
try { foreach ($a in @(Get-VMNetworkAdapter -VMName %[1]s -ErrorAction SilentlyContinue)) {
  $vl = 0; try { $vv = Get-VMNetworkAdapterVlan -VMNetworkAdapter $a -ErrorAction SilentlyContinue; if ($vv -and $vv.OperationMode -eq 'Access') { $vl = [int]$vv.AccessVlanId } } catch {}
  $nics += [pscustomobject]@{ name = [string]$a.Name; switchName = [string]$a.SwitchName; vlanID = [int]$vl }
} } catch {}
# The guest integration services, as they actually are. Read with the rest of
# the configuration because the declared value is almost always "nothing
# declared", which says nothing about the guest in front of the operator.
$integ = @()
try { foreach ($i in @(Get-VMIntegrationService -VMName %[1]s -ErrorAction SilentlyContinue)) {
  $integ += [pscustomobject]@{ name = [string]$i.Name; enabled = [bool]$i.Enabled }
} } catch {}
$dm = $false; $mmin = [uint64]0; $mmax = [uint64]0
try { $dm = [bool]$vm.DynamicMemoryEnabled; $mmin = [uint64]$vm.MemoryMinimum; $mmax = [uint64]$vm.MemoryMaximum } catch {}
$repl = $null
try {
  $rr = Get-VMReplication -VMName %[1]s -ErrorAction SilentlyContinue
  if ($rr) {
    $lt = ''
    try { if ($rr.LastReplicationTime) { $lt = $rr.LastReplicationTime.ToUniversalTime().ToString('o') } } catch {}
    $repl = [pscustomobject]@{
      mode          = [string]$rr.ReplicationMode
      state         = [string]$rr.State
      health        = [string]$rr.Health
      primaryServer = [string]$rr.PrimaryServer
      replicaServer = [string]$rr.ReplicaServer
      lastRepl      = $lt
      frequencySec  = [int]$rr.FrequencySec
    }
  }
} catch {}
[pscustomobject]@{
  exists              = $true
  id                  = [string]$vm.Id
  powerState          = [string]$vm.State
  assignedMemoryBytes = [uint64]$vm.MemoryAssigned
  # What the guest actually wants, as opposed to what the host handed it.
  # Assigned equals startup for a static-memory VM, so assigned-vs-configured is
  # always full and says nothing about usage. Demand is the real figure, and
  # Hyper-V reports it for static and dynamic VMs alike provided the guest's
  # integration services are running — it is 0 when they are not, or when the VM
  # is off, which the console must present as "unavailable" rather than as zero
  # usage. MemoryStatus ("OK"/"Low"/"Warning") is the host's own verdict.
  memoryDemandBytes   = [uint64]$vm.MemoryDemand
  memoryStatus        = [string]$vm.MemoryStatus
  # The integration layer's own verdict on the guest, which nothing else here
  # reports. "OkApplicationsHealthy"/"OkApplicationsUnknown" mean the heartbeat is
  # arriving; "Lost", "NoContact" and "Error" mean it is not. Without it, a guest
  # whose display is blank and whose console will not respond is indistinguishable
  # from one that is merely locked — and the difference decides what an operator
  # should do next. Empty on a VM that is off or has no integration services.
  heartbeat           = [string]$vm.Heartbeat
  cpuUsagePercent     = [int]$vm.CPUUsage
  uptimeSeconds       = [int64]$vm.Uptime.TotalSeconds
  guestOS             = [string]$os
  ipAddress           = [string]$ips
  guestFQDN           = [string]$fqdn
  checkpoints         = @($cps)
  processorCount      = [int]$vm.ProcessorCount
  memoryStartup       = [uint64]$vm.MemoryStartup
  dynamicMemory       = $dm
  memMin              = $mmin
  memMax              = $mmax
  generation          = [int]$vm.Generation
  disks               = @($disks)
  nics                = @($nics)
  integ               = @($integ)
  repl                = $repl
} | ConvertTo-Json -Compress -Depth 6
`, psQuote(name))

	out, err := p.run(ctx, script)
	if err != nil {
		return VMState{}, fmt.Errorf("get vm state %q: %w", name, err)
	}
	var obs vmObservation
	if err := decodeJSON(out, &obs); err != nil {
		return VMState{}, fmt.Errorf("get vm state %q: %w", name, err)
	}
	var cps []types.VMCheckpoint
	for _, c := range obs.Checkpoints {
		var t time.Time
		if c.CreatedAt != "" {
			if pt, perr := time.Parse(time.RFC3339, c.CreatedAt); perr == nil {
				t = pt
			}
		}
		cps = append(cps, types.VMCheckpoint{Name: c.Name, ParentName: c.ParentName, Type: c.Type, CreatedAt: t, IsCurrent: c.IsCurrent})
	}
	var observed *types.VMObserved
	if obs.Exists {
		o := &types.VMObserved{
			ProcessorCount:     obs.ProcessorCount,
			MemoryStartupBytes: obs.MemoryStartup,
			DynamicMemory:      obs.DynamicMemory,
			MinBytes:           obs.MemMin,
			MaxBytes:           obs.MemMax,
			Generation:         obs.Generation,
		}
		for _, d := range obs.Disks {
			o.Disks = append(o.Disks, types.VMDiskSpec{Path: d.Path, SizeBytes: d.SizeBytes})
		}
		for _, n := range obs.Nics {
			o.NetworkAdapters = append(o.NetworkAdapters, types.VMNetworkAdapterSpec{Name: n.Name, SwitchName: n.SwitchName, VLANID: n.VLANID})
		}
		for _, i := range obs.Integ {
			o.IntegrationServices = append(o.IntegrationServices, types.VMIntegrationServiceState{Name: i.Name, Enabled: i.Enabled})
		}
		observed = o
	}
	var repl *types.VMReplicationStatus
	if obs.Repl != nil {
		repl = &types.VMReplicationStatus{
			Mode:                obs.Repl.Mode,
			State:               obs.Repl.State,
			Health:              obs.Repl.Health,
			PrimaryServer:       obs.Repl.PrimaryServer,
			ReplicaServer:       obs.Repl.ReplicaServer,
			LastReplicationTime: obs.Repl.LastRepl,
			FrequencySeconds:    obs.Repl.FrequencySec,
		}
	}
	return VMState{
		Exists:              obs.Exists,
		ID:                  obs.ID,
		PowerState:          powerStateFromHyperV(obs.PowerState),
		AssignedMemoryBytes: obs.AssignedMemoryBytes,
		MemoryDemandBytes:   obs.MemoryDemandBytes,
		MemoryStatus:        obs.MemoryStatus,
		CPUUsagePercent:     obs.CPUUsagePercent,
		UptimeSeconds:       obs.UptimeSeconds,
		Heartbeat:           obs.Heartbeat,
		GuestOS:             obs.GuestOS,
		IPAddress:           obs.IPAddress,
		GuestFQDN:           obs.GuestFQDN,
		Checkpoints:         cps,
		Observed:            observed,
		Replication:         repl,
	}, nil
}

// observedVMListItem is one VM in the host-wide inventory (compact — no
// per-VM KVP/checkpoint reads, which would be too heavy across every VM).
type observedVMListItem struct {
	Name       string       `json:"name"`
	ID         string       `json:"id"`
	PowerState string       `json:"powerState"`
	Clustered  bool         `json:"clustered"`
	GuestOS    string       `json:"guestOS"`
	IPAddress  string       `json:"ipAddress"`
	Processor  int          `json:"processorCount"`
	MemStartup uint64       `json:"memoryStartup"`
	DynMem     bool         `json:"dynamicMemory"`
	MemMin     uint64       `json:"memMin"`
	MemMax     uint64       `json:"memMax"`
	Generation int          `json:"generation"`
	Disks      []vmDiskObs  `json:"disks"`
	Nics       []vmNicObs   `json:"nics"`
	Integ      []vmIntegObs `json:"integ"`
	Repl       *vmReplObs   `json:"repl"`
}

// ListObservedVMs enumerates every VM on the host. Best-effort per VM: a read
// error on one does not drop the rest.
func (p *PowerShell) ListObservedVMs(ctx context.Context) ([]types.ObservedVM, error) {
	const script = `
$ErrorActionPreference = 'Stop'
$out = @()
foreach ($vm in @(Get-VM -ErrorAction SilentlyContinue)) {
  try {
    $ips = ''
    try { $ips = (@($vm | Get-VMNetworkAdapter | Select-Object -ExpandProperty IPAddresses | Where-Object { $_ -and $_ -notlike 'fe80*' -and $_ -ne '127.0.0.1' }) -join ', ') } catch {}
    $disks = @()
    try { foreach ($d in @($vm | Get-VMHardDiskDrive -ErrorAction SilentlyContinue)) {
      $sz = 0; try { $sz = [uint64]((Get-VHD -Path $d.Path -ErrorAction Stop).Size) } catch {}
      $disks += [pscustomobject]@{ path = [string]$d.Path; sizeBytes = [uint64]$sz }
    } } catch {}
    $nics = @()
    try { foreach ($a in @($vm | Get-VMNetworkAdapter -ErrorAction SilentlyContinue)) {
      $vl = 0; try { $vv = Get-VMNetworkAdapterVlan -VMNetworkAdapter $a -ErrorAction SilentlyContinue; if ($vv -and $vv.OperationMode -eq 'Access') { $vl = [int]$vv.AccessVlanId } } catch {}
      $nics += [pscustomobject]@{ name = [string]$a.Name; switchName = [string]$a.SwitchName; vlanID = [int]$vl }
    } } catch {}
    $repl = $null
    try {
      $rr = Get-VMReplication -VM $vm -ErrorAction SilentlyContinue
      if ($rr) {
        $lt = ''; try { if ($rr.LastReplicationTime) { $lt = $rr.LastReplicationTime.ToUniversalTime().ToString('o') } } catch {}
        $repl = [pscustomobject]@{ mode=[string]$rr.ReplicationMode; state=[string]$rr.State; health=[string]$rr.Health; primaryServer=[string]$rr.PrimaryServer; replicaServer=[string]$rr.ReplicaServer; lastRepl=$lt; frequencySec=[int]$rr.FrequencySec }
      }
    } catch {}
    $clustered = $false; try { $clustered = [bool]$vm.IsClustered } catch {}
    $integ = @()
    try { foreach ($i in @(Get-VMIntegrationService -VMName $vm.Name -ErrorAction SilentlyContinue)) {
      $integ += [pscustomobject]@{ name = [string]$i.Name; enabled = [bool]$i.Enabled }
    } } catch {}
    $dm = $false; $mmin = [uint64]0; $mmax = [uint64]0
    try { $dm = [bool]$vm.DynamicMemoryEnabled; $mmin = [uint64]$vm.MemoryMinimum; $mmax = [uint64]$vm.MemoryMaximum } catch {}
    $out += [pscustomobject]@{
      name = [string]$vm.Name; id = [string]$vm.Id; powerState = [string]$vm.State; clustered = $clustered
      guestOS = ''; ipAddress = [string]$ips
      processorCount = [int]$vm.ProcessorCount; memoryStartup = [uint64]$vm.MemoryStartup
      dynamicMemory = $dm; memMin = $mmin; memMax = $mmax; generation = [int]$vm.Generation
      disks = @($disks); nics = @($nics); integ = @($integ); repl = $repl
    }
  } catch {}
}
ConvertTo-Json -InputObject @($out) -Compress -Depth 6
`
	out, err := p.run(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("list observed vms: %w", err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	// ConvertTo-Json emits a bare object (not an array) for a single VM.
	if trimmed[0] == '{' {
		trimmed = "[" + trimmed + "]"
	}
	var items []observedVMListItem
	if err := decodeJSON([]byte(trimmed), &items); err != nil {
		return nil, fmt.Errorf("list observed vms: %w", err)
	}
	result := make([]types.ObservedVM, 0, len(items))
	for _, it := range items {
		cfg := &types.VMObserved{
			ProcessorCount: it.Processor, MemoryStartupBytes: it.MemStartup,
			DynamicMemory: it.DynMem, MinBytes: it.MemMin, MaxBytes: it.MemMax, Generation: it.Generation,
		}
		for _, d := range it.Disks {
			cfg.Disks = append(cfg.Disks, types.VMDiskSpec{Path: d.Path, SizeBytes: d.SizeBytes})
		}
		for _, n := range it.Nics {
			cfg.NetworkAdapters = append(cfg.NetworkAdapters, types.VMNetworkAdapterSpec{Name: n.Name, SwitchName: n.SwitchName, VLANID: n.VLANID})
		}
		for _, i := range it.Integ {
			cfg.IntegrationServices = append(cfg.IntegrationServices, types.VMIntegrationServiceState{Name: i.Name, Enabled: i.Enabled})
		}
		ov := types.ObservedVM{
			Name: it.Name, VMID: it.ID, PowerState: powerStateFromHyperV(it.PowerState),
			Clustered: it.Clustered, GuestOS: it.GuestOS, IPAddress: it.IPAddress, Config: cfg,
		}
		if it.Repl != nil {
			ov.Replication = &types.VMReplicationStatus{
				Mode: it.Repl.Mode, State: it.Repl.State, Health: it.Repl.Health,
				PrimaryServer: it.Repl.PrimaryServer, ReplicaServer: it.Repl.ReplicaServer,
				LastReplicationTime: it.Repl.LastRepl, FrequencySeconds: it.Repl.FrequencySec,
			}
		}
		result = append(result, ov)
	}
	return result, nil
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
		Created         bool   `json:"created"`
		Changed         bool   `json:"changed"`
		PendingPowerOff bool   `json:"pendingPowerOff"`
		PendingDetail   string `json:"pendingDetail"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return VMEnsureResult{}, fmt.Errorf("ensure vm %q: %w", vm.Meta.Name, err)
	}
	r := VMEnsureResult{PendingPowerOff: res.PendingPowerOff, PendingDetail: res.PendingDetail}
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

// normaliseBootOrder maps declared boot-order tokens onto the canonical device
// categories the reconcile script understands ("Drive", "DVD", "Network", and
// "Floppy" for Gen 1 only), tolerating common aliases and casing, dropping
// unknown or gen-inapplicable entries, and de-duplicating while preserving order.
// Returns nil when nothing usable remains, which leaves the boot order unmanaged.
func normaliseBootOrder(order []string, gen int) []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range order {
		var t string
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "drive", "disk", "hdd", "harddrive", "harddisk", "ide", "scsi":
			t = "Drive"
		case "dvd", "cd", "optical", "iso":
			t = "DVD"
		case "network", "net", "pxe", "legacynetworkadapter", "networkadapter":
			t = "Network"
		case "floppy":
			if gen != 1 {
				continue // no floppy on Gen 2 UEFI
			}
			t = "Floppy"
		default:
			continue
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// The boot-order scripts are constants rather than inline literals so a test can
// execute the real thing against stubbed cmdlets. Both enforce ONLY the relative
// order of the device categories the operator declared, among the slots those
// devices already occupy; an entry in no declared category keeps its position.
//
// The earlier version sorted undeclared entries to the end and compared the whole
// sequence. That reported a difference where the declared intent was already
// satisfied. On Gen 2 it did so permanently: a UEFI boot list normally carries a
// file entry (the Windows Boot Manager) which has no .Device and so classifies as
// 'Other', and Windows writes it first on install and re-promotes it on boot.
// Declaring "DVD, Drive, Network" says nothing about where that entry belongs, so
// demoting it was the agent inventing intent — and because Windows put it back,
// the diff never cleared. Every such VM reported "boot order (want
// DVD,Drive,Network,Other, have Other,DVD,Drive,Network) needs the VM off" on
// every pass, for a boot order that already matched.
//
// %[1]s is the quoted VM name, %[2]s the quoted declared-category list.

// Gen 1 BIOS: StartupOrder is an ordered set of the four device categories, and
// Set-VMBios requires the complete set, so the rebuild starts from the current
// list and only permutes the declared devices within it.
const gen1BootOrderScript = `$want = @(%[2]s)
$map = @{ 'Drive'='IDE'; 'DVD'='CD'; 'Network'='LegacyNetworkAdapter'; 'Floppy'='Floppy' }
$decl = @()
foreach ($t in $want) { if ($map.ContainsKey($t)) { $d = $map[$t]; if ($decl -notcontains $d) { $decl += $d } } }
$bios = Get-VMBios -VMName %[1]s
$cur = @($bios.StartupOrder | ForEach-Object { [string]$_ })
$curDecl = @($cur | Where-Object { $decl -contains $_ })
if ($curDecl.Count -gt 1 -and ($curDecl -join ',') -ne ($decl -join ',')) {
  if ($running) { $pending = $true; $pendingWhat += ('boot order (want ' + ($decl -join ',') + ', have ' + ($curDecl -join ',') + ')') } else {
    $ord = @(); $k = 0
    foreach ($d in $cur) { if ($decl -contains $d) { $ord += $decl[$k]; $k++ } else { $ord += $d } }
    Set-VMBios -VMName %[1]s -StartupOrder $ord; $changed = $true
  }
}
`

// Gen 2 UEFI: entries are objects, classified by their backing device. Fewer than
// two declared entries means there is no relative order to enforce.
const gen2BootOrderScript = `$want = @(%[2]s)
$fw = Get-VMFirmware -VMName %[1]s
$entries = @($fw.BootOrder)
if ($entries.Count -gt 0) {
  $cls = {
    param($e)
    $n = ''
    try { $n = $e.Device.GetType().Name } catch {}
    if ($n -eq 'DvdDrive') { 'DVD' } elseif ($n -eq 'HardDiskDrive') { 'Drive' } elseif ($n -like '*NetworkAdapter*') { 'Network' } else { 'Other' }
  }
  $named = @($entries | Where-Object { $want -contains (& $cls $_) })
  if ($named.Count -gt 1) {
    $sorted = @()
    foreach ($t in $want) { foreach ($e in $named) { if ((& $cls $e) -eq $t) { $sorted += $e } } }
    $curSig = (($named | ForEach-Object { & $cls $_ }) -join ',')
    $wantSig = (($sorted | ForEach-Object { & $cls $_ }) -join ',')
    if ($curSig -ne $wantSig) {
      if ($running) { $pending = $true; $pendingWhat += ('boot order (want ' + $wantSig + ', have ' + $curSig + ')') } else {
        # Set-VMFirmware needs every entry, so fill each managed slot from $sorted
        # and leave undeclared entries where they are.
        $ordered = @(); $k = 0
        foreach ($e in $entries) {
          if ($want -contains (& $cls $e)) { $ordered += $sorted[$k]; $k++ } else { $ordered += $e }
        }
        Set-VMFirmware -VMName %[1]s -BootOrder $ordered; $changed = $true
      }
    }
  }
}
`

// indentBlock indents every non-empty line of a generated block, so a script
// fragment built for the top level reads correctly once it is nested inside an
// if. Cosmetic in PowerShell and not in a 200-line generated script somebody has
// to read when it goes wrong.
func indentBlock(block, pad string) string {
	if strings.TrimSpace(block) == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n") + "\n"
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
  if ($running) { $pending = $true; $pendingWhat += 'processor count' } else { Set-VMProcessor -VMName %[1]s -Count %[2]d; $changed = $true }
}`, name, s.ProcessorCount)
	}

	// Memory diff and apply: static unless dynamic memory is configured. A
	// startup of 0 with no dynamic-memory block means "do not manage memory"
	// (same adoption semantics as ProcessorCount above).
	/* Memory, split by what Hyper-V will actually accept on a RUNNING VM.

	   It used to be one test gated wholly on $running, so every memory change
	   waited for a power-off. That is stricter than the platform: with Dynamic
	   Memory already enabled, Minimum and Maximum can be changed live — it is
	   the whole point of dynamic memory — and only Startup, or turning dynamic
	   memory on or off, needs the VM stopped.

	   Deferring a live-capable change is not a safe conservatism. It leaves the
	   VM Progressing with a "needs the VM off" message that is untrue, and asks
	   an operator to schedule an outage to raise a ceiling Hyper-V would have
	   moved while the guest ran. */
	memScript := ""
	if s.DynamicMemory == nil && s.MemoryStartupBytes == 0 {
		// unmanaged memory: leave memScript empty
	} else if s.DynamicMemory != nil {
		// Needs the VM off: enabling dynamic memory at all, or moving Startup.
		needsOff := fmt.Sprintf("(-not $m.DynamicMemoryEnabled) -or ($m.Startup -ne %d)", s.MemoryStartupBytes)
		offApply := fmt.Sprintf("Set-VMMemory -VMName %[1]s -DynamicMemoryEnabled $true -StartupBytes %[2]d -MinimumBytes %[3]d -MaximumBytes %[4]d",
			name, s.MemoryStartupBytes, s.DynamicMemory.MinBytes, s.DynamicMemory.MaxBytes)
		// Applies live: the band, once dynamic memory is on and Startup agrees.
		liveDiff := fmt.Sprintf("($m.Minimum -ne %d) -or ($m.Maximum -ne %d)", s.DynamicMemory.MinBytes, s.DynamicMemory.MaxBytes)
		liveApply := fmt.Sprintf("Set-VMMemory -VMName %[1]s -MinimumBytes %[2]d -MaximumBytes %[3]d",
			name, s.DynamicMemory.MinBytes, s.DynamicMemory.MaxBytes)
		memScript = fmt.Sprintf(`$m = Get-VMMemory -VMName %[1]s
if (%[2]s) {
  # Startup, or dynamic memory itself: Hyper-V refuses these while running.
  if ($running) { $pending = $true; $pendingWhat += 'memory' } else { %[3]s; $changed = $true }
} elseif (%[4]s) {
  # Just the band. Dynamic memory is already on and Startup matches, so this
  # applies whether the VM is running or not — which is what dynamic memory is
  # for. Naming the pending reason 'memory' here would be false.
  %[5]s
  $changed = $true
}`, name, needsOff, offApply, liveDiff, liveApply)
	} else {
		memDiff := fmt.Sprintf("$m.DynamicMemoryEnabled -or ($m.Startup -ne %d)", s.MemoryStartupBytes)
		memApply := fmt.Sprintf("Set-VMMemory -VMName %[1]s -DynamicMemoryEnabled $false -StartupBytes %[2]d", name, s.MemoryStartupBytes)
		memScript = fmt.Sprintf(`$m = Get-VMMemory -VMName %[1]s
if (%[2]s) {
  # Static memory: the assignment is fixed at boot, so every change needs off.
  if ($running) { $pending = $true; $pendingWhat += 'memory' } else { %[3]s; $changed = $true }
}`, name, memDiff, memApply)
	}

	// Set-VM only when there is an automatic-start-action to apply; calling it
	// with just -Name is rejected.
	startAction := ""
	if arg := automaticStartActionArg(s.AutomaticStartAction); arg != "" {
		startAction = fmt.Sprintf("Set-VM -Name %s %s\n", name, arg)
	}

	// Secure Boot (Gen 2 only): converge the UEFI policy so a Linux guest can boot
	// (its shim is signed under the Microsoft UEFI CA, not the Windows template).
	// Applied only when it differs and only while the VM is off (a firmware change
	// requires it stopped), flagging $pending otherwise — same as CPU/memory.
	firmware := ""
	if gen == 2 {
		enable, tmpl := "On", "MicrosoftWindows"
		switch strings.ToLower(s.SecureBoot) {
		case "off":
			enable = "Off"
		case "linux", "uefi", "microsoftueficertificateauthority":
			tmpl = "MicrosoftUEFICertificateAuthority"
		}
		firmware = fmt.Sprintf(`$fw = Get-VMFirmware -VMName %[1]s
$wantSb = %[2]s
$wantTmpl = %[3]s
$isOn = ([string]$fw.SecureBoot -eq 'On')
if ((($wantSb -eq 'On') -ne $isOn) -or ($wantSb -eq 'On' -and [string]$fw.SecureBootTemplate -ne $wantTmpl)) {
  if ($running) { $pending = $true; $pendingWhat += ('Secure Boot (want ' + $wantSb + '/' + $wantTmpl + ', have ' + [string]$fw.SecureBoot + '/' + [string]$fw.SecureBootTemplate + ')') } else {
    if ($wantSb -eq 'On') { Set-VMFirmware -VMName %[1]s -EnableSecureBoot On -SecureBootTemplate $wantTmpl }
    else { Set-VMFirmware -VMName %[1]s -EnableSecureBoot Off }
    $changed = $true
  }
}
`, name, psQuote(enable), psQuote(tmpl))

		/* A software-emulated TPM 2.0 — what Windows 11 requires to install and
		   what BitLocker binds to.

		   Enable-VMTPM alone is not enough and this is the whole reason the
		   feature is worth having in a console. A vTPM is SEALED to key
		   protectors, and Hyper-V refuses to enable one on a VM that has none:
		   "A key protector cannot be found for the virtual machine." So a local
		   key protector is created first when the VM has no usable one. That is
		   the step every runbook on the subject spells out and no console does.

		   Set-VMKeyProtector -NewLocalKeyProtector is the local-guardian form,
		   which is the right one for a VM that lives on this host. A shielded VM
		   with an HGS guardian is a different arrangement entirely, and one
		   Ballast does not pretend to configure from a checkbox.

		   Read back rather than assumed: Get-VMSecurity reports TpmEnabled, so
		   the reconcile compares against what the VM HAS instead of tracking
		   whether it once ran the command. */
		tpmWant := "$false"
		if s.TPM {
			tpmWant = "$true"
		}
		firmware += fmt.Sprintf(`$sec = Get-VMSecurity -VMName %[1]s -ErrorAction SilentlyContinue
$tpmIs = [bool]($sec -and $sec.TpmEnabled)
$tpmWant = %[2]s
if ($tpmIs -ne $tpmWant) {
  if ($running) {
    # Hyper-V refuses to add or remove a TPM on a running VM, and Ballast does
    # not restart somebody's VM to satisfy a checkbox.
    $pending = $true
    $pendingWhat += ('a TPM (want ' + [string]$tpmWant + ', have ' + [string]$tpmIs + ')')
  } else {
    if ($tpmWant) {
      # The key protector FIRST, or Enable-VMTPM fails with "A key protector
      # cannot be found for the virtual machine".
      $hasKp = $false
      try {
        $kp = Get-VMKeyProtector -VMName %[1]s -ErrorAction Stop
        # An unconfigured VM returns a short placeholder rather than nothing, so
        # length is what distinguishes "has one" from "has the default".
        $hasKp = ($kp -and $kp.Length -gt 4)
      } catch {}
      if (-not $hasKp) {
        Set-VMKeyProtector -VMName %[1]s -NewLocalKeyProtector
        Write-Output ('NOTE created a local key protector for ' + %[1]s + '. The vTPM is sealed to it, so this VM cannot simply be exported and imported on another host — that host cannot unseal it. Moving it needs the key protector carried across, or the guest BitLocker recovery key to hand.')
      }
      Enable-VMTPM -VMName %[1]s
    } else {
      Disable-VMTPM -VMName %[1]s
    }
    $changed = $true
  }
}
`, name, tpmWant)
	} else if s.TPM {
		/* Refused on Generation 1 rather than silently ignored.

		   A Gen 1 VM has no UEFI firmware to present a TPM, so the setting can
		   never take. A checkbox that stays ticked and does nothing is how
		   somebody spends an afternoon on a Windows 11 installer that will not
		   proceed, blaming the installer. */
		firmware = fmt.Sprintf(`throw ('%%s is Generation 1, which has no UEFI firmware and therefore cannot have a TPM. Windows 11 needs one, so a Generation 2 VM is required — generation is fixed when the VM is created and cannot be changed afterwards.' -f %[1]s)
`, name)
	}

	/* Console resolution: drive the synthetic video adapter to the declared size.

	   A basic console session shows exactly what the guest's video adapter is
	   driving, so a bigger browser window scales 1024x768 up rather than showing
	   more of anything. Set-VMVideo is the only thing that changes it, and it
	   needs the VM stopped — so, like Secure Boot, it flags $pending while
	   running and settles on the next power-off.

	   Guarded on the cmdlet existing: Set-VMVideo is absent on some Hyper-V
	   builds, and a missing cmdlet must not fail a whole reconcile over a console
	   convenience while power, sizing and networking wait behind it. */
	video := ""
	if w, h, ok := parseResolution(s.VideoResolution); ok {
		video = fmt.Sprintf(`if (Get-Command Set-VMVideo -ErrorAction SilentlyContinue) {
  $vid = Get-VMVideo -VMName %[1]s -ErrorAction SilentlyContinue
  if ($vid -and (($vid.ResolutionType -ne 'Single') -or ($vid.HorizontalResolution -ne %[2]d) -or ($vid.VerticalResolution -ne %[3]d))) {
    if ($running) { $pending = $true; $pendingWhat += ('console resolution (want %[2]dx%[3]d, have ' + [string]$vid.HorizontalResolution + 'x' + [string]$vid.VerticalResolution + ')') } else {
      Set-VMVideo -VMName %[1]s -ResolutionType Single -HorizontalResolution %[2]d -VerticalResolution %[3]d
      $changed = $true
    }
  }
}
`, name, w, h)
	}

	/* Nested virtualisation: expose the host's virtualisation extensions so the
	   guest can run Hyper-V itself.

	   Set at power-on, so like the processor count and Secure Boot this flags
	   $pending while the VM runs and settles on the next power-off. Ballast does
	   not restart somebody's VM to satisfy a checkbox.

	   Driven in BOTH directions rather than only on: a spec with the box cleared
	   has to take the extensions away again, or unticking it in the console
	   would report settled and change nothing. The MAC spoofing half of this is
	   applied per adapter below, and applies live.

	   Guarded on the property existing: ExposeVirtualizationExtensions is absent
	   on Hyper-V before 2016, and a missing property must not fail a whole
	   reconcile — power and networking are queued behind it. */
	nested := fmt.Sprintf(`$vp = Get-VMProcessor -VMName %[1]s
if ($null -ne $vp.ExposeVirtualizationExtensions) {
  if ([bool]$vp.ExposeVirtualizationExtensions -ne $%[2]t) {
    if ($running) { $pending = $true; $pendingWhat += 'nested virtualisation' } else {
      Set-VMProcessor -VMName %[1]s -ExposeVirtualizationExtensions $%[2]t
      $changed = $true
    }
  }
} elseif ($%[2]t) {
  Write-Warning 'this host does not support nested virtualisation (no ExposeVirtualizationExtensions); the setting was not applied'
}
`, name, s.NestedVirtualisation)

	// Boot order: order the VM's actual boot entries by the declared device-category
	// priority. Runs after disks/adapters/ISO are attached (below) so the entries it
	// reorders exist. Like Secure Boot, a firmware/BIOS change needs the VM off, so
	// it flags $pending while running and settles on the next power-off. Empty spec
	// leaves the order unmanaged.
	bootOrder := ""
	if toks := normaliseBootOrder(s.BootOrder, gen); len(toks) > 0 {
		want := psStringList(toks)
		if gen == 1 {
			bootOrder = fmt.Sprintf(gen1BootOrderScript, name, want)
		} else {
			bootOrder = fmt.Sprintf(gen2BootOrderScript, name, want)
		}
	}

	/* Every path this VM declares, so a disk attached from somewhere else can be
	   told from a disk that is simply also declared.

	   Without it, a VM legitimately declaring two disks of the same file name in
	   different folders would read as one disk that had moved. Normalised by
	   PowerShell rather than in Go, because Windows decides what two paths being
	   the same means. */
	declared := ""
	if len(s.Disks) > 0 {
		quoted := make([]string, 0, len(s.Disks))
		for _, d := range s.Disks {
			quoted = append(quoted, psQuote(d.Path))
		}
		declared = "$declaredDisks = @()\nforeach ($dp in @(" + strings.Join(quoted, ", ") +
			")) { $declaredDisks += [IO.Path]::GetFullPath($dp) }\n"
	}

	disks := declared
	for _, d := range s.Disks {
		path := psQuote(d.Path)
		create := ""
		if d.SizeBytes > 0 {
			sizeFlag := fmt.Sprintf("-Fixed -SizeBytes %d", d.SizeBytes)
			if d.Dynamic {
				sizeFlag = fmt.Sprintf("-Dynamic -SizeBytes %d", d.SizeBytes)
			}
			// Test-Path is false for BOTH "the file is not there" and "I cannot read
			// its volume" — a CSV that is offline, mid-rebuild, or owned by another
			// node reads exactly like a missing disk. Those are not the same thing,
			// and acting on the second reading would put a blank VHDX where the real
			// one lives. Only create when the containing directory is demonstrably
			// reachable; otherwise fail with the actual reason instead of New-VHD's
			// bare "Failed to create the virtual hard disk".
			create = fmt.Sprintf(`if (-not (Test-Path %[1]s)) {
  $dir = Split-Path %[1]s -Parent
  if ($dir -and -not (Test-Path -LiteralPath $dir)) {
    try { New-Item -ItemType Directory -Path $dir -Force -ErrorAction Stop | Out-Null }
    catch { throw ('refusing to create ' + %[1]s + ': its directory cannot be read or created from this host (' + $_.Exception.Message + ') - the volume may be offline, still rebuilding, or owned by another node. Not creating a new disk over a path that cannot be read') }
  }
  New-VHD -Path %[1]s %[2]s | Out-Null; $changed = $true
}
`, path, sizeFlag)
		}
		/* Consider the desired disk present if it is the attached file OR an
		   ancestor of an attached differencing disk — when the VM has a
		   checkpoint it runs off an .avhdx whose parent chain leads back to this
		   VHDX, so a naive exact-path match would wrongly try to re-attach the
		   (locked) base. Paths are normalised so forward/backslash and casing
		   differences still match.

		   A MISSING DISK AND A MOVED DISK ARE NOT THE SAME THING, and the check
		   above cannot tell them apart on its own: after a storage migration the
		   same file is attached from a new location, so the declared path matches
		   nothing and this went off to attach a second copy from a folder that no
		   longer holds one. Seen on BallastJumphost, moved from I: to D:: the VM
		   was running perfectly with its disk attached the whole time, and Ballast
		   marked it degraded and handed the operator Add-VMHardDiskDrive's own
		   wording about a file it could not find.

		   The window is real even when everything works — the centre rewrites the
		   declared paths when the move job reports success, and the agent enforces
		   the old ones until it pulls that generation. So the same file name
		   attached from somewhere else is recognised for what it is: the storage
		   moved and the declaration has not caught up. Nothing is attached and
		   nothing is created; it is said once, as a warning, and the next pull
		   settles it.

		   It also guards the worse case, which is silent. A declared disk WITH a
		   size would have had New-VHD create a blank VHDX at the old path and
		   attach that — no error, VM boots, empty disk, real data left behind
		   where it was moved from. The create only runs now when the disk is
		   neither present nor found elsewhere. */
		disks += fmt.Sprintf(`$want = [IO.Path]::GetFullPath(%[2]s)
$leaf = [IO.Path]::GetFileName($want)
$present = $false
$movedTo = ''
foreach ($d in (Get-VMHardDiskDrive -VMName %[1]s)) {
  $p = $d.Path
  while ($p) {
    $full = [IO.Path]::GetFullPath($p)
    if ($full -ieq $want) { $present = $true; break }
    if (-not $movedTo -and ([IO.Path]::GetFileName($full) -ieq $leaf) -and ($declaredDisks -notcontains $full)) { $movedTo = $full }
    $vhd = Get-VHD -Path $p -ErrorAction SilentlyContinue
    if ($vhd) { $p = $vhd.ParentPath } else { $p = $null }
  }
  if ($present) { break }
}
if (-not $present) {
  if ($movedTo) {
    Write-Warning ('the disk declared at ' + %[2]s + ' is attached from ' + $movedTo + ' instead, so this VM' + [char]39 + 's storage has been moved. Nothing was attached or created - the declared path is what needs updating, and a move made from Ballast updates it within a pass or two')
  } else {
%[3]s    try { Add-VMHardDiskDrive -VMName %[1]s -Path %[2]s; $changed = $true }
    catch {
      # During a live migration the VHDX is held open by the running VM, so a node
      # reconciling mid-migration can transiently see the disk as unattached and
      # fail to (re)attach it with "being used by another process". That is expected
      # and settles once migration completes — treat it as transient, not a failure.
      if ($_.Exception.Message -notlike '*another process*' -and $_.Exception.Message -notlike '*being used*') { throw }
    }
  }
}
`, name, path, indentBlock(create, "    "))
	}

	adapters := ""
	for _, a := range s.NetworkAdapters {
		an := psQuote(a.Name)
		sw := psQuote(a.SwitchName)
		// A missing switch must NOT abort the whole VM reconcile: if it did, an
		// unrelated networking detail (e.g. a NIC still pointing at a switch that
		// does not exist on this host after a failover) would wedge everything
		// else — power, sizing, and crucially replication teardown — leaving the
		// VM Degraded and its relationship stuck. So tolerate "unable to find a
		// virtual switch": leave the NIC disconnected and warn, and let the rest
		// of the reconcile proceed. It reconnects on a later pass once the switch
		// exists (or the desired switch is corrected).
		adapters += fmt.Sprintf(`$ad = Get-VMNetworkAdapter -VMName %[1]s | Where-Object { $_.Name -eq %[2]s }
if (-not $ad) {
  try { Add-VMNetworkAdapter -VMName %[1]s -Name %[2]s -SwitchName %[3]s -ErrorAction Stop; $changed = $true }
  catch {
    if ($_.Exception.Message -like '*unable to find a virtual switch*') { Add-VMNetworkAdapter -VMName %[1]s -Name %[2]s -ErrorAction Stop; $changed = $true; Write-Warning ('switch ' + %[3]s + ' not found on this host; ' + %[2]s + ' left disconnected') }
    else { throw }
  }
} elseif ($ad.SwitchName -ne %[3]s) {
  try { Connect-VMNetworkAdapter -VMName %[1]s -Name %[2]s -SwitchName %[3]s -ErrorAction Stop; $changed = $true }
  catch {
    if ($_.Exception.Message -like '*unable to find a virtual switch*') { Write-Warning ('switch ' + %[3]s + ' not found on this host; ' + %[2]s + ' left disconnected') }
    else { throw }
  }
}
`, name, an, sw)
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
		/* MAC address spoofing follows nested virtualisation, on every adapter.

		   A nested guest's inner VMs have MAC addresses the outer switch has
		   never seen, and without spoofing the switch drops their traffic — so
		   the guest boots, runs VMs, and none of them can reach anything. That is
		   why this is not a separate checkbox: the state "nested on, spoofing
		   off" is worse than either setting alone, and it looks like a networking
		   fault rather than a missing tick.

		   Unlike the extensions this applies to a running VM, and it is driven in
		   both directions so clearing the box takes it off again. */
		spoof := "Off"
		if s.NestedVirtualisation {
			spoof = "On"
		}
		/* MAC spoofing is a PORT feature, so it needs a port.

		   An adapter that is not connected to a switch has none, and Hyper-V
		   refuses with a sentence naming neither the adapter nor the reason:

		     Set-VMNetworkAdapter : Modifying features of the Ethernet connection
		     failed. The operation cannot be performed while the object is in its
		     current state.

		   That failed the whole VM reconcile, every pass, on HVNew01 after a copy
		   landed it with its adapters disconnected: a VM otherwise exactly as
		   declared, held Degraded by a setting that cannot apply yet and will apply
		   itself the moment the adapter is connected.

		   So it is skipped while disconnected and SAID, which is the difference
		   between a fault and a fact. */
		adapters += fmt.Sprintf(`$ad = @(Get-VMNetworkAdapter -VMName %[1]s -Name %[2]s -ErrorAction SilentlyContinue)[0]
if ($ad -and $ad.SwitchName) {
  Set-VMNetworkAdapter -VMNetworkAdapter $ad -MacAddressSpoofing %[3]s
} elseif ($ad) {
  Write-Output ('NOTE ' + %[2]s + ' on ' + %[1]s + ' is not connected to a switch, so MAC address spoofing cannot be set yet. ' +
    'It is a port setting and there is no port until the adapter is connected; Ballast applies it on the pass after that.')
}
`, name, an, psQuote(spoof))
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

	/* THE SAME VM UNDER ANOTHER NAME.

	   Everything above matches by NAME, so a VM that has been renamed — by the
	   rename job, in the window before the centre re-keys desired state, or by
	   somebody in Hyper-V Manager — looks exactly like a VM that does not exist.
	   The next line would then be New-VM: a second, empty machine wearing the old
	   name, pointed at the SAME VHDX files. The attach fails with "being used by
	   another process", which this script tolerates as transient, so the result is
	   a phantom VM and no error anywhere.

	   The Hyper-V GUID does not change when a VM is renamed, and the centre
	   already carries it — VMStatus.VMID, reported by the agent and used as the
	   console's preconnection blob. So when it is known, look for it before
	   creating anything.

	   Not an error: during a rename this is the correct, expected state and it
	   settles when the centre re-keys. Said once as a warning, and nothing is
	   created. */
	renameGuard := ""
	if id := strings.TrimSpace(vm.Status.VMID); id != "" {
		renameGuard = fmt.Sprintf(`  try {
    $byId = @(Get-VM -ErrorAction SilentlyContinue | Where-Object { [string]$_.Id -eq %[1]s })
    if ($byId.Count -gt 0) {
      $ownedElsewhere = $true
      Write-Warning ('this VM exists on this host as ' + [string]$byId[0].Name + ', not ' + %[2]s +
        ' - it has been renamed, and nothing was created. The declared name catches up when the centre records the rename')
    }
  } catch {}
`, psQuote(id), name)
	}

	// Place the VM's files in a per-VM folder on the chosen datastore — the first
	// disk's directory (e.g. C:\ClusterStorage\vol1\<VM>) — and point New-VM's
	// config there with -Path. This keeps a cluster VM's config on shared storage
	// (migration-ready) and, crucially, does not depend on the host's default VM
	// path, which may be unset or point at a folder that does not exist (New-VM
	// then fails 0x80070002 "cannot access configuration store"). The folder is
	// created first so both New-VM and New-VHD have somewhere to write.
	mkVMDir, vmPathArg := "", ""
	if len(s.Disks) > 0 && s.Disks[0].Path != "" {
		mkVMDir = fmt.Sprintf("  $vmDir = Split-Path %s -Parent\n  if ($vmDir) { New-Item -ItemType Directory -Path $vmDir -Force | Out-Null }\n", psQuote(s.Disks[0].Path))
		vmPathArg = " -Path $vmDir"
	}

	return fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$created = $false
$changed = $false
$pending = $false
$pendingWhat = @()
# Look the VM up, distinguishing "this host does not have it" from "this host's
# Hyper-V cannot answer". A plain -ErrorAction SilentlyContinue conflates them:
# when VMMS is inconsistent (one bad registration makes Get-VM throw "Hyper-V
# encountered an error trying to access an object"), $vm came back null, the
# cluster-group check below then concluded the VM was simply owned elsewhere,
# and the whole reconcile returned "unchanged" — reporting every VM on a blind
# host as "already matches desired state". A host that cannot enumerate its VMs
# must fail loudly and go Degraded, never report green.
$vm = $null
try { $vm = Get-VM -Name %[1]s -ErrorAction Stop }
catch {
  $msg = [string]$_.Exception.Message
  if ($msg -notlike '*unable to find*' -and $msg -notlike '*not find a virtual machine*') {
    throw ('cannot enumerate VMs on this host (Hyper-V is not answering): ' + $msg)
  }
}
if (-not $vm) {
  $ownedElsewhere = $false
%[17]s%[10]s  if ($ownedElsewhere) {
    [pscustomobject]@{ created = $false; changed = $false; pendingPowerOff = $false } | ConvertTo-Json -Compress
    return
  }
%[11]s  New-VM -Name %[1]s -Generation %[2]d -MemoryStartupBytes %[3]d -NoVHD%[12]s | Out-Null
  $created = $true
}
$running = $false
# By here the VM exists (found above, or just created), so a failure to read it
# back is a host fault, not an absent VM. Let it propagate: guessing "not
# running" would apply firmware/CPU/memory changes to a live VM.
$cur = Get-VM -Name %[1]s -ErrorAction Stop
if ($cur -and $cur.State -ne 'Off') { $running = $true }
%[4]s
%[5]s
%[13]s%[15]s%[16]s%[6]s%[7]s%[8]s%[9]s%[14]s
[pscustomobject]@{ created = $created; changed = $changed; pendingPowerOff = $pending; pendingDetail = ($pendingWhat -join '; ') } | ConvertTo-Json -Compress
`, name, gen, s.MemoryStartupBytes, procScript, memScript, startAction, disks, adapters, iso, clusterGuard, mkVMDir, vmPathArg, firmware, bootOrder, video, nested, renameGuard)
}

/*
parseResolution reads "1920x1080" into its two numbers.

	Refused rather than guessed. A malformed value here would otherwise reach
	Set-VMVideo as a zero and leave the console at 0x0 — a VM whose screen never
	comes back, for a typo in a field nobody would think to look at.
*/
func parseResolution(v string) (w, h int, ok bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(v)), "x")
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || w < 640 || w > 7680 {
		return 0, 0, false
	}
	h, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || h < 480 || h > 4320 {
		return 0, 0, false
	}
	return w, h, true
}

// SetVMPowerState drives the VM to Running (Start-VM) or Off (Stop-VM). It reads
// current state first so a no-op returns OutcomeUnchanged.
// vmPowerScript builds the power script. Split out from SetVMPowerState so the
// ordering it encodes can be tested without a host, the same way ensureVMScript
// is.
func (p *PowerShell) vmPowerScript(name string, desired types.VMPowerState) string {
	var verb string
	switch desired {
	case types.VMPowerRunning:
		verb = fmt.Sprintf("Start-VM -Name %s", psQuote(name))
	case types.VMPowerOff:
		verb = fmt.Sprintf("Stop-VM -Name %s -Force", psQuote(name))
	default:
		// Paused/Saved are observed, never requested; the caller treats an empty
		// script as a no-op.
		return ""
	}

	target := "Running"
	if desired == types.VMPowerOff {
		target = "Off"
	}
	// A clustered VM is powered via its cluster group from any member — the
	// cluster service routes it to the current owner, so we do not need to know or
	// reach the owner node. A standalone VM is powered locally. Detect which by
	// whether a cluster group of that name exists.
	clusterVerb := "Start-ClusterGroup"
	if desired == types.VMPowerOff {
		clusterVerb = "Stop-ClusterGroup"
	}
	return fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
# Guarded on the CMDLET existing, not on the call failing quietly.
#
# -ErrorAction SilentlyContinue does not suppress a CommandNotFoundException:
# the failure happens at command resolution, before any parameter is bound, and
# under $ErrorActionPreference='Stop' it terminates the script. A standalone
# host with no Failover Clustering feature has no Get-ClusterGroup at all, so
# powering a VM on one died with "The term 'Get-ClusterGroup' is not recognized"
# — a message about a missing cmdlet, on a host that was never meant to have it.
#
# Seen on HVNEW06 the moment a VM was evacuated onto it: the destination of an
# evacuation is exactly the host least likely to be a cluster member.
$grp = $null
if (Get-Command Get-ClusterGroup -ErrorAction SilentlyContinue) {
  $grp = Get-ClusterGroup -Name %[1]s -ErrorAction SilentlyContinue
}
if ($grp) {
  if ([string]$grp.State -eq '%[4]s') { [pscustomobject]@{ changed = $false } | ConvertTo-Json -Compress; return }
} else {
  $vm = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
  if (-not $vm) { throw 'VM does not exist' }
  if ([string]$vm.State -eq '%[2]s') { [pscustomobject]@{ changed = $false } | ConvertTo-Json -Compress; return }
}
try {
  if ($grp) {
    # Shut the GUEST down first when stopping a clustered VM.
    #
    # Stop-ClusterGroup obeys the Virtual Machine resource's OfflineAction, which
    # Windows defaults to Save. So "stop" on a clustered VM saved its memory image
    # while the identical button on a standalone VM shut the guest down cleanly —
    # one action with two meanings, and the surprising one is the default.
    #
    # It is not merely inconsistent. A saved image cannot be restored on a node
    # with a different CPU feature set, which is precisely the failure the catch
    # block below exists to diagnose (Hyper-V event 24000) and whose only remedy
    # is destructive. Saving on every stop manufactures that condition across a
    # mixed cluster.
    #
    # With the guest already Off, taking the group offline has nothing to save.
    if ('%[2]s' -eq 'Off') {
      $lv = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
      if ($lv -and [string]$lv.State -eq 'Running') {
        # Best effort on purpose. A guest with no integration services cannot be
        # asked to shut down, and refusing to stop it would be worse than the
        # save this is avoiding — so a failure here falls through to the old
        # behaviour rather than leaving the operator unable to stop a VM.
        try {
          Stop-VM -Name %[1]s -Force -ErrorAction Stop | Out-Null
        } catch {}
        $deadline = (Get-Date).AddSeconds(120)
        while ((Get-Date) -lt $deadline) {
          $cur = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
          if (-not $cur -or [string]$cur.State -eq 'Off') { break }
          Start-Sleep -Seconds 2
        }
      }
    }
    %[5]s -Name %[1]s -ErrorAction Stop | Out-Null
  }
  else { %[3]s | Out-Null }
} catch {
  # A start that fails against a SAVED state is worth diagnosing rather than
  # handing the raw cmdlet text to an operator. Hyper-V logs event 24000 when
  # the memory image was captured on a machine with a different CPU feature
  # set: it cannot be restored here, and no amount of retrying changes that.
  # The only way out is to discard the image and cold boot, which is
  # destructive — so the agent identifies the cause and NEVER acts on it.
  $why = [string]$_.Exception.Message
  # 0x800704F7 is ERROR_MACHINE_LOCKED: a user session on the guest is LOCKED, and
  # Windows refuses a clean shutdown while it is. That is a known failure with a
  # known remedy, so it is named rather than passed through as a hex code — and the
  # remedy is spelled out because forcing past a lock closes somebody's session.
  if ($why -like '*0x800704F7*' -or $why -like '*locked and cannot be shut down*') {
    throw ('a user session on ' + %[1]s + ' is LOCKED, so Windows refused a clean shutdown. The guest is healthy - this is not a fault. Sign in and shut it down, or force it, which closes that session and loses anything unsaved in it.')
  }
  $now = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
  if ('%[2]s' -eq 'Running' -and $now -and [string]$now.State -eq 'Saved') {
    $incompat = $false
    # The event log is the authoritative statement; the message text is only a
    # fallback for when the log is unreadable, because its wording is not a
    # contract.
    foreach ($e in @(Get-WinEvent -FilterHashtable @{ LogName='Microsoft-Windows-Hyper-V-Worker-Admin'; Id=24000; StartTime=(Get-Date).AddMinutes(-10) } -ErrorAction SilentlyContinue)) {
      if ([string]$e.Message -like ('*' + %[1]s + '*')) { $incompat = $true }
    }
    if (-not $incompat -and $why -match 'failed to restore') { $incompat = $true }
    if ($incompat) {
      [pscustomobject]@{ changed = $false; savedStateIncompatible = $true } | ConvertTo-Json -Compress
      return
    }
  }
  throw
}
[pscustomobject]@{ changed = $true } | ConvertTo-Json -Compress
`, psQuote(name), target, verb, clusterGroupState(desired), clusterVerb)
}

func (p *PowerShell) SetVMPowerState(ctx context.Context, name string, desired types.VMPowerState) (Outcome, error) {
	script := p.vmPowerScript(name, desired)
	if script == "" {
		// Paused/Saved are observed, never requested; treat as no-op.
		return OutcomeUnchanged, nil
	}

	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("set vm power %q: %w", name, err)
	}
	var res struct {
		Changed                bool `json:"changed"`
		SavedStateIncompatible bool `json:"savedStateIncompatible"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return OutcomeUnchanged, fmt.Errorf("set vm power %q: %w", name, err)
	}
	if res.SavedStateIncompatible {
		// Prose, because this reaches the operator as a job message. The sentinel
		// is what the reconciler matches on to set a machine-readable reason.
		return OutcomeUnchanged, fmt.Errorf("%w: %q has a saved state captured on a different host, so it cannot be resumed here. Discarding the saved state will cold-boot it — the disks are unaffected, but anything in memory is lost", ErrSavedStateIncompatible, name)
	}
	if res.Changed {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
}

// ErrSavedStateIncompatible reports a VM that cannot start because its saved
// memory image was captured on a host with a different CPU feature set.
//
// It is deliberately its own error rather than one more opaque failure string.
// It is not transient — retrying will never fix it — and it has exactly one
// remedy, which is destructive, so it must be recognisable enough for the
// console to name the cause and offer the action instead of showing a cmdlet
// error and leaving the operator to research it. See
// docs/console-completeness-2026-08-05.md.
var ErrSavedStateIncompatible = errors.New("saved state is not compatible with this host")

// clusterGroupState maps a requested VM power state to the cluster group state
// used to skip a no-op.
func clusterGroupState(desired types.VMPowerState) string {
	if desired == types.VMPowerOff {
		return "Offline"
	}
	return "Online"
}

// RestartVM restarts a running VM (a one-shot imperative action, not a desired
// state — power is never continuously enforced). For a clustered VM it targets
// the current owner node (resolved from the cluster group) so the restart runs
// where the VM actually is; for a standalone VM it restarts locally. Restart-VM
// requests a guest OS restart; -Force skips the confirmation prompt.
func (p *PowerShell) RestartVM(ctx context.Context, name string) error {
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
# Guarded on the CMDLET existing, not on the call failing quietly.
#
# -ErrorAction SilentlyContinue does not suppress a CommandNotFoundException:
# the failure happens at command resolution, before any parameter is bound, and
# under $ErrorActionPreference='Stop' it terminates the script. A standalone
# host with no Failover Clustering feature has no Get-ClusterGroup at all, so
# powering a VM on one died with "The term 'Get-ClusterGroup' is not recognized"
# — a message about a missing cmdlet, on a host that was never meant to have it.
#
# Seen on HVNEW06 the moment a VM was evacuated onto it: the destination of an
# evacuation is exactly the host least likely to be a cluster member.
$grp = $null
if (Get-Command Get-ClusterGroup -ErrorAction SilentlyContinue) {
  $grp = Get-ClusterGroup -Name %[1]s -ErrorAction SilentlyContinue
}
if ($grp) {
  if ([string]$grp.State -ne 'Online') { throw ('cluster role is ' + $grp.State + '; only a running VM can be restarted') }
  Restart-VM -Name %[1]s -ComputerName $grp.OwnerNode.Name -Force -ErrorAction Stop | Out-Null
  return
}
$vm = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
if (-not $vm) { throw 'VM does not exist' }
if ([string]$vm.State -ne 'Running') { throw ('VM is ' + $vm.State + '; only a running VM can be restarted') }
Restart-VM -Name %[1]s -Force -ErrorAction Stop | Out-Null`, psQuote(name))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("restart vm %q: %w", name, err)
	}
	return nil
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
if (-not $vm) { return '!no-vm' }
if ($vm.EnabledState -ne 2) { return ('!not-running:' + $vm.EnabledState) }  # 2 = Enabled (running)
$vmms = Get-CimInstance -Namespace $ns -ClassName Msvm_VirtualSystemManagementService
$sd = Get-CimAssociatedInstance -InputObject $vm -ResultClassName Msvm_VirtualSystemSettingData -Association Msvm_SettingsDefineState
$res = Invoke-CimMethod -InputObject $vmms -MethodName GetVirtualSystemThumbnailImage -Arguments @{ TargetSystem = $sd; WidthPixels = [uint16]%[2]d; HeightPixels = [uint16]%[3]d }
if ($res.ReturnValue -ne 0) { return ('!thumbnail-failed:' + $res.ReturnValue) }
if (-not $res.ImageData) { return '!no-image-data' }
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
	// A capture that produced nothing used to return (nil, nil), which the caller
	// could not tell from success — so it assigned an empty screen and logged
	// nothing at all. The console then showed a VM it knew was RUNNING with no
	// preview and no reason, at either end. Each case now names itself.
	//
	// "!" cannot begin base64, so a marker can never be mistaken for an image.
	if strings.HasPrefix(b64, "!") {
		return nil, fmt.Errorf("get vm screen %q: %s", name, strings.TrimPrefix(b64, "!"))
	}
	if b64 == "" {
		return nil, fmt.Errorf("get vm screen %q: the thumbnail script returned nothing", name)
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
