package hyperv

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
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
$pool = @(Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial })[0]
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
	script := csvScript(spec.Name, spec.SizeBytes, resiliency)
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
$sp = @(Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial })[0]
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
$sp = @(Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial })[0]
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
$sp = @(Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial })[0]
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

// csvScript is the CSV-provisioning script, built by a pure function so its
// guards are pinned by tests without a cluster — the same reason the maintenance
// and vNIC scripts are. The guards are the point of it: every one of them exists
// because New-Volume's own failure said nothing an operator could act on.
func csvScript(name string, sizeBytes uint64, resiliency string) string {
	return fmt.Sprintf(csvTemplate, psQuote(name), sizeBytes, psQuote(resiliency))
}

// csvScriptForTest exposes a representative script so the guards can be asserted.
func csvScriptForTest() string { return csvScript("Vol1", 214748364800, "Mirror") }

const csvTemplate = `
$ErrorActionPreference = 'Stop'
$name = %[1]s
function Rename-CsvMount($n) {
  # Failover Clustering auto-mounts a CSV at C:\ClusterStorage\VolumeN. Rename it to
  # the friendly name so it lands at a predictable path (where the host's default
  # VM/VHD path points). Best-effort.
  # @(...)[0], NOT "| Select-Object -First 1": the -First pipeline stop can
  # abort the whole script (exit 0, truncated output) after cluster cmdlets.
  $csv = @(Get-ClusterSharedVolume -ErrorAction SilentlyContinue | Where-Object { $_.Name -like ('*' + $n + '*') })[0]
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
$sp = @(Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial })[0]
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
# Capacity pre-check, against the FOOTPRINT rather than the logical size.
#
# This compared free space to the logical size and let anything smaller through,
# which is the arithmetic a mirror does not do: a two-way mirror writes two
# copies and a three-way writes three, so a 200GB three-way volume consumes
# 600GB of pool. The guard therefore passed volumes the pool could not hold and
# handed the operator New-Volume's "Not Supported" -- the exact message it exists
# to prevent. Its own comment admitted the gap ("more with mirror resiliency")
# without acting on it.
#
# The copy count comes from the pool's own resiliency setting, not a guess: ask
# for NumberOfDataCopies and fall back to the fault-domain count capped at 3,
# which is what S2D chooses for Mirror on a 3+ node cluster.
$want = [int64]%[2]d
$free = [int64]($sp.Size - $sp.AllocatedSize)
$copies = 1
try {
  $rs = Get-ResiliencySetting -StoragePool $sp -Name %[3]s -ErrorAction SilentlyContinue
  if ($rs -and $rs.NumberOfDataCopiesDefault -gt 0) { $copies = [int]$rs.NumberOfDataCopiesDefault }
} catch {}
if ($copies -le 1 -and %[3]s -eq 'Mirror') {
  $fd = @(Get-StorageFaultDomain -Type StorageScaleUnit -ErrorAction SilentlyContinue).Count
  if ($fd -ge 3) { $copies = 3 } elseif ($fd -ge 2) { $copies = 2 } else { $copies = 1 }
}
$need = [int64]($want * $copies)
if ($want -gt 0 -and $free -lt $need) {
  throw ('insufficient pool capacity for CSV "' + $name + '": ' + [math]::Round($free/1GB,1) + ' GB free, but a ' + [math]::Round($want/1GB,1) + ' GB volume at ' + $copies + '-copy ' + %[3]s + ' resiliency needs ' + [math]::Round($need/1GB,1) + ' GB of pool. Ask for a smaller volume, free space, or add disks.')
}
# Allocatable is not the same as free. A RETIRED disk is healthy, counted in the
# pool's size, and will take no new data - so a pool can report ample free space
# and still refuse a mirror for want of a third fault domain to put a copy in.
# That is the exact shape of "Not Supported", and it is worth saying before the
# cmdlet says nothing.
$retired = @(Get-PhysicalDisk -StoragePool $sp -ErrorAction SilentlyContinue | Where-Object { [string]$_.Usage -replace '-','' -eq 'Retired' })
if ($retired.Count -gt 0) {
  throw ('CSV "' + $name + '" cannot be created while ' + $retired.Count + ' disk(s) in pool "' + $sp.FriendlyName + '" are RETIRED. A retired disk is healthy but takes no new data, so the pool has fewer usable fault domains than it appears to and a mirror cannot be placed. Return them with: Get-PhysicalDisk | Where-Object Usage -eq Retired | Set-PhysicalDisk -Usage AutoSelect')
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
  $sp2 = @(Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial })[0]
  $free2 = if ($sp2) { [int64]($sp2.Size - $sp2.AllocatedSize) } else { [int64]0 }
  throw ('create CSV "' + $name + '" failed: ' + $_.Exception.Message + ' (pool free ' + [math]::Round($free2/1GB,1) + ' GB; a mirror volume needs about 2-3x its size free)')
}
Rename-CsvMount $name
[pscustomobject]@{ status = 'created' } | ConvertTo-Json -Compress
`

