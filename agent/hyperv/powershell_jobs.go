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

func (p *PowerShell) FetchISO(ctx context.Context, url, dest string) error {
	// Idempotent: skip when the ISO is already present. Download to a temporary
	// file then move into place so an interrupted transfer never looks complete.
	// ProgressPreference off makes Invoke-WebRequest stream large files quickly.
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'; "+
		"$dest=%[1]s; $url=%[2]s; "+
		"if (Test-Path $dest) { return }; "+
		"$dir=Split-Path -Parent $dest; if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }; "+
		"$tmp=$dest + [char]46 + 'download'; "+
		"Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing; "+
		"Move-Item -Force -Path $tmp -Destination $dest", psQuote(dest), psQuote(url))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("fetch iso to %q: %w", dest, err)
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
// RemoveMgmtVNIC removes a management-OS vNIC by name. Idempotent: a no-op when
// no such vNIC exists. Used to clean up stray/leftover management vNICs.
func (p *PowerShell) RemoveMgmtVNIC(ctx context.Context, name string) error {
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; if (Get-VMNetworkAdapter -ManagementOS -Name %[1]s -ErrorAction SilentlyContinue) { Remove-VMNetworkAdapter -ManagementOS -Name %[1]s }", psQuote(name))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("remove management vNIC %q: %w", name, err)
	}
	return nil
}

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

