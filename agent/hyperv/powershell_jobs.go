package hyperv

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// PowerShell backing for imperative Jobs. These run locally on the host (the
// agent executes them), so cluster cmdlets have no WinRM double-hop. Single
// quotes only, per the -Command quoting constraint.

func (p *PowerShell) CreateVMCheckpoint(ctx context.Context, vmName, checkpointName string) error {
	name := ""
	if checkpointName != "" {
		name = fmt.Sprintf(" -SnapshotName %s", psQuote(checkpointName))
	}
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; Checkpoint-VM -Name %s%s | Out-Null", psQuote(vmName), name)
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("checkpoint vm %q: %w", vmName, err)
	}
	return nil
}

func (p *PowerShell) ExportVM(ctx context.Context, vmName, path string) error {
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; if (-not (Test-Path %[2]s)) { New-Item -ItemType Directory -Path %[2]s -Force | Out-Null }; Export-VM -Name %[1]s -Path %[2]s", psQuote(vmName), psQuote(path))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("export vm %q to %q: %w", vmName, path, err)
	}
	return nil
}

func (p *PowerShell) ApplyVMCheckpoint(ctx context.Context, vmName, checkpointName string) error {
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; Restore-VMCheckpoint -VMName %s -Name %s -Confirm:$false | Out-Null", psQuote(vmName), psQuote(checkpointName))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("apply checkpoint %q on %q: %w", checkpointName, vmName, err)
	}
	return nil
}

func (p *PowerShell) RemoveVMCheckpoint(ctx context.Context, vmName, checkpointName string) error {
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; Remove-VMCheckpoint -VMName %s -Name %s -Confirm:$false | Out-Null", psQuote(vmName), psQuote(checkpointName))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("remove checkpoint %q on %q: %w", checkpointName, vmName, err)
	}
	return nil
}

func (p *PowerShell) AddClusterNode(ctx context.Context, node string) error {
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; Import-Module FailoverClusters; Add-ClusterNode -Name %s | Out-Null", psQuote(node))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("add cluster node %q: %w", node, err)
	}
	return nil
}

func (p *PowerShell) EvictClusterNode(ctx context.Context, node string) error {
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; Import-Module FailoverClusters; Remove-ClusterNode -Name %s -Force | Out-Null", psQuote(node))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("evict cluster node %q: %w", node, err)
	}
	return nil
}

func (p *PowerShell) DrainNode(ctx context.Context, node string) error {
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; Import-Module FailoverClusters; Suspend-ClusterNode -Name %s -Drain | Out-Null", psQuote(node))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("drain node %q: %w", node, err)
	}
	return nil
}

func (p *PowerShell) ResumeNode(ctx context.Context, node string) error {
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; Import-Module FailoverClusters; Resume-ClusterNode -Name %s | Out-Null", psQuote(node))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("resume node %q: %w", node, err)
	}
	return nil
}

// RemoveSwitch deletes a virtual switch from the host. Idempotent: absent switch
// is a no-op. -Force suppresses the confirmation prompt.
func (p *PowerShell) RemoveSwitch(ctx context.Context, name string) error {
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; if (Get-VMSwitch -Name %s -ErrorAction SilentlyContinue) { Remove-VMSwitch -Name %s -Force }", psQuote(name), psQuote(name))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("remove switch %q: %w", name, err)
	}
	return nil
}