// DestroyS2D tears an S2D pool down for good: removes the cluster shared
// volumes, destroys the virtual disks, deletes the pool, and disables Storage
// Spaces Direct on the cluster. wipeDisks additionally resets the pool's
// physical disks to blank so they are raw and reusable.
//
// This is the step that has no undo, and it exists because the alternative was
// worse. Switching a cluster from S2D to an array leaves the old pool behind:
// it reports Unknown / Read-only for ever, the console cannot tell it from a
// failed one, and every disk stays claimed so nothing can reuse the capacity.
// The only way out was a PowerShell session on a member, which CLAUDE.md counts
// as a defect rather than a runbook step.
//
// It refuses while any volume still holds a VM. "Destroy the pool" and "delete
// the VMs on it" are not the same intention, and a caller who has not moved the
// workload off almost certainly means the first without the second.
func (p *PowerShell) DestroyS2D(ctx context.Context, wipeDisks bool) (string, error) {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Continue'
$log = @()
$wipe = $%[1]t
Import-Module FailoverClusters -ErrorAction SilentlyContinue

$cl = Get-Cluster -ErrorAction SilentlyContinue
if (-not $cl) { throw 'this host is not a cluster member, so there is no clustered S2D pool to destroy' }

# REFUSE while a VM still lives on the storage. Destroying the pool would take
# its disks with it, and "destroy the pool" is not "delete the VMs on it".
$onCSV = @()
foreach ($vm in @(Get-ClusterGroup -Cluster $cl.Name -ErrorAction SilentlyContinue | Where-Object { [string]$_.GroupType -eq 'VirtualMachine' })) {
  $onCSV += [string]$vm.Name
}
if ($onCSV.Count -gt 0) {
  throw ('refusing to destroy the pool: ' + $onCSV.Count + ' clustered VM role(s) still exist (' +
    (($onCSV | Sort-Object -Unique) -join ', ') + '). Move or delete them first - destroying the pool would take their disks with it.')
}

# 1. Cluster shared volumes come out of the cluster before their virtual disks
#    can be destroyed; a CSV still in Available Storage pins the disk.
foreach ($csv in @(Get-ClusterSharedVolume -Cluster $cl.Name -ErrorAction SilentlyContinue)) {
  try { Remove-ClusterSharedVolume -Cluster $cl.Name -Name ([string]$csv.Name) -ErrorAction Stop; $log += ('csv-' + [string]$csv.Name) }
  catch { $log += ('csv-err-' + ([string]$_.Exception.Message).Trim()) }
}
# The cluster disk resources the CSVs became, AND the pool's own cluster
# resource.
#
# Filtering to 'Physical Disk' missed the second one, and that is what made the
# removal fail: while the pool is still a clustered resource the cluster owns it,
# and Remove-StoragePool answers "Invalid Parameter" — an error that names
# nothing. Seen on the rig 2026-08-24, with the ClusterStoragePool group still
# Online after a teardown that reported having removed everything.
foreach ($r in @(Get-ClusterResource -Cluster $cl.Name -ErrorAction SilentlyContinue |
    Where-Object { [string]$_.ResourceType -eq 'Physical Disk' -or [string]$_.ResourceType -eq 'Storage Pool' })) {
  try { Remove-ClusterResource -Cluster $cl.Name -Name ([string]$r.Name) -Force -ErrorAction Stop; $log += ('res-' + [string]$r.Name) }
  catch { $log += ('res-err-' + [string]$r.ResourceType + '-' + ([string]$_.Exception.Message).Trim()) }
}

# 2. Virtual disks.
foreach ($vd in @(Get-VirtualDisk -ErrorAction SilentlyContinue)) {
  try { $vd | Remove-VirtualDisk -Confirm:$false -ErrorAction Stop; $log += ('vdisk-' + [string]$vd.FriendlyName) }
  catch { $log += ('vdisk-err-' + ([string]$_.Exception.Message).Trim()) }
}

# 3. The pool itself.
$pools = @(Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial })
$poolsAtStart = $pools.Count
foreach ($sp in $pools) {
  # A READ-ONLY pool refuses to be removed, and says "Invalid Parameter" rather
  # than "this pool is read-only". S2DCluster's sat at OperationalStatus
  # Read-only for a day reporting exactly that. Clearing it is not a
  # modification worth agonising over: the pool is about to be destroyed.
  if ([bool]$sp.IsReadOnly) {
    try { Set-StoragePool -InputObject $sp -IsReadOnly $false -ErrorAction Stop; $log += 'pool-writable' }
    catch { $log += ('pool-writable-err-' + ([string]$_.Exception.Message).Trim()) }
    $sp = @(Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { [string]$_.FriendlyName -eq [string]$sp.FriendlyName })[0]
  }
  try { Remove-StoragePool -InputObject $sp -Confirm:$false -ErrorAction Stop; $log += ('pool-' + [string]$sp.FriendlyName) }
  catch { $log += ('pool-err-' + ([string]$_.Exception.Message).Trim()) }
}
# Disable-ClusterS2D removes the pool itself on a healthy cluster, so a pool that
# survived the explicit removal often goes here. Try it before giving up rather
# than reporting a half-torn cluster the operator then has to finish by hand.
if (@(Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial }).Count -gt 0) {
  try { Disable-ClusterS2D -Cluster $cl.Name -Confirm:$false -ErrorAction Stop; $log += 'disabled-s2d-to-clear-pool' }
  catch { $log += ('disable-first-err-' + ([string]$_.Exception.Message).Trim()) }
}

