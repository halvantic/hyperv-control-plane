package hyperv

import (
	"context"
	"fmt"
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
$pool = (Get-StoragePool -ErrorAction SilentlyContinue | Where-Object { -not $_.IsPrimordial } | Select-Object -First 1).FriendlyName
if (-not $pool) { throw 'no Storage Spaces Direct pool found' }
New-Volume -StoragePoolFriendlyName $pool -FriendlyName %[1]s -FileSystem CSVFS_ReFS -Size %[2]d -ResiliencySettingName %[3]s | Out-Null
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
