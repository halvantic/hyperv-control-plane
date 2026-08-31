package hyperv

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/joshua-fourie/ballast/api/types"
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

// CloneVM copies an Off source VM into an independent new VM in folder. It copies
// each source VHD (first → <new>.vhdx, extras → <new>-N.vhdx), creates the new VM
// matching the source's generation / CPU / (dynamic) memory with a fresh identity
// and a dynamic MAC, and connects the first NIC to the source's switch. The
// source must be Off — a running VM's VHDX is locked, so a live copy is refused.
func (p *PowerShell) CloneVM(ctx context.Context, srcName, newName, folder string) error {
	script := cloneVMScript(srcName, newName, folder)
	if _, err := p.run(ctx, script); err != nil {
		return fmt.Errorf("clone vm %q to %q: %w", srcName, newName, err)
	}
	return nil
}

// cloneVMScript is built as a pure function so its content is unit-tested
// without a host, like the template scripts.
func cloneVMScript(srcName, newName, folder string) string {
	// A clustered source that is Off is an OFFLINE ROLE, and an offline role is
	// deregistered from Hyper-V — so the state a clone requires is the state in
	// which Get-VM cannot find the VM. The prelude registers it for the duration
	// and puts it back; see clusteredVMRegisterPrelude.
	return fmt.Sprintf(`$ErrorActionPreference='Stop'
$src=%[1]s; $new=%[2]s; $folder=%[3]s
`, psQuote(srcName), psQuote(newName), psQuote(folder)) +
		clusteredVMRegisterPrelude("$src") +
		`$s = $__vm
$state = [string]$s.State
if ($state -eq 'Saved') {
  throw ('source VM ' + $src + ' is in a SAVED state, not Off. Its disk is not locked, but it holds writes that were still in memory when it was saved, so the clone would behave like a machine that crashed. Start it and shut the guest down cleanly, or discard the saved state (Remove-VMSavedState) to leave it Off. A clustered VM saves rather than shuts down when its role goes offline, because AutomaticStopAction defaults to Save.')
}
if ($state -ne 'Off') { throw ('source VM ' + $src + ' must be Off to clone: it is ' + $state + ', and holds its disk open while it runs - stop it first') }
if (Get-VM -Name $new -ErrorAction SilentlyContinue) { throw ('a VM named ' + $new + ' already exists on this host') }
$srcDisks = @(Get-VMHardDiskDrive -VMName $src | ForEach-Object { [string]$_.Path })
if ($srcDisks.Count -eq 0) { throw ('source VM ' + $src + ' has no disks to clone') }
if (-not (Test-Path -LiteralPath $folder)) { New-Item -ItemType Directory -Path $folder -Force | Out-Null }
$newDisks = @()
for ($i=0; $i -lt $srcDisks.Count; $i++) {
  $fn = if ($i -eq 0) { $new + '.vhdx' } else { $new + '-' + $i + '.vhdx' }
  $dest = Join-Path $folder $fn
  if (Test-Path -LiteralPath $dest) { throw ('target disk already exists: ' + $dest) }
  Copy-Item -LiteralPath $srcDisks[$i] -Destination $dest -Force
  $newDisks += $dest
}
$gen = [int]$s.Generation
New-VM -Name $new -Generation $gen -MemoryStartupBytes ([int64]$s.MemoryStartup) -VHDPath $newDisks[0] | Out-Null
for ($i=1; $i -lt $newDisks.Count; $i++) { Add-VMHardDiskDrive -VMName $new -Path $newDisks[$i] }
Set-VM -Name $new -ProcessorCount ([int]$s.ProcessorCount)
if ($s.DynamicMemoryEnabled) { Set-VM -Name $new -DynamicMemory -MemoryMinimumBytes ([int64]$s.MemoryMinimum) -MemoryMaximumBytes ([int64]$s.MemoryMaximum) }
$sn = @(Get-VMNetworkAdapter -VMName $src)[0]
if ($sn -and [string]$sn.SwitchName) { Get-VMNetworkAdapter -VMName $new | Connect-VMNetworkAdapter -SwitchName ([string]$sn.SwitchName) }
'RESULT=OK'` +
		clusteredVMRestoreSuffix
}