# 4. Turn S2D off, so nothing reclaims the disks on the next pass. Slow on a
#    degraded pool, which is why it comes AFTER the pool is already gone.
try { Disable-ClusterS2D -Cluster $cl.Name -Confirm:$false -ErrorAction Stop; $log += 'disabled-s2d' }
catch { $log += ('disable-err-' + ([string]$_.Exception.Message).Trim()) }

# 5. Optionally return the disks to raw. Only THIS node's own disks: Get-PhysicalDisk
#    is cluster-wide in a Spaces context and wiping on that set could reach shared
#    storage belonging to something else.
$wiped = 0
if ($wipe) {
  $localSerials = @(Get-Disk -ErrorAction SilentlyContinue | Where-Object { $_.BusType -ne 'iSCSI' -and -not $_.IsBoot -and -not $_.IsSystem } | ForEach-Object { [string]$_.SerialNumber })
  foreach ($pd in @(Get-PhysicalDisk -ErrorAction SilentlyContinue | Where-Object { $localSerials -contains [string]$_.SerialNumber })) {
    try {
      Set-PhysicalDisk -InputObject $pd -Usage AutoSelect -ErrorAction SilentlyContinue
      Reset-PhysicalDisk -InputObject $pd -ErrorAction SilentlyContinue
      $d = Get-Disk -ErrorAction SilentlyContinue | Where-Object { [string]$_.SerialNumber -eq [string]$pd.SerialNumber }
      foreach ($x in @($d)) {
        if ([string]$x.PartitionStyle -ne 'RAW') { Clear-Disk -Number ([int]$x.Number) -RemoveData -RemoveOEM -Confirm:$false -ErrorAction Stop }
        $wiped++
      }
    } catch { $log += ('wipe-err-' + ([string]$_.Exception.Message).Trim()) }
  }
}

$left = @(Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial }).Count
'RESULT=' + (@{ poolsAtStart = $poolsAtStart; poolsLeft = $left; wiped = $wiped; log = $log } | ConvertTo-Json -Compress -Depth 3)`, wipeDisks)

	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("destroy s2d: %w", err)
	}
	var res struct {
		PoolsAtStart int      `json:"poolsAtStart"`
		PoolsLeft    int      `json:"poolsLeft"`
		Wiped        int      `json:"wiped"`
		Log          []string `json:"log"`
	}
	if err := json.Unmarshal([]byte(resultJSON(string(out))), &res); err != nil {
		return "", fmt.Errorf("destroy s2d: could not read the result: %w", err)
	}
	detail := strings.Join(res.Log, " | ")

	// NOTHING WAS THERE TO DESTROY is not success, and must never be reported as
	// it. A clustered pool is only enumerable from a node that can currently see
	// it, so running this on the wrong member finds no pool, removes nothing, and
	// — before this — returned "destroyed the S2D pool and its volumes" having
	// done absolutely nothing. On the rig 2026-08-23 that is exactly what
	// happened: two jobs reported success while the cluster went on reporting the
	// pool, with every step's error swallowed into a log nobody saw.
	//
	// A destructive action claiming an outcome it did not produce is the worst
	// version of this codebase's oldest defect, so it is now an error naming
	// where to run it instead.
	if res.PoolsAtStart == 0 {
		return "", fmt.Errorf("no storage pool is visible from this host, so nothing was destroyed. "+
			"A clustered pool can only be enumerated from a node that currently sees it — run this on the pool's owner node. "+
			"What this pass did try: %s", orNone(detail))
	}
	if res.PoolsLeft > 0 {
		return "", fmt.Errorf("the pool is STILL PRESENT after the teardown — treat the cluster as half torn down and look before retrying: %s", orNone(detail))
	}
	// Steps whose errors were tolerated still have to be surfaced: the pool going
	// does not mean every CSV and resource went with it cleanly.
	if strings.Contains(detail, "-err-") {
		return "", fmt.Errorf("the pool was removed but parts of the teardown failed, so the cluster may be left half tidy: %s", detail)
	}

	note := "destroyed the S2D pool and its volumes, and disabled Storage Spaces Direct on the cluster"
	if wipeDisks {
		if res.Wiped == 0 {
			note += "; NO disks were returned to raw — none of this host's local disks were in the pool, so run the wipe on the members that hold them"
		} else {
			note += "; " + strconv.Itoa(res.Wiped) + " local disks returned to raw"
		}
	} else {
		note += "; the disks were left claimed — use Reset pool disks on each member to return them to raw"
	}
	if detail != "" {
		note += ". Steps: " + detail
	}
	return note, nil
}

// orNone renders an empty step log as something readable rather than a dangling
// colon, so a message that explains nothing at least says that it explains
// nothing.
func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(the pass recorded no steps at all, which means it found nothing to act on)"
	}
	return s
}