// RemoveVM stops (if running) and deletes a VM from the host. If the VM is a
// clustered (highly-available) role, its cluster group is removed first so the
// delete does not leave an orphaned role behind; this makes the job work for a
// clustered VM regardless of which member it ran on. The VHDX files are left on
// disk. Idempotent: absent VM/role is a no-op.
func (p *PowerShell) RemoveVM(ctx context.Context, name string) error {
	q := psQuote(name)
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
$g = Get-ClusterGroup -Name %[1]s -ErrorAction SilentlyContinue
if ($g) {
  Stop-ClusterGroup -Name %[1]s -ErrorAction SilentlyContinue | Out-Null
  Remove-ClusterGroup -Name %[1]s -RemoveResources -Force -ErrorAction SilentlyContinue
}
$vm = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
if ($vm) { if ($vm.State -ne 'Off') { Stop-VM -Name %[1]s -TurnOff -Force }; Remove-VM -Name %[1]s -Force }`, q)
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("remove vm %q: %w", name, err)
	}
	return nil
}

// FormatDisk wipes a physical disk back to a raw, poolable state: it clears any
// partitions/data and resets the disk if it was retained in a storage pool.
// Destructive and deliberately guarded — it refuses the boot/system disk. The
// disk is identified by its PhysicalDisk DeviceId (as reported in inventory).
// Idempotent: a disk that is already raw simply ends up raw again.
func (p *PowerShell) FormatDisk(ctx context.Context, deviceID string) error {
	id := psQuote(deviceID)
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; "+
		"$pd = Get-PhysicalDisk | Where-Object { [string]$_.DeviceId -eq %[1]s }; "+
		"if (-not $pd) { throw 'no physical disk with DeviceId ' + %[1]s }; "+
		"$disk = $pd | Get-Disk -ErrorAction SilentlyContinue; "+
		"if ($disk) { "+
		"if ($disk.IsBoot -or $disk.IsSystem) { throw 'refusing to format the OS/boot disk' }; "+
		"Set-Disk -Number $disk.Number -IsReadOnly $false -ErrorAction SilentlyContinue; "+
		"Set-Disk -Number $disk.Number -IsOffline $false -ErrorAction SilentlyContinue; "+
		"if ($disk.PartitionStyle -ne 'RAW') { Clear-Disk -Number $disk.Number -RemoveData -RemoveOEM -Confirm:$false -ErrorAction SilentlyContinue } }; "+
		"Reset-PhysicalDisk -UniqueId $pd.UniqueId -ErrorAction SilentlyContinue; "+
		"$after = Get-PhysicalDisk | Where-Object { [string]$_.DeviceId -eq %[1]s }; "+
		"if ($after.CanPool) { 'RESULT=WIPED' } else { 'RESULT=WIPED_NOPOOL' }", id)
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("format disk %q: %w", deviceID, err)
	}
	return nil
}

// EnsureClusterVMRole makes a VM highly available. Idempotent: if a VM cluster
// group already exists for it, it is left alone. The new role's group is named
// after the VM, which is what the discovery observation keys on.
func (p *PowerShell) EnsureClusterVMRole(ctx context.Context, vmName string) (Outcome, error) {
	q := psQuote(vmName)
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; Import-Module FailoverClusters; "+
		"$g = Get-ClusterGroup -ErrorAction SilentlyContinue | Where-Object { $_.GroupType -eq 'VirtualMachine' -and $_.Name -eq %s }; "+
		"if ($g) { 'unchanged' } else { Add-ClusterVirtualMachineRole -VirtualMachine %s -Name %s | Out-Null; 'created' }", q, q, q)
	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure cluster vm role %q: %w", vmName, err)
	}
	if strings.Contains(string(out), "created") {
		return OutcomeCreated, nil
	}
	return OutcomeUnchanged, nil
}

func (p *PowerShell) MoveClusterGroup(ctx context.Context, group, node string) error {
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; Import-Module FailoverClusters; Move-ClusterGroup -Name %s -Node %s | Out-Null", psQuote(group), psQuote(node))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("move cluster group %q to %q: %w", group, node, err)
	}
	return nil
}

func (p *PowerShell) MoveClusterSharedVolume(ctx context.Context, volume, node string) error {
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; Import-Module FailoverClusters; Move-ClusterSharedVolume -Name %s -Node %s | Out-Null", psQuote(volume), psQuote(node))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("move CSV %q to %q: %w", volume, node, err)
	}
	return nil
}

// MoveClusterVM moves a clustered VM role to node. The default migration type is
// used deliberately: Failover Clustering live-migrates it when the VM is running
// (no downtime) and does a quick move when it is stopped, rather than forcing
// Live (which errors on a stopped VM).
func (p *PowerShell) MoveClusterVM(ctx context.Context, vm, node string) error {
	// On failure Move-ClusterVirtualMachineRole only says "check the event log",
	// which is useless from the console. Catch the failure and fold the recent
	// error/warning events into the message so the operator sees the real cause
	// (e.g. a migration-transport listener problem) without opening Event Viewer.
	// A clustered VM logs to the Hyper-V High-Availability channel and, for the
	// cluster side, to FailoverClustering/Operational and the System log — query
	// all of them, including Critical level. The migration type is left to
	// Hyper-V (live when the VM is running, quick when it is off).
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
Import-Module FailoverClusters
try {
  Move-ClusterVirtualMachineRole -Name %[1]s -Node %[2]s -ErrorAction Stop | Out-Null
} catch {
  $msg = $_.Exception.Message
  $since = (Get-Date).AddMinutes(-5)
  $logs = @('Microsoft-Windows-Hyper-V-High-Availability-Admin','Microsoft-Windows-Hyper-V-VMMS-Admin','Microsoft-Windows-FailoverClustering/Operational')
  # A clustered live migration fails on the DESTINATION node (the receive-side
  # listen-socket / transport error lands there), so gather events from both the
  # source (local) and the target node. The agent runs as a domain admin, so a
  # remote read of the target's log is a single hop (no delegation).
  $rows = @()
  foreach ($pair in @(@{ n = $env:COMPUTERNAME; remote = $false }, @{ n = %[2]s; remote = $true })) {
    $node = $pair.n
    $ev = @()
    foreach ($l in $logs) {
      try {
        if ($pair.remote) { $ev += Get-WinEvent -ComputerName $node -FilterHashtable @{ LogName=$l; StartTime=$since; Level=1,2,3 } -MaxEvents 6 -ErrorAction SilentlyContinue }
        else { $ev += Get-WinEvent -FilterHashtable @{ LogName=$l; StartTime=$since; Level=1,2,3 } -MaxEvents 6 -ErrorAction SilentlyContinue }
      } catch {}
    }
    try {
      if ($pair.remote) { $ev += Get-WinEvent -ComputerName $node -FilterHashtable @{ LogName='System'; StartTime=$since; Level=1,2,3 } -MaxEvents 40 -ErrorAction SilentlyContinue | Where-Object { $_.ProviderName -like '*FailoverClustering*' -or $_.ProviderName -like '*Hyper-V*' } }
      else { $ev += Get-WinEvent -FilterHashtable @{ LogName='System'; StartTime=$since; Level=1,2,3 } -MaxEvents 40 -ErrorAction SilentlyContinue | Where-Object { $_.ProviderName -like '*FailoverClustering*' -or $_.ProviderName -like '*Hyper-V*' } }
    } catch {}
    foreach ($e in ($ev | Sort-Object TimeCreated -Unique)) {
      $rows += '[' + $node + ' ' + $e.TimeCreated.ToString('HH:mm:ss') + ' id=' + $e.Id + '] ' + (($e.Message -split [Environment]::NewLine)[0])
    }
  }
  $detail = ($rows -join ' | ')
  if (-not $detail) { $detail = 'no related events on source or destination in the last 5 minutes; run Get-ClusterLog for detail' }
  throw ($msg + ' -- events: ' + $detail)
}`, psQuote(vm), psQuote(node))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("migrate VM %q to %q: %w", vm, node, err)
	}
	return nil
}