func (p *PowerShell) FetchISO(ctx context.Context, url, dest string) (string, error) {
	// Idempotent: skip when the ISO is already present. Fetch to a temp file then
	// move into place so an interrupted transfer never looks complete.
	//
	// Source can be a UNC/local path or an http(s) URL:
	//   - UNC (\\server\share\x.iso) or local (D:\x.iso): Copy-Item, direct and
	//     resumable-safe — the robust way to place a large ISO, no centre hop.
	//   - URL: a streaming, resumable download.
	//
	// Start-BitsTransfer is deliberately NOT used, and must not be reintroduced.
	// BITS creates its transfer job in the caller's logon session; the agent runs
	// as a Windows service in Session 0 with no interactive logon, so every call
	// fails ERROR_NOT_LOGGED_ON ("the user has not logged on to the network").
	// That failure was swallowed by a bare catch for months, so every multi-GB
	// fetch silently took an Invoke-WebRequest fallback that buffers, cannot
	// resume, and died on long transfers ("an existing connection was forcibly
	// closed"), restarting from zero each retry.
	//
	// Instead: stream the response straight to disk with HttpClient
	// (ResponseHeadersRead, so nothing is buffered in memory) and resume with a
	// Range request on retry, picking up from the bytes already on disk. The
	// centre serves ISOs with http.ServeContent, which honours Range. A server
	// that ignores Range answers 200 rather than 206, which is detected and
	// restarts the file cleanly rather than corrupting it by appending.
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; "+
		"$dest=%[1]s; $url=%[2]s; "+
		"if (Test-Path $dest) { return }; "+
		"$dir=Split-Path -Parent $dest; if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }; "+
		"$tmp=$dest + [char]46 + 'download'; "+
		// A leftover temp file is either an abandoned transfer (delete it and
		// retry) or one another process still holds open. Removing it used to be
		// -ErrorAction SilentlyContinue, so a locked file fell through to the
		// download and died with a bare "the process cannot access the file".
		// Distinguish the two: a lock means a concurrent placement, and saying so
		// is the difference between an operator retrying and an operator guessing.
		"if (Test-Path $tmp) { "+
		"  try { Remove-Item $tmp -Force -ErrorAction Stop } "+
		"  catch { throw ('a transfer of this ISO is already in progress on this host (' + $tmp + ' is open in another process); wait for it to finish, or delete that file if it was abandoned') } "+
		"}; "+
		"if ($url -like '\\\\*' -or $url -match '^[A-Za-z]:\\\\') { "+
		"  Copy-Item -LiteralPath $url -Destination $tmp -Force "+
		"} else { "+
		"  Add-Type -AssemblyName System.Net.Http -ErrorAction SilentlyContinue; "+
		"  $attempt=0; $maxAttempts=5; $done=$false; $lastErr=''; $resumed=0; "+
		"  while (-not $done -and $attempt -lt $maxAttempts) { "+
		"    $attempt++; "+
		"    $have=0; if (Test-Path $tmp) { $have=[int64](Get-Item $tmp).Length }; "+
		"    $h=$null; $resp=$null; $fs=$null; "+
		"    try { "+
		"      $h=New-Object System.Net.Http.HttpClient; "+
		// With ResponseHeadersRead this bounds only the header phase, so keep it
		// short — a server that never answers should fail fast. The body is
		// bounded separately by the per-read idle timeout below.
		"      $h.Timeout=[TimeSpan]::FromMinutes(5); "+
		"      $req=New-Object System.Net.Http.HttpRequestMessage([System.Net.Http.HttpMethod]::Get, $url); "+
		"      if ($have -gt 0) { $req.Headers.Range=New-Object System.Net.Http.Headers.RangeHeaderValue($have, $null) }; "+
		"      $resp=$h.SendAsync($req, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult(); "+
		"      if (-not $resp.IsSuccessStatusCode) { throw ('HTTP ' + [int]$resp.StatusCode + ' ' + [string]$resp.ReasonPhrase) }; "+
		// 206 means the server honoured the Range and we append; a 200 to a ranged
		// request means it ignored it and is resending the whole body, so the file
		// must be truncated or the two copies would be concatenated.
		"      $append=($have -gt 0 -and [int]$resp.StatusCode -eq 206); "+
		"      if ($have -gt 0 -and -not $append) { $have=0 }; "+
		"      if ($append) { $resumed=$resumed+1 }; "+
		"      $mode=[System.IO.FileMode]::Create; if ($append) { $mode=[System.IO.FileMode]::Append }; "+
		"      $fs=New-Object System.IO.FileStream($tmp, $mode, [System.IO.FileAccess]::Write, [System.IO.FileShare]::None); "+
		"      $stream=$resp.Content.ReadAsStreamAsync().GetAwaiter().GetResult(); "+
		// Copy chunk by chunk with an idle timeout rather than Stream.CopyTo.
		// CopyTo has no read deadline: HttpClient.Timeout does not cover the body
		// once headers are read, so a stalled or half-open connection blocks
		// forever and the retry/resume below can never fire — observed live as a
		// transfer frozen at 183MB while the server had already finished the
		// request 8 minutes earlier. A read that delivers nothing for idleSeconds
		// is treated as dead, which unwinds into the retry and resumes by Range.
		"      $idleMs=60000; $buf=New-Object byte[] 1048576; "+
		"      while ($true) { "+
		"        $rt=$stream.ReadAsync($buf,0,$buf.Length); "+
		"        if (-not $rt.Wait($idleMs)) { throw ('transfer stalled: no data for ' + [int]($idleMs/1000) + 's') }; "+
		"        $n=$rt.Result; if ($n -le 0) { break }; "+
		"        $fs.Write($buf,0,$n) "+
		"      }; "+
		// Only a body that ran to a clean end counts as done; anything else falls
		// through to a resumed retry.
		"      $done=$true "+
		"    } catch { "+
		"      $lastErr=[string]$_.Exception.Message "+
		"    } finally { "+
		"      if ($fs) { $fs.Dispose() }; if ($resp) { $resp.Dispose() }; if ($h) { $h.Dispose() } "+
		"    }; "+
		"    if (-not $done -and $attempt -lt $maxAttempts) { Start-Sleep -Seconds ([Math]::Min(30, 3 * $attempt)) } "+
		"  }; "+
		"  if (-not $done) { throw ('download failed after ' + $attempt + ' attempt(s): ' + $lastErr) }; "+
		"  if ($resumed -gt 0) { Write-Output ('NOTE=transfer resumed ' + $resumed + ' time(s) after a dropped connection') } "+
		"}; "+
		"Move-Item -Force -Path $tmp -Destination $dest", psQuote(dest), psQuote(url))
	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("fetch iso to %q: %w", dest, err)
	}
	return firstMarkerValue(string(out), "NOTE="), nil
}

