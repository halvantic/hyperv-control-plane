package hyperv

import (
	"context"
	"fmt"
	"strings"
)

type storageObservation struct {
	S2DEnabled bool     `json:"s2dEnabled"`
	Volumes    []string `json:"volumes"`
}

const storageStateScript = `
$ErrorActionPreference = 'Stop'
$enabled = $false
$s2d = Get-ClusterStorageSpacesDirect -WarningAction SilentlyContinue -ErrorAction SilentlyContinue 3>$null
if ($s2d -and $s2d.State -eq 'Enabled') { $enabled = $true }
$vols = @()
if ($enabled) { $vols = @((Get-VirtualDisk -ErrorAction SilentlyContinue).FriendlyName) }
[pscustomobject]@{ s2dEnabled = $enabled; volumes = @($vols) } | ConvertTo-Json -Compress
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
	return StorageState{S2DEnabled: obs.S2DEnabled, Volumes: obs.Volumes}, nil
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
func (p *PowerShell) EnsureS2DPoolDisks(ctx context.Context) (Outcome, error) {
	script := `
$ErrorActionPreference = 'Stop'
$pool = Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial } | Select-Object -First 1
if (-not $pool) { [pscustomobject]@{ changed = $false } | ConvertTo-Json -Compress; return }
$claim = @(Get-PhysicalDisk -CanPool $true -ErrorAction SilentlyContinue)
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
$existing = Get-VirtualDisk -FriendlyName %[1]s -ErrorAction SilentlyContinue
if ($existing) { [pscustomobject]@{ changed = $false } | ConvertTo-Json -Compress; return }
$sp = Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial } | Select-Object -First 1
if (-not $sp) { throw 'no Storage Spaces Direct pool found' }
# A resilient volume cannot be created on a pool that is not Healthy; New-Volume
# would otherwise fail with an opaque "Not Supported". Surface the real cause —
# the pool health and how many disks are unhealthy — so the operator can retire
# or replace them (or run the pool-repair job) before retrying.
$bad = @(Get-PhysicalDisk -StoragePool $sp -ErrorAction SilentlyContinue | Where-Object { $_.HealthStatus -ne 'Healthy' })
if ($sp.HealthStatus -ne 'Healthy' -or $bad.Count -gt 0) {
  throw ('S2D pool "' + $sp.FriendlyName + '" is ' + $sp.HealthStatus + '/' + ($sp.OperationalStatus -join ',') + ' with ' + $bad.Count + ' unhealthy disk(s); a CSV cannot be created until the pool is Healthy. Retire/replace the unhealthy disks (Repair pool) and retry.')
}
New-Volume -StoragePoolFriendlyName $sp.FriendlyName -FriendlyName %[1]s -FileSystem CSVFS_ReFS -Size %[2]d -ResiliencySettingName %[3]s | Out-Null
[pscustomobject]@{ changed = $true } | ConvertTo-Json -Compress
`, psQuote(spec.Name), spec.SizeBytes, psQuote(resiliency))

	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure CSV %q: %w", spec.Name, err)
	}
	var res struct {
		Changed bool `json:"changed"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure CSV %q: %w", spec.Name, err)
	}
	if res.Changed {
		return OutcomeCreated, nil
	}
	return OutcomeUnchanged, nil
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
# Retire so Storage Spaces stops placing data on them, then remove them from the
# pool; the pool rebalances/repairs onto the remaining healthy disks.
$bad | Set-PhysicalDisk -Usage Retired -ErrorAction SilentlyContinue
$removed = 0
foreach ($d in $bad) {
  try { Remove-PhysicalDisk -PhysicalDisks $d -StoragePoolFriendlyName $sp.FriendlyName -Confirm:$false -ErrorAction Stop; $removed++ } catch {}
}
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
