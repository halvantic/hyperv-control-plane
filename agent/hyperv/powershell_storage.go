package hyperv

import (
	"context"
	"fmt"
	"strings"
)

type storageObservation struct {
	S2DEnabled bool     `json:"s2dEnabled"`
	S2DKnown   bool     `json:"s2dKnown"`
	Volumes    []string `json:"volumes"`
}

// storageStateScript reports whether S2D is enabled AND whether that state could
// be determined at all. Under heavy I/O (e.g. a large copy to a CSV) the
// Get-ClusterStorageSpacesDirect query can be starved and return nothing; treating
// that as "disabled" made the reconcile fire a spurious, heavy
// Enable-ClusterStorageSpacesDirect on an already-enabled cluster. So: the query
// is wrapped to distinguish failure (unknown) from a real 'Disabled', and the
// presence of any S2D virtual disk is taken as a reliable positive signal that
// S2D is on even when the state query itself does not answer.
const storageStateScript = `
$ErrorActionPreference = 'Stop'
$enabled = $false
$known = $false
try {
  $s2d = Get-ClusterStorageSpacesDirect -WarningAction SilentlyContinue -ErrorAction Stop 3>$null
  if ($s2d) { $known = $true; if ($s2d.State -eq 'Enabled') { $enabled = $true } }
} catch { $known = $false }
$vols = @((Get-VirtualDisk -ErrorAction SilentlyContinue).FriendlyName)
if ($vols.Count -gt 0) { $enabled = $true; $known = $true }
[pscustomobject]@{ s2dEnabled = $enabled; s2dKnown = $known; volumes = @($vols) } | ConvertTo-Json -Compress
`

func (p *PowerShell) GetStorageState(ctx context.Context) (StorageState, error) {
	out, err := p.run(ctx, storageStateScript)
	if err != nil {
		return StorageState{}, fmt.Errorf("get storage state: %w", err)
	}
	var obs storageObservation
	if err := decodeJSON(out, &obs); err != nil {
		return StorageState{}, fmt.Errorf("get storage state: %w", err)
	}
	return StorageState{S2DEnabled: obs.S2DEnabled, S2DKnown: obs.S2DKnown, Volumes: obs.Volumes}, nil
}

func (p *PowerShell) EnableS2D(ctx context.Context) (Outcome, error) {
	if err := p.run2(ctx, "$ErrorActionPreference='Stop'; Enable-ClusterStorageSpacesDirect -Confirm:$false | Out-Null"); err != nil {
		return OutcomeUnchanged, fmt.Errorf("enable S2D: %w", err)
	}
	return OutcomeCreated, nil
}