// firstMarkerValue returns the text after the first line beginning with prefix,
// or "" when no line carries it. Scripts use it to hand back an advisory note
// alongside a plain success, without turning the note into an error.
func firstMarkerValue(out, prefix string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
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

// RemoveMgmtVNIC removes a management-OS vNIC by name. Idempotent: a no-op when
// no such vNIC exists. Used to clean up stray/leftover management vNICs.
func (p *PowerShell) RemoveMgmtVNIC(ctx context.Context, name string) error {
	script := fmt.Sprintf("$ErrorActionPreference='Stop'; if (Get-VMNetworkAdapter -ManagementOS -Name %[1]s -ErrorAction SilentlyContinue) { Remove-VMNetworkAdapter -ManagementOS -Name %[1]s }", psQuote(name))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("remove management vNIC %q: %w", name, err)
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
  # Take the group offline by TURNING THE VM OFF, not by saving it.
  #
  # Stop-ClusterGroup obeys the Virtual Machine resource's OfflineAction, which
  # Windows defaults to Save (1). So deleting a clustered VM wrote its entire
  # memory image to disk and then threw that image away — the same default that
  # caught the power path (see vmPowerScript, which shuts the guest down first).
  #
  # The cost is not only the wasted write. The removal has to finish before the
  # reconcile loop's next pass reaches this VM, or the pass recreates the VM the
  # job has just deleted; a save of several GB is exactly what makes those two
  # overlap. It is why 'Tes' came back into the cluster minutes after it was
  # deleted (2026-08-20).
  #
  # Best effort by design: the resource is deleted moments later, so overriding
  # its offline action changes nothing that outlives this script, and a failure
  # here simply leaves the old, slower behaviour.
  try {
    foreach ($res in @($g | Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { [string]$_.ResourceType -eq 'Virtual Machine' })) {
      Set-ClusterParameter -InputObject $res -Name OfflineAction -Value 0 -ErrorAction Stop
    }
  } catch {}
  Stop-ClusterGroup -Name %[1]s -ErrorAction SilentlyContinue | Out-Null
  Remove-ClusterGroup -Name %[1]s -RemoveResources -Force -ErrorAction SilentlyContinue
}
$vm = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
if ($vm) {
  # Stopping is a courtesy, not a precondition. A VM whose configuration storage
  # has gone sits in SavedCritical and Stop-VM cannot work — Hyper-V has nothing
  # to read — so under $ErrorActionPreference='Stop' its failure aborted the
  # script before Remove-VM ever ran. The VM the operator most needs to delete
  # was the one deletion refused to attempt.
  #
  # Observed 2026-08-06: 'Windows Temp Test' on HVNEW03, SavedCritical with
  # "Cannot connect to virtual machine configuration storage" after its CSV was
  # deleted, and every removal failing on Stop-VM.
  #
  # Remove-VM does not need the storage: it removes the REGISTRATION. So try to
  # stop, tolerate failure, and always go on to remove.
  if ($vm.State -ne 'Off') {
    try { Stop-VM -Name %[1]s -TurnOff -Force -ErrorAction Stop } catch {}
  }
  try { Remove-VM -Name %[1]s -Force -ErrorAction Stop } catch {}
  # Judge by the outcome, not by whether a cmdlet complained. Remove-VM can emit
  # a trailing error for a VM whose files it could not tidy while still having
  # deregistered it, and reporting that as a failure would leave the operator
  # deleting something that is already gone.
  $still = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
  if ($still) {
    $state = [string]$still.State
    if ($state -eq 'SavedCritical' -or $state -eq 'Critical' -or [string]$still.Status -like '*configuration storage*') {
      throw ('cannot remove ' + %[1]s + ': it is ' + $state + ' because Hyper-V cannot reach its configuration storage (' + [string]$still.Status + '), and the registration survived the attempt. Its files are on storage that is gone - restore or reattach that storage and retry, or remove the registration on the host.')
    }
    throw ('cannot remove ' + %[1]s + ': it is still registered on this host after the removal (state ' + $state + ').')
  }
}`, q)
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
	// Selected by UniqueId when the identifier is one, and AMBIGUITY IS REFUSED.
	//
	// DeviceId is unique per bus, not per host: a local SSD and an iSCSI LUN both
	// report DeviceId 2. Matching on it could return two disks, and then the OS
	// guard silently stopped working — $disk.IsBoot on a two-element array is null,
	// which is falsy — while Clear-Disk took the first Number in the array. A
	// destructive operation was choosing its target by an identifier that does not
	// identify. Refusing costs one message; guessing costs the wrong disk.
	script := formatDiskScript(deviceID)
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
# Safeguard: never wipe disks while the pool is unhealthy or a repair/regeneration
# is running. Adding disk churn to a degraded pool is exactly what cascades into a
# heartbeat/repair storm — make the operator resolve pool health first.
$sp0 = @(Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial })[0]
if ($sp0 -and $sp0.HealthStatus -ne 'Healthy') { throw ('refusing to reset disks: the S2D pool is ' + $sp0.HealthStatus + '/' + ($sp0.OperationalStatus -join ',') + '. Resolve pool health first (a repair may be in progress).') }
$rj = @(Get-StorageJob -ErrorAction SilentlyContinue | Where-Object { $_.JobState -eq 'Running' -and $_.Name -match 'Repair|Regeneration|Rebalance' })
if ($rj.Count -gt 0) { throw ('refusing to reset disks: a storage ' + ($rj[0].Name) + ' job is in progress — wait for it to finish before adding disks.') }
# Physical disks that are members of the S2D pool — never touch these.
$members = @()
try { $sp = @(Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial })[0]; if ($sp) { $members = @(($sp | Get-PhysicalDisk -ErrorAction SilentlyContinue).UniqueId) } } catch {}
# Enumerate ONLY disks physically connected to THIS node via its StorageNode. This
# is critical: in an S2D cluster Get-Disk/Get-PhysicalDisk return cluster-wide
# objects (including the Spaces virtual disks that back the CSVs), so iterating
# those risks wiping shared storage or another node's disks. If the local node's
# disks cannot be resolved, do NOTHING rather than fall back to a cluster-wide set.
$local = @()
try {
  $sn = @(Get-StorageNode -ErrorAction SilentlyContinue | Where-Object { $_.Name -like ($env:COMPUTERNAME + '*') })[0]
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

// ReleasePoolDisks releases disks that a storage pool still claims on a host
// that is no longer a cluster member, returning them to CanPool.
//
// This is the inverse of ResetPoolDisks, which skips pool members by design — so
// after a cluster is torn down (or a teardown fails part-way) every data disk
// sits "In a Pool", claimed by a pool with no cluster behind it, and nothing in
// Ballast could release it. Clearing partitions does not help: pool membership
// lives in the disk's metadata, so Clear-Disk leaves CanPool false. The claim is
// only released by removing the pool that holds it and resetting the disk.
//
// deviceID names a single disk by UniqueId (preferred) or DeviceId; empty
// releases every non-OS disk physically connected to this node.
//
// Refused outright on a cluster member: there the pool is live and owned by S2D,
// and releasing a disk out from under it is damage, not recovery.
func (p *PowerShell) ReleasePoolDisks(ctx context.Context, deviceID string) (string, error) {
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
$target = %s
# A live cluster member's pool belongs to S2D. Releasing a disk from under it is
# not a recovery, so refuse rather than let the console offer a destructive act
# that looks like a repair.
$cl = $null
try { Import-Module FailoverClusters -ErrorAction SilentlyContinue; $cl = Get-Cluster -ErrorAction SilentlyContinue } catch {}
if ($cl) { throw ('refusing to release pool disks: this host is a member of cluster ' + $cl.Name + ', where the pool is owned by Storage Spaces Direct. Evict the node first, or use the cluster''s own pool actions.') }

# Only disks physically connected to THIS node. Get-PhysicalDisk is cluster-wide
# in a Spaces context, and a fallback to that set could wipe shared storage.
$local = @()
try {
  $sn = @(Get-StorageNode -ErrorAction SilentlyContinue | Where-Object { $_.Name -like ($env:COMPUTERNAME + '*') })[0]
  if ($sn) { $local = @($sn | Get-PhysicalDisk -PhysicallyConnected -ErrorAction SilentlyContinue) }
} catch {}
if (-not $local -or $local.Count -eq 0) { $local = @(Get-PhysicalDisk -ErrorAction SilentlyContinue | Where-Object { $_.BusType -ne 'Spaces' -and $_.BusType -ne 'File Backed Virtual' }) }
if (-not $local -or $local.Count -eq 0) { throw 'could not resolve this host''s local physical disks, so nothing was touched' }

if ($target) { $local = @($local | Where-Object { $_.UniqueId -eq $target -or [string]$_.DeviceId -eq $target }) }
if ($target -and $local.Count -eq 0) { throw ('no local physical disk matches ' + $target) }
if ($target -and $local.Count -gt 1) { throw ('the id ' + $target + ' matches ' + $local.Count + ' local disks; identify the disk by its unique id instead') }

# Drop the pools holding the claims. Read-only is how an orphaned pool comes up
# once its cluster is gone, and a read-only pool refuses every removal.
$pools = 0; $poolErrs = @()
foreach ($sp in @(Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial })) {
  try {
    Set-StoragePool -InputObject $sp -IsReadOnly $false -ErrorAction SilentlyContinue
    $sp | Get-VirtualDisk -ErrorAction SilentlyContinue | Remove-VirtualDisk -Confirm:$false -ErrorAction SilentlyContinue
    Remove-StoragePool -InputObject $sp -Confirm:$false -ErrorAction Stop
    $pools++
  } catch { $poolErrs += ($sp.FriendlyName + ': ' + $_.Exception.Message) }
}

$released = 0; $poolable = 0; $skipped = 0; $errs = @()
foreach ($pd in $local) {
  $disk = $pd | Get-Disk -ErrorAction SilentlyContinue
  if ($disk -and ($disk.IsBoot -or $disk.IsSystem)) { $skipped++; continue }
  if ($pd.BusType -eq 'Spaces' -or $pd.BusType -eq 'File Backed Virtual') { $skipped++; continue }
  try {
    Reset-PhysicalDisk -UniqueId $pd.UniqueId -ErrorAction SilentlyContinue
    if ($disk) {
      Set-Disk -Number $disk.Number -IsReadOnly $false -ErrorAction SilentlyContinue
      Set-Disk -Number $disk.Number -IsOffline $false -ErrorAction SilentlyContinue
      if ($disk.PartitionStyle -ne 'RAW') { Clear-Disk -Number $disk.Number -RemoveData -RemoveOEM -Confirm:$false -ErrorAction SilentlyContinue }
    }
    $released++
    $after = Get-PhysicalDisk -UniqueId $pd.UniqueId -ErrorAction SilentlyContinue
    if ($after -and $after.CanPool) { $poolable++ }
  } catch { $errs += ($pd.UniqueId + ': ' + $_.Exception.Message) }
}
# Say how many actually came back poolable, not just how many were acted on: a
# disk that is still claimed after this is the whole point of the report.
$msg = 'RESULT released=' + $released + ' nowPoolable=' + $poolable + ' skipped=' + $skipped + ' poolsRemoved=' + $pools
if ($released -gt 0 -and $poolable -lt $released) { $msg += ' :: ' + ($released - $poolable) + ' disk(s) are still claimed — a reboot rescans the storage bus and clears ghost pool entries' }
if ($poolErrs) { $msg += ' :: pool removal: ' + ($poolErrs -join '; ') }
if ($errs) { $msg += ' :: ' + ($errs -join '; ') }
$msg`, psQuote(deviceID))
	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("release pool disks: %w", err)
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