// ResetPoolDisks wipes every LOCAL non-OS disk that is not already an S2D pool
// member, so a node newly added to the cluster contributes its disks to the pool
// (S2D only claims blank/CanPool disks). It enumerates via Get-Disk, which is
// local to this host, so it can never touch another node's disks; it skips the
// boot/system disk and any disk already in the pool, so running it on an existing
// member is a safe no-op. Destructive for the node's own data disks — intended for
// preparing a node for S2D. Returns a summary. After this the former's
// EnsureS2DPoolDisks claims the now-poolable disks on its next pass.
func (p *PowerShell) ResetPoolDisks(ctx context.Context) (string, error) {
	script := `$ErrorActionPreference='Stop'
# Physical disks that are members of the S2D pool — never touch these.
$members = @()
try { $sp = Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial } | Select-Object -First 1; if ($sp) { $members = @(($sp | Get-PhysicalDisk -ErrorAction SilentlyContinue).UniqueId) } } catch {}
# Enumerate ONLY disks physically connected to THIS node via its StorageNode. This
# is critical: in an S2D cluster Get-Disk/Get-PhysicalDisk return cluster-wide
# objects (including the Spaces virtual disks that back the CSVs), so iterating
# those risks wiping shared storage or another node's disks. If the local node's
# disks cannot be resolved, do NOTHING rather than fall back to a cluster-wide set.
$local = @()
try {
  $sn = Get-StorageNode -ErrorAction SilentlyContinue | Where-Object { $_.Name -like ($env:COMPUTERNAME + '*') } | Select-Object -First 1
  if ($sn) { $local = @($sn | Get-PhysicalDisk -PhysicallyConnected -ErrorAction SilentlyContinue) }
} catch {}
if (-not $local -or $local.Count -eq 0) { 'RESULT wiped=0 nowPoolable=0 skipped=0 (could not resolve this node''s local disks — no action)'; return }
$wiped = 0; $pool = 0; $skipped = 0; $errs = @()
foreach ($pd in $local) {
  if ($members -contains $pd.UniqueId) { $skipped++; continue }   # already in the pool
  $disk = $pd | Get-Disk -ErrorAction SilentlyContinue
  if (-not $disk) { $skipped++; continue }
  # Never the OS/boot disk, and never a virtual/Spaces disk (a CSV) even if one
  # somehow appears local.
  if ($disk.IsBoot -or $disk.IsSystem -or $disk.BusType -eq 'Spaces' -or $disk.BusType -eq 'File Backed Virtual') { $skipped++; continue }
  try {
    Set-Disk -Number $disk.Number -IsReadOnly $false -ErrorAction SilentlyContinue
    Set-Disk -Number $disk.Number -IsOffline $false -ErrorAction SilentlyContinue
    if ($disk.PartitionStyle -ne 'RAW') { Clear-Disk -Number $disk.Number -RemoveData -RemoveOEM -Confirm:$false -ErrorAction SilentlyContinue }
    Reset-PhysicalDisk -UniqueId $pd.UniqueId -ErrorAction SilentlyContinue
    $wiped++
    $after = Get-PhysicalDisk -UniqueId $pd.UniqueId -ErrorAction SilentlyContinue
    if ($after -and $after.CanPool) { $pool++ }
  } catch { $errs += ('disk ' + $disk.Number + ': ' + $_.Exception.Message) }
}
'RESULT wiped=' + $wiped + ' nowPoolable=' + $pool + ' skipped=' + $skipped + $(if ($errs) { ' :: ' + ($errs -join '; ') } else { '' })`
	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("reset pool disks: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// FormatDiskDrive initialises a physical disk to GPT, creates a single
// max-size partition, formats it NTFS and assigns a drive letter. Refuses the
// OS/boot disk. Idempotent: if the disk already has a partition with the
// requested drive letter the operation is a no-op.
func (p *PowerShell) FormatDiskDrive(ctx context.Context, deviceID, driveLetter string) error {
	id := psQuote(deviceID)
	letter := psQuote(driveLetter)
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; "+
		"$pd = Get-PhysicalDisk | Where-Object { [string]$_.DeviceId -eq %[1]s }; "+
		"if (-not $pd) { throw 'no physical disk with DeviceId ' + %[1]s }; "+
		"$disk = $pd | Get-Disk -ErrorAction SilentlyContinue; "+
		"if (-not $disk) { throw 'disk not reachable via Get-Disk' }; "+
		"if ($disk.IsBoot -or $disk.IsSystem) { throw 'refusing to format the OS/boot disk' }; "+
		"$existing = $disk | Get-Partition -ErrorAction SilentlyContinue | Where-Object { $_.DriveLetter -eq %[2]s }; "+
		"if ($existing) { 'RESULT=NOOP'; return }; "+
		"Set-Disk -Number $disk.Number -IsReadOnly $false -ErrorAction SilentlyContinue; "+
		"Set-Disk -Number $disk.Number -IsOffline $false -ErrorAction SilentlyContinue; "+
		"if ($disk.PartitionStyle -ne 'RAW') { Clear-Disk -Number $disk.Number -RemoveData -RemoveOEM -Confirm:$false }; "+
		"Initialize-Disk -Number $disk.Number -PartitionStyle GPT; "+
		"New-Partition -DiskNumber $disk.Number -UseMaximumSize -DriveLetter %[2]s | Out-Null; "+
		"Format-Volume -DriveLetter %[2]s -FileSystem NTFS -Confirm:$false | Out-Null; "+
		"'RESULT=FORMATTED'", id, letter)
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("format disk drive %q → %s: %w", deviceID, driveLetter, err)
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
func (p *PowerShell) MoveClusterVM(ctx context.Context, vm, node string, onProgress ProgressFunc) error {
	// Run the move in a background job so the foreground can poll Msvm_MigrationJob
	// and stream PROGRESS lines. On failure keep the hard-won two-node event
	// capture: Move-ClusterVirtualMachineRole only says "check the event log", and
	// a clustered live migration's real cause often lands on the DESTINATION node
	// (receive-side transport/listener), so gather Hyper-V High-Availability, VMMS,
	// FailoverClustering/Operational and System events from both nodes. Migration
	// type is picked by VM state (Live when running, Quick when off).
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
Import-Module FailoverClusters -ErrorAction SilentlyContinue
$vm = %[1]s; $tn = %[2]s
$grp = Get-ClusterGroup -Name $vm -ErrorAction SilentlyContinue
if ($grp -and $grp.OwnerNode.Name -eq $tn) { Write-Output ('DONE already on ' + $tn + '; no move needed'); return }
$v = Get-VM -Name $vm -ErrorAction SilentlyContinue
$mt = 'Quick'; if ($v -and $v.State -eq 'Running') { $mt = 'Live' }
$job = Start-Job -ScriptBlock { param($vm,$tn,$mt) Import-Module FailoverClusters -ErrorAction SilentlyContinue; Move-ClusterVirtualMachineRole -Name $vm -Node $tn -MigrationType $mt } -ArgumentList $vm,$tn,$mt
$last = -1
while ($job.State -eq 'Running') {
  $mj = Get-CimInstance -Namespace root\virtualization\v2 -ClassName Msvm_MigrationJob -ErrorAction SilentlyContinue | Sort-Object PercentComplete -Descending | Select-Object -First 1
  if ($mj) { $pc = [int]$mj.PercentComplete; if ($pc -ne $last) { Write-Output ('PROGRESS ' + $pc); $last = $pc } }
  Start-Sleep -Seconds 2
}
Receive-Job $job -ErrorAction SilentlyContinue | Out-Null
if ($job.State -eq 'Failed') {
  $msg = ''; try { $msg = [string]$job.ChildJobs[0].JobStateInfo.Reason.Message } catch {}
  $since = (Get-Date).AddMinutes(-5)
  $logs = @('Microsoft-Windows-Hyper-V-High-Availability-Admin','Microsoft-Windows-Hyper-V-VMMS-Admin','Microsoft-Windows-FailoverClustering/Operational')
  $rows = @()
  foreach ($pair in @(@{ n = $env:COMPUTERNAME; remote = $false }, @{ n = $tn; remote = $true })) {
    $en = $pair.n; $ev = @()
    foreach ($l in $logs) {
      try {
        if ($pair.remote) { $ev += Get-WinEvent -ComputerName $en -FilterHashtable @{ LogName=$l; StartTime=$since; Level=1,2,3 } -MaxEvents 6 -ErrorAction SilentlyContinue }
        else { $ev += Get-WinEvent -FilterHashtable @{ LogName=$l; StartTime=$since; Level=1,2,3 } -MaxEvents 6 -ErrorAction SilentlyContinue }
      } catch {}
    }
    try {
      if ($pair.remote) { $ev += Get-WinEvent -ComputerName $en -FilterHashtable @{ LogName='System'; StartTime=$since; Level=1,2,3 } -MaxEvents 40 -ErrorAction SilentlyContinue | Where-Object { $_.ProviderName -like '*FailoverClustering*' -or $_.ProviderName -like '*Hyper-V*' } }
      else { $ev += Get-WinEvent -FilterHashtable @{ LogName='System'; StartTime=$since; Level=1,2,3 } -MaxEvents 40 -ErrorAction SilentlyContinue | Where-Object { $_.ProviderName -like '*FailoverClustering*' -or $_.ProviderName -like '*Hyper-V*' } }
    } catch {}
    foreach ($e in ($ev | Sort-Object TimeCreated -Unique)) {
      $rows += '[' + $en + ' ' + $e.TimeCreated.ToString('HH:mm:ss') + ' id=' + $e.Id + '] ' + (($e.Message -split [Environment]::NewLine)[0])
    }
  }
  $detail = ($rows -join ' | ')
  if (-not $detail) { $detail = 'no related events on source or destination in the last 5 minutes; run Get-ClusterLog for detail' }
  Remove-Job $job -Force -ErrorAction SilentlyContinue
  throw ('(' + $mt + ' migration) ' + $msg + ' -- events: ' + $detail)
}
Remove-Job $job -Force -ErrorAction SilentlyContinue
Write-Output ('DONE live-migrated ' + $vm + ' to ' + $tn)`, psQuote(vm), psQuote(node))
	var result string
	if err := p.runStream(ctx, script, migrationLineHandler(onProgress, &result)); err != nil {
		return fmt.Errorf("migrate VM %q to %q: %w", vm, node, err)
	}
	return nil
}

// MigrateVM shared-nothing live-migrates a standalone VM to another host, moving
// its storage too (Move-VM -IncludeStorage). It runs on the source host. It
// enables migration locally first; the destination must also have it enabled and,
// for Kerberos, constrained delegation between the two computer accounts — that is
// host setup done elsewhere. On failure it folds the recent VMMS event detail into
// the message so the real cause (transport / delegation / CPU compat) is visible.
func (p *PowerShell) MigrateVM(ctx context.Context, vm, destHost, destPath string, onProgress ProgressFunc) (string, error) {
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
$vm = %[1]s; $dest = %[2]s; $path = %[3]s
if (-not $path) { $path = 'C:\VMs\' + $vm }
if (-not (Get-VM -Name $vm -ErrorAction SilentlyContinue)) { throw ('VM ' + $vm + ' is not on this host') }
# Service-initiated migration MUST use Kerberos: CredSSP delegates an interactive
# user's credentials, which the agent service does not have, so it fails with "no
# credentials available in the security package" (0x8009030E). Enable migration
# and set Kerberos on the source and — over a single Kerberos hop, as a domain
# admin — on the destination too. Kerberos also needs constrained delegation
# between the two computer accounts, provisioned by the MigrateVM job beforehand.
Enable-VMMigration -ErrorAction SilentlyContinue | Out-Null
Set-VMHost -VirtualMachineMigrationAuthenticationType Kerberos -UseAnyNetworkForMigration $true -ErrorAction SilentlyContinue
try {
  Enable-VMMigration -ComputerName $dest -ErrorAction Stop | Out-Null
  Set-VMHost -ComputerName $dest -VirtualMachineMigrationAuthenticationType Kerberos -UseAnyNetworkForMigration $true -ErrorAction Stop
} catch {}
%[4]s
Write-Output ('DONE migrated ' + $vm + ' to ' + $dest)`,
		psQuote(vm), psQuote(destHost), psQuote(destPath),
		migrateWithProgress("Move-VM -Name $vm -DestinationHost $dest -IncludeStorage -DestinationStoragePath $path"))
	result := "migrated " + vm + " to " + destHost
	err := p.runStream(ctx, script, migrationLineHandler(onProgress, &result))
	if err != nil {
		return "", fmt.Errorf("migrate vm %q to %q: %w", vm, destHost, err)
	}
	return result, nil
}

// migrateWithProgress wraps a Move-* command so it runs in a background job while
// the foreground polls Msvm_MigrationJob and emits "PROGRESS <pct>" lines the
// agent streams into progress reports. On failure it folds the recent VMMS event
// detail into the thrown message so the real cause is visible.
func migrateWithProgress(moveCmd string) string {
	return `$job = Start-Job -ScriptBlock { param($vm,$dest,$node,$path,$mt) Import-Module Hyper-V -ErrorAction SilentlyContinue; Import-Module FailoverClusters -ErrorAction SilentlyContinue; ` + moveCmd + ` } -ArgumentList $vm,$dest,$node,$path,$mt
$last = -1
while ($job.State -eq 'Running') {
  $mj = Get-CimInstance -Namespace root\virtualization\v2 -ClassName Msvm_MigrationJob -ErrorAction SilentlyContinue | Sort-Object PercentComplete -Descending | Select-Object -First 1
  if ($mj) { $pc = [int]$mj.PercentComplete; if ($pc -ne $last) { Write-Output ('PROGRESS ' + $pc); $last = $pc } }
  Start-Sleep -Seconds 2
}
Receive-Job $job -ErrorAction SilentlyContinue | Out-Null
if ($job.State -eq 'Failed') {
  $reason = ''
  try { $reason = [string]$job.ChildJobs[0].JobStateInfo.Reason.Message } catch {}
  $detail = ''
  try {
    $ev = Get-WinEvent -FilterHashtable @{ LogName='Microsoft-Windows-Hyper-V-VMMS-Admin'; StartTime=(Get-Date).AddMinutes(-5); Level=1,2,3 } -MaxEvents 6 -ErrorAction SilentlyContinue
    if ($ev) { $detail = ' -- events: ' + (($ev | ForEach-Object { ($_.Message -split [Environment]::NewLine)[0] }) -join ' | ') }
  } catch {}
  Remove-Job $job -Force -ErrorAction SilentlyContinue
  throw ($reason + $detail)
}
Remove-Job $job -Force -ErrorAction SilentlyContinue`
}

// migrationLineHandler parses the streamed migration output: PROGRESS lines
// become progress notes; a DONE line overrides the result message.
func migrationLineHandler(onProgress ProgressFunc, result *string) func(string) {
	return func(line string) {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "PROGRESS "):
			onProgress.emit("live migration " + strings.TrimPrefix(line, "PROGRESS ") + "%")
		case strings.HasPrefix(line, "DONE "):
			*result = strings.TrimPrefix(line, "DONE ")
		}
	}
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