// EnsureS2DPoolDisks adds any poolable disks across the cluster to the S2D pool.
// Add-ClusterNode joins a node to the cluster but does not claim its disks, so a
// node added after S2D was enabled keeps its disks CanPool and contributes no
// capacity until they are explicitly added. Idempotent: a no-op when nothing is
// poolable.
//
// It also bootstraps a MISSING pool: Enable-ClusterStorageSpacesDirect on a
// cluster with no eligible disks completes with only a warning, reports state
// Enabled, and creates no pool — and nothing else ever creates it, so CSV
// provisioning fails forever. When S2D is Enabled, no pool exists, and poolable
// disks are now visible, cycle S2D (disable + enable). This is safe by
// construction: no pool means no data to destroy, and re-enabling claims the
// disks and creates the pool the sanctioned way.
func (p *PowerShell) EnsureS2DPoolDisks(ctx context.Context) (Outcome, error) {
	script := `
$ErrorActionPreference = 'Stop'
$pool = Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial } | Select-Object -First 1
$claim = @(Get-PhysicalDisk -CanPool $true -ErrorAction SilentlyContinue)
if (-not $pool) {
  # The caller only runs this when S2D is enabled, so no pool means Enable ran
  # while no disks were eligible and created nothing. If poolable disks exist
  # now, cycle S2D so enable re-claims them and creates the pool (safe: no pool
  # means no data). A failure here must surface, not be swallowed — a silent
  # skip leaves CSV provisioning failing forever with no visible cause.
  if ($claim.Count -eq 0) { [pscustomobject]@{ changed = $false } | ConvertTo-Json -Compress; return }
  try {
    Disable-ClusterStorageSpacesDirect -Confirm:$false -WarningAction SilentlyContinue 3>$null | Out-Null
  } catch {
    # Enabled-but-poolless S2D can refuse the cmdlet's own cleanup (HRESULT
    # 0x80070001 setting FaultDomainAwarenessDefault on the subsystem) because
    # a poolless enable never established that state. The only thing to undo
    # here is the cluster's S2DEnabled flag; the property is read-only through
    # the provider, so clear it in the live cluster hive (the documented
    # workaround for a wedged disable) and let the fresh Enable below rebuild
    # everything the proper way.
    Set-ItemProperty -Path 'HKLM:\Cluster' -Name S2DEnabled -Value 0
    Start-Sleep -Seconds 5
  }
  # On virtual hardware, enable with the cache disabled: S2D's cache tier needs
  # real NVMe/SSD device hierarchy, and enabling it in a nested/VM lab fails
  # partway (flag set, no pool created) — the exact wedge this path recovers.
  $isVM = (Get-CimInstance Win32_ComputerSystem).Model -match 'Virtual|VMware'
  if ($isVM) {
    Enable-ClusterStorageSpacesDirect -CacheState Disabled -Confirm:$false -WarningAction SilentlyContinue 3>$null | Out-Null
  } else {
    Enable-ClusterStorageSpacesDirect -Confirm:$false -WarningAction SilentlyContinue 3>$null | Out-Null
  }
  [pscustomobject]@{ changed = $true; bootstrapped = $true } | ConvertTo-Json -Compress; return
}
if (-not $claim -or $claim.Count -eq 0) { [pscustomobject]@{ changed = $false } | ConvertTo-Json -Compress; return }
Add-PhysicalDisk -StoragePoolFriendlyName $pool.FriendlyName -PhysicalDisks $claim
[pscustomobject]@{ changed = $true; added = $claim.Count } | ConvertTo-Json -Compress
`
	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure S2D pool disks: %w", err)
	}
	var res struct {
		Changed bool `json:"changed"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure S2D pool disks: %w", err)
	}
	if res.Changed {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
}

// EnsureCSV creates the CSV if absent. It locates the S2D pool (the single
// non-primordial storage pool) and creates a CSVFS_ReFS volume on it, which
// Failover Clustering automatically presents as a Cluster Shared Volume.
func (p *PowerShell) EnsureCSV(ctx context.Context, spec CSVProvision) (Outcome, error) {
	resiliency := spec.ResiliencyType
	if resiliency == "" {
		resiliency = "Mirror"
	}
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$name = %[1]s
function Rename-CsvMount($n) {
  # Failover Clustering auto-mounts a CSV at C:\ClusterStorage\VolumeN. Rename it to
  # the friendly name so it lands at a predictable path (where the host's default
  # VM/VHD path points). Best-effort.
  $csv = Get-ClusterSharedVolume -ErrorAction SilentlyContinue | Where-Object { $_.Name -like ('*' + $n + '*') } | Select-Object -First 1
  if ($csv) {
    $cur = $csv.SharedVolumeInfo.FriendlyVolumeName
    $wantPath = 'C:\ClusterStorage\' + $n
    if ($cur -and $cur -ne $wantPath -and -not (Test-Path $wantPath)) { try { Rename-Item -Path $cur -NewName $n -ErrorAction Stop } catch {} }
  }
}
$existing = Get-VirtualDisk -FriendlyName $name -ErrorAction SilentlyContinue
if ($existing) {
  Rename-CsvMount $name
  # A volume can be present but still materialising on S2D (allocation/resync). Only
  # call it ready when Healthy; otherwise report provisioning so the reconcile shows
  # progress rather than a spurious failure.
  if ($existing.HealthStatus -eq 'Healthy') { [pscustomobject]@{ status = 'ready' } | ConvertTo-Json -Compress; return }
  [pscustomobject]@{ status = 'provisioning'; detail = ('volume materialising (' + [string]$existing.HealthStatus + '/' + (@($existing.OperationalStatus) -join ',') + ')') } | ConvertTo-Json -Compress; return
}
$sp = Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial } | Select-Object -First 1
if (-not $sp) {
  $n = @(Get-PhysicalDisk -CanPool $true -ErrorAction SilentlyContinue).Count
  throw ('no S2D pool exists yet (' + $n + ' poolable disk(s) visible). S2D was likely enabled while no disks were eligible; the pool is bootstrapped automatically once poolable disks appear - retries next pass.')
}
# A resilient volume cannot be created on a pool that is not Healthy; New-Volume
# would otherwise fail with an opaque "Not Supported". Surface the real cause.
$bad = @(Get-PhysicalDisk -StoragePool $sp -ErrorAction SilentlyContinue | Where-Object { $_.HealthStatus -ne 'Healthy' })
if ($sp.HealthStatus -ne 'Healthy' -or $bad.Count -gt 0) {
  throw ('S2D pool "' + $sp.FriendlyName + '" is ' + $sp.HealthStatus + '/' + ($sp.OperationalStatus -join ',') + ' with ' + $bad.Count + ' unhealthy disk(s); a CSV cannot be created until the pool is Healthy. Retire/replace the unhealthy disks (Repair pool) and retry.')
}
# Capacity pre-check: a mirror volume needs roughly 2-3x its logical size of free
# pool space. Refuse early with a clear message rather than New-Volume's opaque
# "Not Supported" when the pool plainly cannot hold it.
$want = [int64]%[2]d
$free = [int64]($sp.Size - $sp.AllocatedSize)
if ($want -gt 0 -and $free -lt $want) {
  throw ('insufficient pool capacity for CSV "' + $name + '": ' + [math]::Round($free/1GB,1) + ' GB free, volume needs ' + [math]::Round($want/1GB,1) + ' GB (more with mirror resiliency). Free space or add disks, then retry.')
}
try {
  New-Volume -StoragePoolFriendlyName $sp.FriendlyName -FriendlyName $name -FileSystem CSVFS_ReFS -Size $want -ResiliencySettingName %[3]s -ErrorAction Stop | Out-Null
} catch {
  # S2D volume creation is slow; a retry can race an in-flight creation and fail
  # opaquely while the volume is actually appearing — treat that as provisioning,
  # not a failure. Otherwise surface the real cause with the pool's current free
  # space, which is usually the true constraint behind "Not Supported".
  $now = Get-VirtualDisk -FriendlyName $name -ErrorAction SilentlyContinue
  if ($now) { [pscustomobject]@{ status = 'provisioning'; detail = 'creation in progress' } | ConvertTo-Json -Compress; return }
  $sp2 = Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial } | Select-Object -First 1
  $free2 = if ($sp2) { [int64]($sp2.Size - $sp2.AllocatedSize) } else { [int64]0 }
  throw ('create CSV "' + $name + '" failed: ' + $_.Exception.Message + ' (pool free ' + [math]::Round($free2/1GB,1) + ' GB; a mirror volume needs about 2-3x its size free)')
}
Rename-CsvMount $name
[pscustomobject]@{ status = 'created' } | ConvertTo-Json -Compress
`, psQuote(spec.Name), spec.SizeBytes, psQuote(resiliency))

	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure CSV %q: %w", spec.Name, err)
	}
	var res struct {
		Status string `json:"status"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure CSV %q: %w", spec.Name, err)
	}
	switch res.Status {
	case "created":
		return OutcomeCreated, nil
	case "provisioning":
		// Still materialising on S2D — not settled, but not a failure. Reported as
		// progress so it never surfaces as ApplyFailed during a normal creation.
		return OutcomeUpdated, nil
	default: // "ready"
		return OutcomeUnchanged, nil
	}
}

// RemoveCSV deletes the Cluster Shared Volume backed by the virtual disk of the
// given name. On an S2D cluster Remove-VirtualDisk also removes the associated
// cluster physical-disk resource, taking the CSV offline and out of the cluster.
// A no-op (no error) when no virtual disk of that name exists, so the job is
// idempotent and safe to retry.
func (p *PowerShell) RemoveCSV(ctx context.Context, name string) error {
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$vd = Get-VirtualDisk -FriendlyName %[1]s -ErrorAction SilentlyContinue
if (-not $vd) { 'RESULT=NOOP'; return }
$vd | Remove-VirtualDisk -Confirm:$false
'RESULT=REMOVED'
`, psQuote(name))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("remove CSV %q: %w", name, err)
	}
	return nil
}