// ClusterVMRolesPresent reports which of the named VMs already exist as
// highly-available cluster roles, in ONE query.
//
// EnsureClusterVMRole enumerates every cluster group to answer that for a single
// VM, so reconciling n VMs ran the same cluster-wide query n times. On the rig
// Get-ClusterGroup costs 2437ms cold, which is 2.4s per VM spent re-reading a
// list that is identical every time — 73s on a thirty-VM host.
//
// Only the read is batched. Creating a missing role stays per VM: it is rare,
// it is the part that can fail, and its outcome belongs to one VM's condition.
func (p *PowerShell) ClusterVMRolesPresent(ctx context.Context, names []string) (map[string]bool, error) {
	if len(names) == 0 {
		return map[string]bool{}, nil
	}
	// One name per line rather than JSON: ConvertTo-Json on Windows PowerShell
	// collapses a single-element array to a bare string, so a cluster with
	// exactly one VM role would decode differently from one with two. Lines have
	// no such edge, and a role name cannot contain a newline.
	script := "$ErrorActionPreference='Stop'; Import-Module FailoverClusters; " +
		"Get-ClusterGroup -ErrorAction SilentlyContinue | Where-Object { $_.GroupType -eq 'VirtualMachine' } | " +
		"ForEach-Object { [string]$_.Name }"
	out, err := p.run(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("list cluster vm roles: %w", err)
	}
	present := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			present[strings.ToLower(name)] = true
		}
	}
	out2 := make(map[string]bool, len(names))
	for _, n := range names {
		out2[n] = present[strings.ToLower(n)]
	}
	return out2, nil
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
  $mj = @(Get-CimInstance -Namespace root\virtualization\v2 -ClassName Msvm_MigrationJob -ErrorAction SilentlyContinue | Sort-Object PercentComplete -Descending)[0]
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
func (p *PowerShell) MigrateVM(ctx context.Context, vm, destHost, destPath, sourceCluster, targetCluster string, networkMap []types.EvacuationNIC, onProgress ProgressFunc) (string, error) {
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
# The copy goes through an administrative share the DESTINATION publishes for
# this host, so the move rests on plain SMB between the two. Ballast turns File
# and Printer Sharing on there itself rather than failing and asking somebody to
# do it: both ends are hosts it runs an agent on, and telling an operator to open
# a PowerShell session on the destination is the defect, not the remedy.
#
# It reaches the destination the same way the migration settings above do — one
# Kerberos hop as a domain admin. Idempotent, and SAID OUT LOUD when it changes
# anything: a firewall that Ballast opened is a change to the host that outlives
# this job, and the operator should not have to infer it.
$fwNote = ''
try {
  $fwSession = New-CimSession -ComputerName $dest -ErrorAction Stop
  $fwOff = @(Get-NetFirewallRule -CimSession $fwSession -DisplayGroup 'File and Printer Sharing' -ErrorAction Stop |
    Where-Object { -not $_.Enabled -or [string]$_.Enabled -eq 'False' })
  if ($fwOff.Count -gt 0) {
    Enable-NetFirewallRule -CimSession $fwSession -DisplayGroup 'File and Printer Sharing' -ErrorAction Stop
    Write-Output ('PROGRESS enabled File and Printer Sharing on ' + $dest + ' so the migration share can be reached')
  }
  Remove-CimSession $fwSession -ErrorAction SilentlyContinue
} catch {
  $fwNote = ' Ballast could not open the firewall on ' + $dest + ' itself: ' + $_.Exception.Message + '.'
}

# Then check it, BEFORE anything is touched. Without SMB, Move-VM gets most of
# the way in and fails with 0x80070035 - having already stunned the guest and,
# with the role removed first, having left the VM un-clustered.
#
# admin$ is the closest thing testable in advance: same protocol, same
# authentication, present on any host that could publish the migration share.
$destShare = '\\' + $dest + '\admin$'
if (-not (Test-Path $destShare -ErrorAction SilentlyContinue)) {
  throw ('this host cannot reach ' + $destShare + ' over SMB, and a shared-nothing migration copies through an ' +
    'administrative share on the destination. Nothing has been changed here.' + $fwNote + ' File and Printer Sharing is ' +
    'on, so what is left is the network between the two hosts: check this host can resolve and reach ' + $dest + ' on 445')
}
%[5]s
try {
%[4]s
# Did it actually leave?
#
# A move that reported success and did not happen is the worst thing this job
# can produce: the evacuation records the VM as Moved, stops watching it, and
# the operator reads a host as empty that is still running everything. It cost
# two VMs to find, both reported Moved with their disks untouched on the source
# CSV. Move-VM takes the VM off this host, so it being here afterwards is proof
# the move did not happen, whatever the exit status said.
if (Get-VM -Name %[1]s -ErrorAction SilentlyContinue) {
  throw ('the move reported success but ' + %[1]s + ' is still registered on this host, so nothing was moved. ' +
    'The VM has not been touched and is where it was')
}
} catch {
%[7]s  throw
}
%[6]s
Write-Output ('DONE migrated ' + $vm + ' to ' + $dest)`,
		psQuote(vm), psQuote(destHost), psQuote(destPath),
		migrateWithProgress(moveWithNetworkMap(networkMap)),
		unclusterScript(vm, sourceCluster), reclusterScript(vm, targetCluster), restoreClusterScript(vm, sourceCluster))
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
	/* $ErrorActionPreference INSIDE the job.

	   It is set in the parent script, and a Start-Job child does not inherit it
	   — it runs at the default Continue. So a NON-TERMINATING Move-VM error left
	   the job in state Completed, the failure check below never fired, and the
	   caller wrote "DONE migrated" over a move that had not happened.

	   Measured: HVNew01 and HVNew02, both powered off, were reported Moved by an
	   evacuation while their disks sat untouched on the source CSV. The one that
	   did work was the running VM, because a live migration produces the
	   progress this loop watches and a real result. */
	return `$job = Start-Job -ScriptBlock { param($vm,$dest,$node,$path,$mt) $ErrorActionPreference = 'Stop'; Import-Module Hyper-V -ErrorAction SilentlyContinue; Import-Module FailoverClusters -ErrorAction SilentlyContinue; ` + moveCmd + ` } -ArgumentList $vm,$dest,$node,$path,$mt
$last = -1
while ($job.State -eq 'Running') {
  $mj = @(Get-CimInstance -Namespace root\virtualization\v2 -ClassName Msvm_MigrationJob -ErrorAction SilentlyContinue | Sort-Object PercentComplete -Descending)[0]
  if ($mj) { $pc = [int]$mj.PercentComplete; if ($pc -ne $last) { Write-Output ('PROGRESS ' + $pc); $last = $pc } }
  Start-Sleep -Seconds 2
}
# The job's own output and errors, KEPT. Piping them to Out-Null discarded the
# only account of what went wrong, so a failure that did reach here arrived as
# an empty message.
$jobErr = ''
try {
  $out = @(Receive-Job $job -ErrorAction SilentlyContinue 2>&1)
  $jobErr = (($out | Where-Object { $_ -is [System.Management.Automation.ErrorRecord] } |
    ForEach-Object { [string]$_.Exception.Message }) -join ' | ')
} catch {}
if ($job.State -eq 'Failed' -or $jobErr) {
  $reason = $jobErr
  try { if (-not $reason) { $reason = [string]$job.ChildJobs[0].JobStateInfo.Reason.Message } } catch {}
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
// EnsureHostDNS enforces the rule "DNS belongs where the default route lives":
// every up adapter with a manual static IPv4 AND a default route (the
// management path) gets the given DNS servers; a manual-IP adapter with NO
// default route (an isolated cluster network — storage, live migration) gets
// its DNS cleared and DNS registration disabled. DNS or a gateway on an
// interface makes Failover Clustering classify that network as
// ClusterAndClient, and New-Cluster then demands a cluster IP for it — DNS
// pushed onto a storage vNIC broke cluster formation exactly that way.
// Idempotent; unlike RepairHostDNS it does not require domain membership.
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
  # ANY real IPv4 address, however it was obtained — not just a static one.
  #
  # Requiring PrefixOrigin 'Manual' skipped every DHCP-addressed NIC, which is
  # exactly what a freshly onboarded host has. So a host that declared DNS
  # servers had them silently not applied: DHCP kept handing it the firewall as
  # its resolver, the domain join could not find the domain's SRV records, and
  # nothing reported that the declared DNS had been ignored. Setting DNS on a
  # DHCP interface is ordinary and does not disturb its address.
  #
  # APIPA is still excluded: 169.254 means the NIC has no usable address at all.
  $hasIp = @(Get-NetIPAddress -InterfaceIndex $n.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -notlike '169.254.*' }).Count -gt 0
  if (-not $hasIp) { continue }
  $routed = @(Get-NetRoute -InterfaceIndex $n.ifIndex -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue).Count -gt 0
  $cur = @((Get-DnsClientServerAddress -InterfaceIndex $n.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue).ServerAddresses)
  if ($routed) {
    if (($cur -join ',') -ne $want) {
      try { Set-DnsClientServerAddress -InterfaceIndex $n.ifIndex -ServerAddresses $servers -ErrorAction Stop; $changed = $true } catch {}
    }
  } else {
    # Isolated cluster network: no DNS, no DNS registration, so clustering
    # keeps it cluster-only and no stray A records are published for it.
    if ($cur.Count -gt 0) {
      try { Set-DnsClientServerAddress -InterfaceIndex $n.ifIndex -ResetServerAddresses -ErrorAction Stop; $changed = $true } catch {}
    }
    $dc = Get-DnsClient -InterfaceIndex $n.ifIndex -ErrorAction SilentlyContinue
    if ($dc -and $dc.RegisterThisConnectionsAddress) {
      try { Set-DnsClient -InterfaceIndex $n.ifIndex -RegisterThisConnectionsAddress $false -ErrorAction Stop; $changed = $true } catch {}
    }
    # And no IPv6 default route from router advertisements: on a flat layer-2
    # every interface hears the RA, and any default route (v4 OR v6) makes
    # clustering classify the network ClusterAndClient, which blocks New-Cluster
    # ("no address was given to configure the Cluster Name on this network").
    $v6def = @(Get-NetRoute -InterfaceIndex $n.ifIndex -AddressFamily IPv6 -DestinationPrefix '::/0' -ErrorAction SilentlyContinue)
    if ($v6def.Count -gt 0) {
      try { Set-NetIPInterface -InterfaceIndex $n.ifIndex -AddressFamily IPv6 -RouterDiscovery Disabled -ErrorAction Stop } catch {}
      try { $v6def | Remove-NetRoute -Confirm:$false -ErrorAction Stop; $changed = $true } catch {}
    }
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
$stuck = 0
foreach ($prof in (Get-NetConnectionProfile -ErrorAction SilentlyContinue)) {
  if ($prof.NetworkCategory -eq 'Public') {
    try {
      Set-NetConnectionProfile -InterfaceIndex $prof.InterfaceIndex -NetworkCategory Private -ErrorAction Stop
      $changed++
    } catch {
      # "network marked 'Identifying...'" is not a permanent failure — it is NLA
      # still deciding, and Set-NetConnectionProfile refuses while it does. On an
      # isolated storage/live-migration VLAN with no gateway or DNS, that state
      # can persist, so a heal that only ever calls Set gives up every pass and
      # the NIC stays Public for ever. Observed live: a Storage vNIC sat Public
      # while this ran on every cycle, blocking the SMB traffic carrying CSV I/O
      # and taking a volume degraded. Restarting NLA forces re-identification,
      # which is what the operator-run repair does and why that one works.
      $failed += ($prof.InterfaceAlias + ': ' + $_.Exception.Message)
      if ($_.Exception.Message -match 'Identifying') { $stuck++ }
    }
  }
}
if ($stuck -gt 0) {
  try {
    Restart-Service NlaSvc -Force -ErrorAction Stop
    Start-Sleep -Seconds 4
    # Re-try the ones that were mid-identification; NLA may also have promoted
    # them straight to DomainAuthenticated, which is better than Private.
    $failed = @()
    foreach ($prof in (Get-NetConnectionProfile -ErrorAction SilentlyContinue)) {
      if ($prof.NetworkCategory -eq 'Public') {
        try { Set-NetConnectionProfile -InterfaceIndex $prof.InterfaceIndex -NetworkCategory Private -ErrorAction Stop; $changed++ }
        catch { $failed += ($prof.InterfaceAlias + ': ' + $_.Exception.Message) }
      }
    }
  } catch {
    $failed += ('NLA restart failed: ' + $_.Exception.Message)
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
$forced = %[1]s
$dc = $null
if ($forced) { $dc = $forced }
else {
  # Discovery works by asking each configured resolver for the domain's SOA, so
  # it needs a domain to ask about. A workgroup host has none — but that is
  # exactly the host whose DNS is wrong, cannot join because of it, and cannot
  # fix it by joining. So the workgroup test belongs HERE, on the guess, not on
  # the whole job: an explicitly supplied DC DNS is always applied.
  $domain = (Get-CimInstance Win32_ComputerSystem).Domain
  if (-not $domain -or $domain -eq 'WORKGROUP') { throw 'host is not domain-joined, so its DC cannot be discovered from its domain; supply the DC DNS address explicitly' }
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
  if (-not $ips) { continue }
  # It must also be ROUTED. A converged host has several statically addressed
  # interfaces and Get-NetAdapter returns them in no meaningful order, so "first
  # one with a static IP" can select the storage vNIC and hand DNS registration
  # to a network that must never publish an A record. The default route is what
  # distinguishes management from fabric.
  if (@(Get-NetRoute -InterfaceIndex $n.ifIndex -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue).Count -eq 0) { continue }
  $mgmtIdx = [int]$n.ifIndex; break
}
# A host not yet given a static address — a freshly onboarded one, still on DHCP
# — matches nothing above. Falling through with mgmtIdx unset would then DISABLE
# registration on its only routed NIC, so the host stops publishing its own A
# record: a repair that breaks name resolution. Treat the routed DHCP NIC as
# management instead.
if ($mgmtIdx -lt 0) {
  foreach ($n in (Get-NetAdapter -ErrorAction SilentlyContinue)) {
    $ips = @(Get-NetIPAddress -InterfaceIndex $n.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -notlike '169.254.*' -and ($clusterIps -notcontains $_.IPAddress) })
    if (-not $ips) { continue }
    if (@(Get-NetRoute -InterfaceIndex $n.ifIndex -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue).Count -eq 0) { continue }
    $mgmtIdx = [int]$n.ifIndex; break
  }
}
$fixed = @()
foreach ($n in (Get-NetAdapter -ErrorAction SilentlyContinue)) {
  $cur = @((Get-DnsClientServerAddress -InterfaceIndex $n.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue).ServerAddresses)
  $reg = [bool](Get-DnsClient -InterfaceIndex $n.ifIndex -ErrorAction SilentlyContinue).RegisterThisConnectionsAddress
  $wantReg = ([int]$n.ifIndex -eq $mgmtIdx)
  # An interface with no IPv4 default route is an ISOLATED cluster network
  # (storage, live migration). It must have no DNS at all and must not register:
  # a published A record for a non-routed address breaks name resolution to the
  # host, and DNS on the interface makes clustering classify the network as
  # client-facing. Repairing DNS "on all NICs" without this test stamps the DC
  # onto the fabric vNICs, which the reconcile loop then spends every pass
  # undoing — see the inherit rule in agent/reconcile ReconcileHost.
  $routed = @(Get-NetRoute -InterfaceIndex $n.ifIndex -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue).Count -gt 0
  if (-not $routed) {
    if (($cur.Count -eq 0) -and (-not $reg)) { continue }
    if ($cur.Count -gt 0) { try { Set-DnsClientServerAddress -InterfaceIndex $n.ifIndex -ResetServerAddresses -ErrorAction Stop } catch {} }
    if ($reg) { try { Set-DnsClient -InterfaceIndex $n.ifIndex -RegisterThisConnectionsAddress $false -ErrorAction SilentlyContinue } catch {} }
    $fixed += ($n.Name + ' (isolated: dns was ' + ($cur -join ',') + ' -> none, reg ' + $reg + '->False)')
    continue
  }
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
  $f = @(Get-ChildItem -Path $dir -Filter *.log -ErrorAction SilentlyContinue)[0]
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

// MoveVMStorage relocates a VM's files to folder — Hyper-V storage migration,
// which runs live: the VM keeps running while its disks are mirrored across and
// then switched over.
//
// Every destination path is dictated explicitly rather than handed to
// -DestinationStoragePath, which would let Hyper-V impose its own layout
// (<dest>\Virtual Hard Disks\...). Two reasons. It keeps a moved VM laid out the
// same way as one Ballast created or deployed, and — the load-bearing one — it
// makes the resulting paths predictable, so the centre can author them into
// desired state when the job succeeds instead of guessing where they ended up.
func (p *PowerShell) MoveVMStorage(ctx context.Context, vm, folder string, onProgress ProgressFunc) (string, error) {
	script := "$ErrorActionPreference='Stop'\n$vm = " + psQuote(vm) + "; $folder = " + psQuote(folder) + "\n" +
		clusteredVMRegisterPrelude("$vm") +
		`$state = [string]$__vm.State
if ($state -eq 'Saved') {
  throw ('VM ' + $vm + ' is Saved. Storage migration cannot move a saved VM, because its memory image is tied to where the files are. Discard the saved state (leaving it Off) or start it, then move.')
}
$disks = @(Get-VMHardDiskDrive -VMName $vm)
if ($disks.Count -eq 0) { throw ('VM ' + $vm + ' has no disks to move') }
if (-not (Test-Path -LiteralPath $folder)) { New-Item -ItemType Directory -Path $folder -Force | Out-Null }
# Test-Path is false both for absent and for unreadable, so prove the destination
# is reachable before a live migration starts writing gigabytes at it.
if (-not (Test-Path -LiteralPath $folder)) {
  throw ('the destination ' + $folder + ' is not reachable from this host (an offline CSV, or a volume owned by another node, reads exactly like a missing folder)')
}
$vhds = @()
foreach ($d in $disks) {
  $src = [string]$d.Path
  $target = Join-Path $folder (Split-Path -Leaf $src)
  if ($src -ieq $target) { continue }
  if (Test-Path -LiteralPath $target) { throw ('a disk already exists at ' + $target + '; move to a different datastore or clear it first') }
  $vhds += @{ SourceFilePath = $src; DestinationFilePath = $target }
}
if ($vhds.Count -eq 0) { 'DONE ' + $vm + ' is already on that datastore'; return }
# migrateWithProgress hands $path and $mt to the background job by position, so
# the move command inside it sees these names and not the ones used above.
$path = $folder
$mt = $vhds
` + migrateWithProgress("Move-VMStorage -VMName $vm -VirtualMachinePath $path -SnapshotFilePath $path -SmartPagingFilePath $path -Vhds $mt") + `
Write-Output ('DONE moved ' + $vm + ' storage to ' + $folder)
` + clusteredVMRestoreSuffix
	result := "moved " + vm + " storage to " + folder
	err := p.runStream(ctx, script, migrationLineHandler(onProgress, &result))
	if err != nil {
		return "", fmt.Errorf("move storage of %q to %q: %w", vm, folder, err)
	}
	return result, nil
}

// formatDiskScriptForTest exposes the disk-wipe script so its refusals can be
// asserted without a host. The guard it carries — that a disk which cannot be
// read is refused rather than skipped past — is not reachable any other way.
func formatDiskScriptForTest() string {
	return formatDiskScript("__test__")
}

// formatDiskScript is the script FormatDisk runs, factored out so it has exactly
// one definition rather than one for the agent and one for a test to drift from.
func formatDiskScript(id string) string {
	return fmt.Sprintf(formatDiskTemplate, psQuote(id))
}

const formatDiskTemplate = `$ErrorActionPreference='Stop'
$want = %[1]s
$pd = @(Get-PhysicalDisk -ErrorAction SilentlyContinue | Where-Object { [string]$_.UniqueId -eq $want })
if ($pd.Count -eq 0) {
  $pd = @(Get-PhysicalDisk -ErrorAction SilentlyContinue | Where-Object { [string]$_.DeviceId -eq $want })
}
if ($pd.Count -eq 0) { throw ('no physical disk with id ' + $want + ' on this host') }
if ($pd.Count -gt 1) {
  $seen = @($pd | ForEach-Object { [string]$_.UniqueId + ' (' + [string]$_.BusType + ', ' + [math]::Round($_.Size/1GB,1) + 'GB)' })
  throw ('id ' + $want + ' matches ' + $pd.Count + ' disks on this host, because a disk id is unique per bus and not per host: ' + ($seen -join '; ') + '. Refusing to erase one of them by guessing — identify the disk by its unique id instead.')
}
$pd = $pd[0]
$disk = @($pd | Get-Disk -ErrorAction SilentlyContinue)
if ($disk.Count -gt 1) { throw ('id ' + $want + ' resolves to more than one disk; refusing to erase by guessing') }
# A FAILED READ IS NOT AN ABSENT DISK.
#
# This was "if ($disk.Count -eq 1) { ...guard...; wipe }": when Get-Disk could not
# answer, Count was 0, the whole block was skipped -- INCLUDING the OS/boot guard
# -- and the script carried on to Reset-PhysicalDisk. The one check standing
# between this and erasing a boot disk was disabled by the read that was supposed
# to feed it, silently, with no error anywhere.
#
# So an unreadable disk is refused. A physical disk that resolves to no disk
# object is not a disk safely skipped past; it is a disk nothing is known about.
if ($disk.Count -eq 0) {
  # A POOLED disk has no Disk object BY DESIGN — Storage Spaces owns it, so there
  # is no partition or volume layer for Windows to present. That is the common
  # case here and it is not a read failure: saying "cannot be read" sent an
  # operator looking for a broken disk when the answer was "it is in a pool, take
  # it out first". The pool name is already known, so name it and name the step.
  $inPool = ''
  try {
    foreach ($sp in @(Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial })) {
      if (@($sp | Get-PhysicalDisk -ErrorAction SilentlyContinue | Where-Object { [string]$_.UniqueId -eq [string]$pd.UniqueId }).Count -gt 0) { $inPool = [string]$sp.FriendlyName; break }
    }
  } catch {}
  if ($inPool) {
    throw ('the disk with id ' + $want + ' is a member of storage pool "' + $inPool + '", so Windows presents no disk object for it and it cannot be formatted while the pool holds it. Remove it from the pool first (or rebuild the pool) - a pooled disk is owned by Storage Spaces, not by the host.')
  }
  throw ('the physical disk with id ' + $want + ' could not be resolved to a disk on this host, so whether it is the OS or boot disk cannot be established. Refusing to erase a disk that cannot be read.')
}
$d = $disk[0]
if ($d.IsBoot -or $d.IsSystem) { throw 'refusing to format the OS/boot disk' }
# Stop, not SilentlyContinue: if the disk cannot be brought online or made
# writable, the wipe below would run against a disk in a state nobody checked.
Set-Disk -Number $d.Number -IsReadOnly $false -ErrorAction Stop
Set-Disk -Number $d.Number -IsOffline $false -ErrorAction Stop
if ($d.PartitionStyle -ne 'RAW') { Clear-Disk -Number $d.Number -RemoveData -RemoveOEM -Confirm:$false -ErrorAction Stop }
Reset-PhysicalDisk -UniqueId $pd.UniqueId -ErrorAction SilentlyContinue
$after = @(Get-PhysicalDisk -ErrorAction SilentlyContinue | Where-Object { [string]$_.UniqueId -eq [string]$pd.UniqueId })
if ($after.Count -gt 0 -and $after[0].CanPool) { 'RESULT=WIPED' } else { 'RESULT=WIPED_NOPOOL' }`

/* unclusterScript takes a VM's HA role off the source cluster before it moves.

   Move-VM will not touch a VM the cluster owns, and the failure it gives for
   trying is about the VM being "clustered" rather than about what to do. Taking
   the role off leaves the VM registered and running on its current node — the
   cluster simply stops managing it — which is exactly the state a shared-nothing
   move needs and is also a state the VM survives if the move then fails.

   Idempotent: a VM with no cluster group is already in the right state, which
   matters because this runs again on every retry. */
func unclusterScript(vm, cluster string) string {
	if strings.TrimSpace(cluster) == "" {
		return ""
	}
	/* -RemoveResources, because a VM's group is never empty.

	   Without it the cluster refuses: "Group 'X' is not empty. Please use
	   -RemoveResources to remove this group and any resources it contains."
	   The resources in question are the Virtual Machine and Virtual Machine
	   Configuration cluster resources — the CLUSTERING of the VM, not the VM.
	   Removing them un-clusters it and leaves it registered and running on its
	   node, which is exactly what Failover Cluster Manager's "Remove role"
	   does and exactly the state a shared-nothing move needs.

	   The VM is checked for immediately afterwards. If that reading is ever
	   wrong the next line moves nothing and the pass would report success on a
	   VM that no longer exists, so it stops instead — loudly, and while the
	   files are still on the datastore. */
	return fmt.Sprintf(`$g = Get-ClusterGroup -Name %[1]s -ErrorAction SilentlyContinue
if ($g) {
  Write-Output ('PROGRESS taking ' + %[1]s + ' out of cluster ' + %[2]s + ' so it can be moved')
  Remove-ClusterGroup -Name %[1]s -RemoveResources -Force -ErrorAction Stop
  if (-not (Get-VM -Name %[1]s -ErrorAction SilentlyContinue)) {
    throw ('removing the cluster role for ' + %[1]s + ' also removed the VM from this host, which it must not do. ' +
      'Nothing further has been attempted; the VM files are still on the datastore and the VM can be re-registered from them')
  }
  Write-Output ('PROGRESS ' + %[1]s + ' is no longer a cluster role and is still on this host')
}
`, psQuote(vm), psQuote(cluster))
}

/* reclusterScript makes the VM an HA role on the destination cluster.

   Runs AFTER the move, against the destination — the VM is not here any more,
   so the cmdlet is aimed at a node of the target cluster. A failure here leaves
   a VM that moved successfully and is not highly available, which is worth
   saying plainly rather than failing the whole move: the copy is done and
   re-running it would move a VM that has already arrived. */
func reclusterScript(vm, cluster string) string {
	if strings.TrimSpace(cluster) == "" {
		return ""
	}
	return fmt.Sprintf(`try {
  Write-Output ('PROGRESS making ' + %[1]s + ' highly available on ' + %[2]s)
  Add-ClusterVirtualMachineRole -Cluster %[2]s -VirtualMachine %[1]s -ErrorAction Stop | Out-Null
} catch {
  Write-Warning ('the VM moved to ' + $dest + ' but could not be made highly available on ' + %[2]s + ': ' + $_.Exception.Message + '. It is running and unclustered; add the role in Failover Cluster Manager or re-run this move')
}
`, psQuote(vm), psQuote(cluster))
}

/*
restoreClusterScript puts a VM's HA role back when a move fails.

	The role has to come off before Move-VM will touch a clustered VM, and it is
	removed before the copy starts. So a move that fails leaves the VM where it
	was and NOT highly available — running, reachable, and quietly no longer
	protected. Nobody would notice until a node went down.

	Best effort, and it never masks the real failure: the original error is what
	the operator needs, and a problem re-adding the role is reported beside it
	rather than instead of it.
*/
func restoreClusterScript(vm, cluster string) string {
	if strings.TrimSpace(cluster) == "" {
		return ""
	}
	return fmt.Sprintf(`  if (Get-VM -Name %[1]s -ErrorAction SilentlyContinue) {
    try {
      Write-Output ('PROGRESS the move failed; putting ' + %[1]s + ' back into cluster ' + %[2]s)
      Add-ClusterVirtualMachineRole -Cluster %[2]s -VirtualMachine %[1]s -ErrorAction Stop | Out-Null
    } catch {
      Write-Warning ('the move failed AND ' + %[1]s + ' could not be put back into ' + %[2]s + ': ' + $_.Exception.Message + '. It is running on this host and is no longer highly available; add the role in Failover Cluster Manager')
    }
  }
`, psQuote(vm), psQuote(cluster))
}