// RepairHostDNS fixes the multi-homed-host DNS problem: a DHCP secondary NIC is
// often handed the router as DNS and still registers in DNS, which breaks
// AD/DNS registration (event 1196) and makes Kerberos-dependent operations (live
// migration) flaky. It sets every physical NIC's DNS to the domain controller
// and registers only on the NIC that routes to the DC.
//
// The DC's DNS is determined by VERIFICATION, not by guessing the "management"
// NIC — a guess is fooled by a floating cluster IP (which is also a static
// address). The DC DNS is the configured DNS server that actually resolves the
// AD domain's SOA. dns, when non-empty, overrides this (used to recover a host
// whose NICs no longer point at the DC). Idempotent.
// EnsureHostDNS sets every up physical NIC's IPv4 DNS to the given servers,
// idempotently. Unlike RepairHostDNS it does not require domain membership, so
// it can run before a join. Reports UPDATED/NOOP via a distinct token so the
// outcome doesn't depend on substring-matching arbitrary cmdlet output.
func (p *PowerShell) EnsureHostDNS(ctx context.Context, dns []string) (Outcome, error) {
	if len(dns) == 0 {
		return OutcomeUnchanged, nil
	}
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
$servers = @(%[1]s.Split(',') | ForEach-Object { $_.Trim() } | Where-Object { $_ })
if (-not $servers) { 'RESULT=NOOP'; return }
$want = ($servers -join ',')
$changed = $false
foreach ($n in (Get-NetAdapter -ErrorAction SilentlyContinue | Where-Object { $_.Status -eq 'Up' })) {
  # Only set DNS on a NIC that carries a routable manual static IPv4 — the real
  # management NIC — not APIPA/no-IP strays or SET team members. Otherwise DNS
  # ends up stranded on a no-IP vNIC while the NIC with the IP has none.
  $hasIp = @(Get-NetIPAddress -InterfaceIndex $n.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.PrefixOrigin -eq 'Manual' -and $_.IPAddress -notlike '169.254.*' }).Count -gt 0
  if (-not $hasIp) { continue }
  $cur = @((Get-DnsClientServerAddress -InterfaceIndex $n.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue).ServerAddresses)
  if (($cur -join ',') -ne $want) {
    try { Set-DnsClientServerAddress -InterfaceIndex $n.ifIndex -ServerAddresses $servers -ErrorAction Stop; $changed = $true } catch {}
  }
}
if ($changed) { 'RESULT=UPDATED' } else { 'RESULT=NOOP' }`, psQuote(strings.Join(dns, ",")))
	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure host dns: %w", err)
	}
	if strings.Contains(string(out), "RESULT=UPDATED") {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
}

// RepairNetworkProfile sets any NIC on the Public network profile to Private. A
// host NIC stuck on Public (often a side effect of creating or removing a
// vSwitch) silently breaks WinRM and failover clustering. Domain-authenticated
// NICs are left alone (that profile is assigned automatically when the DC is
// reachable). Returns a per-NIC summary so the operator can confirm the result.
func (p *PowerShell) RepairNetworkProfile(ctx context.Context) (string, error) {
	// DomainAuthenticated cannot be set with Set-NetConnectionProfile (it only
	// accepts Public/Private) — the NLA service assigns it automatically when it
	// can reach a domain controller on the NIC. So this flips Public -> Private (so
	// the restrictive Public firewall rules stop applying) and then restarts NLA to
	// force domain re-detection: a NIC that can now reach the DC (correct DNS) is
	// promoted Private -> DomainAuthenticated, which is what cross-node WMI/RPC
	// (Add-ClusterVirtualMachineRole etc.) needs.
	const script = `$ErrorActionPreference='Continue'