// RepairStoragePool returns a degraded S2D pool to Healthy by retiring and
// removing the physical disks that are no longer Healthy (lost communication,
// transient error, failed) — e.g. a departed node's disks orphaned after a
// cluster teardown. It reports how many disks it removed. Idempotent: a no-op
// (no error) when every disk is already Healthy.
func (p *PowerShell) RepairStoragePool(ctx context.Context) (string, error) {
	script := `
$ErrorActionPreference = 'Stop'
$sp = Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial } | Select-Object -First 1
if (-not $sp) { throw 'no Storage Spaces Direct pool found' }
$bad = @(Get-PhysicalDisk -StoragePool $sp -ErrorAction SilentlyContinue | Where-Object { $_.HealthStatus -ne 'Healthy' })
if ($bad.Count -eq 0) { 'RESULT=NOOP pool ' + $sp.FriendlyName + ' is ' + $sp.HealthStatus; return }
# A departed node's disks stall in "Removing From Pool" because S2D tries to drain
# data off disks that are gone. Tell the pool to actively retire missing disks so
# the removal completes; then retire and remove each unhealthy disk.
try { Set-StoragePool -FriendlyName $sp.FriendlyName -RetireMissingPhysicalDisks Always -ErrorAction SilentlyContinue } catch {}
$bad | Set-PhysicalDisk -Usage Retired -ErrorAction SilentlyContinue
$removed = 0; $errs = @()
foreach ($d in $bad) {
  try { Remove-PhysicalDisk -PhysicalDisks $d -StoragePoolFriendlyName $sp.FriendlyName -Confirm:$false -ErrorAction Stop; $removed++ }
  catch { $errs += ('SN ' + $d.SerialNumber + ': ' + $_.Exception.Message) }
}
if ($removed -eq 0 -and $errs.Count -gt 0) { throw ('could not remove any of ' + $bad.Count + ' unhealthy disk(s): ' + ($errs -join ' | ')) }
'RESULT=REPAIRED removed ' + $removed + ' of ' + $bad.Count + ' unhealthy disk(s) from ' + $sp.FriendlyName
`
	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("repair storage pool: %w", err)
	}
	msg := strings.TrimSpace(string(out))
	if i := strings.Index(msg, "RESULT="); i >= 0 {
		msg = strings.TrimSpace(msg[i+len("RESULT="):])
	}
	return msg, nil
}