// ValidateCluster runs Test-Cluster and returns the report path. Storage tests
// are excluded by default because they can be disruptive on an in-use CSV; the
// caller can opt into a different category set via include.
func (p *PowerShell) ValidateCluster(ctx context.Context, nodes, include []string) (string, error) {
	if len(include) == 0 {
		include = []string{"Inventory", "Network", "System Configuration"}
	}
	nodeClause := ""
	if len(nodes) > 0 {
		nodeClause = "-Node " + psStringList(nodes) + " "
	}
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; Import-Module FailoverClusters; (Test-Cluster %s-Include %s -WarningAction SilentlyContinue).FullName",
		nodeClause, psStringList(include))
	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("validate cluster: %w", err)
	}
	report := strings.TrimSpace(string(out))
	if report == "" {
		return "validation ran (no report path returned)", nil
	}
	return "validation report: " + report, nil
}

// RepairHostDNS fixes the multi-homed-host DNS problem: the management NIC (the
// one with a static IPv4) carries the correct domain-controller DNS, but a DHCP
// secondary NIC is often handed the router as DNS and still registers in DNS —
// which breaks AD/DNS registration (event 1196) and can make Kerberos-dependent
// operations (live migration) flaky. This copies the management NIC's DNS onto
// every other physical NIC and turns off DNS registration on those NICs, then
// re-registers. Idempotent.
func (p *PowerShell) RepairHostDNS(ctx context.Context) (string, error) {
	script := `$ErrorActionPreference='Stop'
$mgmt = Get-NetAdapter -Physical -ErrorAction SilentlyContinue | Where-Object {
  Get-NetIPAddress -InterfaceIndex $_.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.PrefixOrigin -eq 'Manual' -and $_.IPAddress -notlike '169.254.*' }
} | Select-Object -First 1
if (-not $mgmt) { throw 'no management NIC with a static IPv4 found; cannot determine the correct DNS server' }
$dcDns = @((Get-DnsClientServerAddress -InterfaceIndex $mgmt.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue).ServerAddresses)
if (-not $dcDns) { throw 'management NIC has no DNS server configured' }
$fixed = @()
foreach ($n in (Get-NetAdapter -Physical -ErrorAction SilentlyContinue | Where-Object { $_.ifIndex -ne $mgmt.ifIndex })) {
  $cur = @((Get-DnsClientServerAddress -InterfaceIndex $n.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue).ServerAddresses)
  $reg = [bool](Get-DnsClient -InterfaceIndex $n.ifIndex -ErrorAction SilentlyContinue).RegisterThisConnectionsAddress
  $needs = $reg -or (($cur -join ',') -ne ($dcDns -join ','))
  if (-not $needs) { continue }
  try { Set-DnsClient -InterfaceIndex $n.ifIndex -RegisterThisConnectionsAddress $false -ErrorAction SilentlyContinue } catch {}
  try { Set-DnsClientServerAddress -InterfaceIndex $n.ifIndex -ServerAddresses $dcDns -ErrorAction Stop } catch {}
  $fixed += ($n.Name + ' (was ' + (($cur -join ',')) + ', reg=' + $reg + ')')
}
Register-DnsClient -ErrorAction SilentlyContinue | Out-Null
if ($fixed.Count -eq 0) { 'no change: all non-management NICs already use ' + ($dcDns -join ',') + ' and do not register' }
else { 'pointed DNS at ' + ($dcDns -join ',') + ' and disabled registration on: ' + ($fixed -join '; ') }`
	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("repair host DNS: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ClusterLog generates Get-ClusterLog for the recent window on this node and
// returns the lines relevant to migration/errors (and any operator filter, e.g.
// a VM name) — the cluster.log carries per-operation detail the Windows event
// log does not. It writes to a temp dir, greps, and cleans up. The result is
// capped so it stays a usable job message.
func (p *PowerShell) ClusterLog(ctx context.Context, span, filter string) (string, error) {
	mins := 15
	if n, err := strconv.Atoi(strings.TrimSpace(span)); err == nil && n > 0 && n <= 1440 {
		mins = n
	}
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
Import-Module FailoverClusters
$dir = Join-Path $env:TEMP ('blog_' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $dir -Force | Out-Null
try {
  Get-ClusterLog -Node $env:COMPUTERNAME -TimeSpan %[1]d -Destination $dir -ErrorAction Stop | Out-Null
  $f = Get-ChildItem -Path $dir -Filter *.log -ErrorAction SilentlyContinue | Select-Object -First 1
  if (-not $f) { 'no cluster log generated'; return }
  $pat = 'migrat|live migration|0x8007|0x800|fail|error|warn|rejected|denied|listen|kerberos|21111|20406|22038'
  $flt = %[2]s
  if ($flt) { $pat = $pat + '|' + [regex]::Escape($flt) }
  $hits = Select-String -Path $f.FullName -Pattern $pat -AllMatches -ErrorAction SilentlyContinue | Select-Object -Last 60 | ForEach-Object { $_.Line.Trim() }
  if (-not $hits) { 'cluster log generated; no migration/error lines in the last %[1]d min' ; return }
  ($hits -join [Environment]::NewLine)
} finally {
  Remove-Item $dir -Recurse -Force -ErrorAction SilentlyContinue
}`, mins, psQuote(filter))
	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("cluster log: %w", err)
	}
	res := strings.TrimSpace(string(out))
	if len(res) > 12000 {
		res = res[len(res)-12000:]
	}
	return res, nil
}