$lines = @()
$changed = 0
foreach ($prof in (Get-NetConnectionProfile -ErrorAction SilentlyContinue)) {
  if ($prof.NetworkCategory -eq 'Public') {
    try {
      Set-NetConnectionProfile -InterfaceIndex $prof.InterfaceIndex -NetworkCategory Private -ErrorAction Stop
      $lines += ($prof.InterfaceAlias + ': Public -> Private')
      $changed++
    } catch {
      $lines += ($prof.InterfaceAlias + ': Public (could not change - ' + $_.Exception.Message + ')')
    }
  }
}
# A broken machine secure channel keeps a NIC off DomainAuthenticated even with
# working DNS. Only attempt a repair once the domain actually resolves (a DC is
# reachable) — otherwise it fails "server is not operational". DNS must be fixed
# first (Fix host DNS). The agent runs as a domain admin, so the repair itself has
# rights. Harmless when the channel is already healthy.
$domain = (Get-CimInstance Win32_ComputerSystem).Domain
if ($domain -and $domain -ne 'WORKGROUP') {
  $dcReachable = $false
  try { if (Resolve-DnsName -Name $domain -Type SOA -QuickTimeout -ErrorAction Stop) { $dcReachable = $true } } catch {}
  if (-not $dcReachable) {
    $lines += ('cannot resolve ' + $domain + ' — run Fix host DNS first (skipped secure-channel repair)')
  } else {
    try {
      if (-not (Test-ComputerSecureChannel -ErrorAction Stop)) {
        if (Test-ComputerSecureChannel -Repair -ErrorAction SilentlyContinue) { $lines += 'secure channel repaired' }
        else { $lines += 'secure channel down (repair failed)' }
      }
    } catch { $lines += ('secure channel: ' + $_.Exception.Message) }
  }
}
# Force NLA to re-evaluate every NIC's network location so a domain NIC with
# working DNS is promoted to DomainAuthenticated. Best-effort; no link drop.
try { Restart-Service NlaSvc -Force -ErrorAction Stop; Start-Sleep -Seconds 4; $lines += 'NLA restarted (domain re-detect)' } catch { $lines += ('NLA restart failed: ' + $_.Exception.Message) }
$after = @(Get-NetConnectionProfile -ErrorAction SilentlyContinue | ForEach-Object { $_.InterfaceAlias + '=' + $_.NetworkCategory }) -join ', '
if (-not $after) { 'no network connection profiles found' }
else { ($lines -join '; ') + ' [' + $changed + ' Public->Private] now: ' + $after }`
	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("repair network profile: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// EnsureNetworkProfilesPrivate is the idempotent, reconcile-driven form of
// RepairNetworkProfile. It flips only NICs currently on the Public profile to
// Private and reports whether it had to. It emits a "changed=<n> failed=<n>"
// summary the agent parses into an Outcome; a Set failure surfaces as an error
// so the reconciler can record it as an advisory condition without degrading the
// host (the profile is host hygiene, not part of the desired spec).
func (p *PowerShell) EnsureNetworkProfilesPrivate(ctx context.Context) (Outcome, error) {
	const script = `$ErrorActionPreference='Stop'