// RebuildStoragePool is the heavy remediation for a pool that Repair cannot
// salvage — a stale, degraded pool left over from a torn-down cluster, holding
// orphaned (departed-node) disks and inaccessible volumes it will not shed. It
// DESTROYS the pool and every volume on it, then re-enables Storage Spaces Direct
// so a fresh, healthy pool is created from the cluster's current disks, named
// after the current cluster. Destructive — all data on the pool is lost — and
// gated behind an explicit operator action. Runs on a cluster member.
func (p *PowerShell) RebuildStoragePool(ctx context.Context) (string, error) {
	// This does NOT Disable/Enable S2D — that is a heavy cluster operation that
	// hangs for many minutes on a degraded pool (exactly the case this fixes).
	// Instead: destroy the pool's volumes (so nothing pins it), then retire and
	// remove the unhealthy/orphaned disks (which succeeds once the pool holds no
	// data), then rename the pool to match the current cluster. Fast and safe.
	script := `
$ErrorActionPreference = 'Continue'
$log = @()
$sp = Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial } | Select-Object -First 1
if (-not $sp) { throw 'no Storage Spaces Direct pool found' }
# 1. Destroy every virtual disk / CSV so nothing pins the pool's resiliency.
foreach ($vd in @(Get-VirtualDisk -ErrorAction SilentlyContinue)) {
  try { $vd | Remove-VirtualDisk -Confirm:$false -ErrorAction Stop; $log += ('vdisk-' + $vd.FriendlyName) } catch { $log += ('vdisk-err-' + $_.Exception.Message) }
}
# 2. Retire and remove every unhealthy disk. With no volumes left there is no data
#    to preserve, so removing a departed node's orphaned disks now succeeds.
try { Set-StoragePool -FriendlyName $sp.FriendlyName -RetireMissingPhysicalDisks Always -ErrorAction SilentlyContinue } catch {}
$bad = @(Get-PhysicalDisk -StoragePool $sp -ErrorAction SilentlyContinue | Where-Object { $_.HealthStatus -ne 'Healthy' })
$bad | Set-PhysicalDisk -Usage Retired -ErrorAction SilentlyContinue
$removed = 0
foreach ($d in $bad) {
  try { Remove-PhysicalDisk -PhysicalDisks $d -StoragePoolFriendlyName $sp.FriendlyName -Confirm:$false -ErrorAction Stop; $removed++ } catch { $log += ('rm-err-' + $_.Exception.Message) }
}
# 3. Rename the pool to match the current cluster (it was left over from another).
$cn = (Get-Cluster -ErrorAction SilentlyContinue).Name
if ($cn) { $want = 'S2D on ' + $cn; if ($sp.FriendlyName -ne $want) { try { Set-StoragePool -FriendlyName $sp.FriendlyName -NewFriendlyName $want -ErrorAction Stop; $log += ('renamed-' + $want) } catch { $log += ('rename-err-' + $_.Exception.Message) } } }
$sp = Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial } | Select-Object -First 1
'RESULT=REBUILT ' + $sp.FriendlyName + ' ' + $sp.HealthStatus + ' removed=' + $removed + '/' + $bad.Count + ' :: ' + ($log -join ' | ')
`
	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("rebuild storage pool: %w", err)
	}
	msg := strings.TrimSpace(string(out))
	if i := strings.Index(msg, "RESULT="); i >= 0 {
		msg = strings.TrimSpace(msg[i+len("RESULT="):])
	}
	return msg, nil
}