$changed = 0
$failed = @()
foreach ($prof in (Get-NetConnectionProfile -ErrorAction SilentlyContinue)) {
  if ($prof.NetworkCategory -eq 'Public') {
    try {
      Set-NetConnectionProfile -InterfaceIndex $prof.InterfaceIndex -NetworkCategory Private -ErrorAction Stop
      $changed++
    } catch {
      $failed += ($prof.InterfaceAlias + ': ' + $_.Exception.Message)
    }
  }
}
'changed=' + $changed + ' failed=' + $failed.Count + $(if ($failed) { ' :: ' + ($failed -join '; ') } else { '' })`
	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure network profiles private: %w", err)
	}
	summary := strings.TrimSpace(string(out))
	var changed, failed int
	fmt.Sscanf(summary, "changed=%d failed=%d", &changed, &failed)
	outcome := OutcomeUnchanged
	if changed > 0 {
		outcome = OutcomeUpdated
	}
	if failed > 0 {
		return outcome, fmt.Errorf("ensure network profiles private: %s", summary)
	}
	return outcome, nil
}

func (p *PowerShell) RepairHostDNS(ctx context.Context, dns string) (string, error) {
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
$domain = (Get-CimInstance Win32_ComputerSystem).Domain
if (-not $domain -or $domain -eq 'WORKGROUP') { throw 'host is not domain-joined' }
$forced = %[1]s
$dc = $null
if ($forced) { $dc = $forced }
else {
  $cands = @()
  foreach ($n in (Get-NetAdapter -ErrorAction SilentlyContinue)) {
    $cands += @((Get-DnsClientServerAddress -InterfaceIndex $n.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue).ServerAddresses)
  }
  $cands = @($cands | Where-Object { $_ } | Select-Object -Unique)
  foreach ($c in $cands) { try { if (Resolve-DnsName -Server $c -Name $domain -Type SOA -ErrorAction Stop) { $dc = $c; break } } catch {} }
  if (-not $dc) { throw ('no configured DNS server resolves domain ' + $domain + ' (candidates: ' + ($cands -join ',') + '); pass the DC DNS explicitly') }
}
# The management NIC keeps DNS registration; everyone else does not. It is the
# NIC carrying the host's own static IPv4 — NOT a DHCP NIC (whose default gateway
# would otherwise win a route-based guess) and NOT the floating cluster IP (also
# a static address). Cluster IPs are excluded so a node owning the cluster VIP is
# not mistaken for management.
$clusterIps = @()
try {
  Import-Module FailoverClusters -ErrorAction SilentlyContinue
  $clusterIps = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'IP Address' } | ForEach-Object { ($_ | Get-ClusterParameter -Name Address -ErrorAction SilentlyContinue).Value })
} catch {}
$mgmtIdx = -1
foreach ($n in (Get-NetAdapter -ErrorAction SilentlyContinue)) {
  $ips = @(Get-NetIPAddress -InterfaceIndex $n.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.PrefixOrigin -eq 'Manual' -and $_.IPAddress -notlike '169.254.*' -and ($clusterIps -notcontains $_.IPAddress) })
  if ($ips) { $mgmtIdx = [int]$n.ifIndex; break }
}
$fixed = @()
foreach ($n in (Get-NetAdapter -ErrorAction SilentlyContinue)) {
  $cur = @((Get-DnsClientServerAddress -InterfaceIndex $n.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue).ServerAddresses)
  $reg = [bool](Get-DnsClient -InterfaceIndex $n.ifIndex -ErrorAction SilentlyContinue).RegisterThisConnectionsAddress
  $wantReg = ([int]$n.ifIndex -eq $mgmtIdx)
  $needDns = (($cur -join ',') -ne $dc)
  if ((-not $needDns) -and ($reg -eq $wantReg)) { continue }
  if ($needDns) { try { Set-DnsClientServerAddress -InterfaceIndex $n.ifIndex -ServerAddresses $dc -ErrorAction Stop } catch {} }
  try { Set-DnsClient -InterfaceIndex $n.ifIndex -RegisterThisConnectionsAddress $wantReg -ErrorAction SilentlyContinue } catch {}
  $fixed += ($n.Name + ' (dns was ' + ($cur -join ',') + ', reg ' + $reg + '->' + $wantReg + ')')
}
Register-DnsClient -ErrorAction SilentlyContinue | Out-Null
if ($fixed.Count -eq 0) { 'no change: DC DNS ' + $dc + ' already on all NICs (register only on mgmt)' }
else { 'DC DNS = ' + $dc + ' (mgmt ifIndex ' + $mgmtIdx + '); ' + ($fixed -join '; ') }`, psQuote(dns))
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
