package hyperv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/joshua-fourie/ballast/api/types"
)

/* Ballast — finding and importing VMs that are already on the storage.

   Adopting a LUN that has data on it brings the volume into the cluster and
   then says nothing whatever about the virtual machines sitting on it. An
   operator sees 800GB used and no VMs, and the only route to finding out what
   is there is a PowerShell session and a directory listing — which the brief
   calls a defect in Ballast, not a runbook step.

   This is the answer in two halves, deliberately separated by cost:

     SCAN     walks the storage roots for .vmcx configurations nothing has
              registered, reads each one, and runs Compare-VM to find out what
              would stop it importing HERE. Expensive, because the cost scales
              with somebody else's data, so it runs on demand rather than on
              every pass.
     IMPORT   registers one of them. A job, one VM at a time, with the two
              refusals that matter enforced at the point of action rather than
              only rendered in the console.

   Nothing here imports on its own. Every reconcile in Ballast is idempotent and
   re-applies for ever; an import is a one-time decision about somebody's data,
   and three of its failure modes destroy or corrupt that data if guessed. It
   does not belong in a loop. */

// importScanCap bounds the walk. A volume adopted from a busy cluster can hold
// hundreds of configurations and Compare-VM loads each one; past this the count
// is reported as Truncated rather than the scan silently costing the pass.
const importScanCap = 200

// importScanBudget bounds the whole scan the way iscsiStepBudget bounds the
// iSCSI step, and for the same reason: on Secondary a single step ate 3m43s of
// a 5m pass and everything after it never ran.
const importScanBudget = 120 * time.Second

/*
scanImportableVMsScript walks the given roots.

	Every identity here is READ, never constructed. The VM's name comes from the
	configuration, not from the folder — Hyper-V names a VM's folder at creation
	and never renames it afterwards, so the directory is a stale label. This
	repo has paid for constructed identifiers repeatedly (an adapter alias built
	from a vNIC name that resolved to a dead device; a target name rebuilt from a
	lower-cased echo), and a VM folder is the same trap with a bigger blast
	radius: importing under the wrong identity is not a failed cmdlet, it is a VM
	registered as something it is not.
*/
func scanImportableVMsScript(roots []string, cap int) string {
	var lit []string
	for _, r := range roots {
		lit = append(lit, "'"+strings.ReplaceAll(r, "'", "''")+"'")
	}
	return `
$ErrorActionPreference = 'Stop'
$roots = @(` + strings.Join(lit, ",") + `)
$cap = ` + fmt.Sprint(cap) + `
$out = @{ vms = @(); roots = @(); truncated = $false; message = '' }

# Configurations already registered on THIS host, by path and by id. A VM that
# is registered here is not importable here; listing it would invite an operator
# to import a VM they already have.
$mine = @{}
$myIds = @{}
foreach ($v in @(Get-VM -ErrorAction SilentlyContinue)) {
  $myIds[([string]$v.Id).ToLower()] = [string]$v.Name
  try {
    $cp = [string]$v.ConfigurationLocation
    if ($cp) { $mine[$cp.ToLower()] = $true }
  } catch {}
}

$found = @()
foreach ($root in $roots) {
  if (-not (Test-Path -LiteralPath $root)) { continue }
  $out.roots += $root
  try {
    # -Force so a hidden "Virtual Machines" folder is still walked; Hyper-V does
    # not hide it, but a volume that came from somewhere else may.
    $cfgs = @(Get-ChildItem -LiteralPath $root -Recurse -Force -Filter '*.vmcx' -ErrorAction SilentlyContinue)
  } catch { $cfgs = @() }
  foreach ($c in $cfgs) {
    if ($found.Count -ge $cap) { $out.truncated = $true; break }
    # Checkpoint and planned-VM configurations live alongside the real one. Only
    # the config directly in a "Virtual Machines" folder is the VM itself; the
    # rest would import as duplicates of their parent.
    if ($c.DirectoryName -notmatch 'Virtual Machines$') { continue }
    if ($mine.ContainsKey(([string]$c.DirectoryName).ToLower())) { continue }
    if ($mine.ContainsKey(([string](Split-Path $c.DirectoryName -Parent)).ToLower())) { continue }
    $found += $c
  }
  if ($out.truncated) { break }
}

foreach ($c in $found) {
  $rec = @{
    configPath = [string]$c.FullName
    volume     = ''
    name = ''; id = ''; generation = 0; processorCount = 0
    memoryStartupBytes = 0; sizeBytes = 0
    savedState = $false; registeredElsewhere = $false; registeredOn = ''
    inUseElsewhere = $false
    incompatibilities = @(); compatible = $false; compatKnown = $false; error = ''
  }
  foreach ($root in $out.roots) {
    if (([string]$c.FullName).ToLower().StartsWith(([string]$root).ToLower())) { $rec.volume = $root; break }
  }

  # Compare-VM against THIS host is the substance of the feature. It reads the
  # configuration and reports what would stop it running here — a switch that
  # does not exist, an ISO that is gone, processor features that differ — before
  # anything is imported, rather than as a failed Import-VM afterwards.
  $report = $null
  try {
    $report = Compare-VM -Path $c.FullName -ErrorAction Stop
  } catch {
    # A configuration that cannot even be read is still worth reporting. An
    # unreadable .vmcx is a fact about the storage; omitting the row would imply
    # the volume holds nothing, which is the absent-is-not-zero trap.
    $rec.error = ([string]$_.Exception.Message).Trim()
    $out.vms += $rec
    continue
  }

  $vm = $null
  try { $vm = $report.VM } catch {}
  if ($vm) {
    $rec.name = [string]$vm.Name
    $rec.id = ([string]$vm.Id)
    try { $rec.generation = [int]$vm.Generation } catch {}
    try { $rec.processorCount = [int]$vm.ProcessorCount } catch {}
    try { $rec.memoryStartupBytes = [int64]$vm.MemoryStartup } catch {}
    # A SAVED VM carries a memory image captured against the exact processor it
    # ran on. Restoring it on different silicon fails with a message naming
    # neither the cause nor the cure — a diagnosis Ballast can make here.
    try { $rec.savedState = ([string]$vm.State -eq 'Saved') } catch {}
    if ($myIds.ContainsKey(([string]$vm.Id).ToLower())) {
      $rec.registeredElsewhere = $true
      $rec.registeredOn = 'this host'
    }
  }

  # What the folder costs, so the console can price a COPY against registering
  # in place before an operator picks the wrong one.
  try {
    $dir = Split-Path $c.DirectoryName -Parent
    $rec.sizeBytes = [int64]((Get-ChildItem -LiteralPath $dir -Recurse -Force -File -ErrorAction SilentlyContinue |
      Measure-Object -Property Length -Sum).Sum)
  } catch {}

  # An open file means something outside this host is running it. Adopted
  # storage very often came from a cluster that is still up, and importing into
  # a live VM's files corrupts them.
  try {
    $vhds = @(Get-ChildItem -LiteralPath (Split-Path $c.DirectoryName -Parent) -Recurse -Force -File -Include '*.vhdx','*.vhd' -ErrorAction SilentlyContinue)
    foreach ($f in $vhds) {
      try { $h = [System.IO.File]::Open($f.FullName, 'Open', 'ReadWrite', 'None'); $h.Close() }
      catch { $rec.inUseElsewhere = $true; break }
    }
  } catch {}

  $rec.compatKnown = $true
  $inc = @()
  try { $inc = @($report.Incompatibilities) } catch {}
  foreach ($i in $inc) {
    $m = ''; $code = 0; $fix = $false
    try { $m = ([string]$i.Message).Trim() } catch {}
    try { $code = [int]$i.MessageId } catch {}
    # Compare-VM marks a finding resolvable by exposing the offending object on
    # the report; those are the ones an operator can act on before importing.
    try { $fix = ($null -ne $i.Source) } catch {}
    $rec.incompatibilities += @{ code = $code; message = $m; fixable = $fix }
  }
  $rec.compatible = ($rec.incompatibilities.Count -eq 0)
  $out.vms += $rec
}

$out | ConvertTo-Json -Depth 6 -Compress
`
}

// scanResult is the script's own shape.
type scanResult struct {
	VMs []struct {
		ConfigPath          string `json:"configPath"`
		Volume              string `json:"volume"`
		Name                string `json:"name"`
		ID                  string `json:"id"`
		Generation          int32  `json:"generation"`
		ProcessorCount      int32  `json:"processorCount"`
		MemoryStartupBytes  int64  `json:"memoryStartupBytes"`
		SizeBytes           int64  `json:"sizeBytes"`
		SavedState          bool   `json:"savedState"`
		RegisteredElsewhere bool   `json:"registeredElsewhere"`
		RegisteredOn        string `json:"registeredOn"`
		InUseElsewhere      bool   `json:"inUseElsewhere"`
		Compatible          bool   `json:"compatible"`
		CompatKnown         bool   `json:"compatKnown"`
		Error               string `json:"error"`
		Incompatibilities   []struct {
			Code    int32  `json:"code"`
			Message string `json:"message"`
			Fixable bool   `json:"fixable"`
		} `json:"incompatibilities"`
	} `json:"vms"`
	Roots     []string `json:"roots"`
	Truncated bool     `json:"truncated"`
	Message   string   `json:"message"`
}

// ScanImportableVMs walks the given storage roots and reports the VM
// configurations nothing has registered, with Compare-VM's verdict on each.
func (p *PowerShell) ScanImportableVMs(ctx context.Context, roots []string) ([]types.ImportableVM, *types.ImportScanStatus, error) {
	if len(roots) == 0 {
		return nil, &types.ImportScanStatus{
			ScannedAt: time.Now().UTC(),
			Message:   "no storage roots to scan: this host has no cluster shared volumes and no data volumes declared",
		}, nil
	}
	sctx, cancel := context.WithTimeout(ctx, importScanBudget)
	defer cancel()

	started := time.Now()
	raw, err := p.run(sctx, scanImportableVMsScript(roots, importScanCap))
	elapsed := time.Since(started).Milliseconds()
	if err != nil {
		// A scan that FAILED must never read as a scan that found nothing. The
		// failure goes into the status where the console renders it, rather than
		// leaving an empty list to be read as an empty volume.
		return nil, &types.ImportScanStatus{
			Roots:     roots,
			ScannedAt: time.Now().UTC(),
			ElapsedMs: elapsed,
			Message:   "the scan did not complete: " + err.Error(),
		}, nil
	}

	var res scanResult
	if jerr := json.Unmarshal(bytes.TrimSpace(raw), &res); jerr != nil {
		return nil, &types.ImportScanStatus{
			Roots:     roots,
			ScannedAt: time.Now().UTC(),
			ElapsedMs: elapsed,
			Message:   "the scan returned something this agent could not read: " + jerr.Error(),
		}, nil
	}

	out := make([]types.ImportableVM, 0, len(res.VMs))
	for _, v := range res.VMs {
		vm := types.ImportableVM{
			Name:                v.Name,
			ID:                  v.ID,
			ConfigPath:          v.ConfigPath,
			Volume:              v.Volume,
			Generation:          v.Generation,
			ProcessorCount:      v.ProcessorCount,
			MemoryStartupBytes:  v.MemoryStartupBytes,
			SizeBytes:           v.SizeBytes,
			SavedState:          v.SavedState,
			RegisteredElsewhere: v.RegisteredElsewhere,
			RegisteredOn:        v.RegisteredOn,
			InUseElsewhere:      v.InUseElsewhere,
			Compatible:          v.Compatible,
			CompatKnown:         v.CompatKnown,
			Error:               v.Error,
		}
		// A configuration with no readable name is still a real object on the
		// storage. Naming it by its folder is the least-wrong label available,
		// and it is only ever used for DISPLAY — the import takes the path.
		if vm.Name == "" && vm.ConfigPath != "" {
			vm.Name = filepath.Base(filepath.Dir(filepath.Dir(vm.ConfigPath)))
		}
		for _, i := range v.Incompatibilities {
			vm.Incompatibilities = append(vm.Incompatibilities, explainIncompatibility(i.Code, i.Message, i.Fixable))
		}
		out = append(out, vm)
	}

	st := &types.ImportScanStatus{
		Roots:     res.Roots,
		ScannedAt: time.Now().UTC(),
		ElapsedMs: elapsed,
		Truncated: res.Truncated,
		Message:   res.Message,
	}
	if len(st.Roots) == 0 {
		st.Roots = roots
	}
	if res.Truncated {
		st.Message = fmt.Sprintf("stopped after %d configurations, so there may be more on this storage than are listed", importScanCap)
	}
	return out, st, nil
}

/*
explainIncompatibility turns Compare-VM's code and sentence into something an

	operator can act on.

	This is the brief's rule about raw errors, applied where it is cheapest to
	apply. "Could not find Ethernet switch 'ConvergedSwitch'." tells someone the
	import would fail; it does not say that the fix is to create the switch or
	move the adapter, nor that Ballast can do the first of those from the centre.

	A code with no entry here keeps Windows' own message and offers no remedy.
	That is deliberate: inventing a remedy for a finding nobody has seen would be
	worse than silence, because a wrong remedy in an infra console gets followed.
*/
func explainIncompatibility(code int32, msg string, fixable bool) types.VMIncompatibility {
	c := types.VMIncompatibility{Code: code, Message: msg, Fixable: fixable, Kind: "Other"}
	lower := strings.ToLower(msg)
	switch {
	case code == 33012 || strings.Contains(lower, "ethernet switch"):
		c.Kind = "Switch"
		c.Remedy = "this VM's network adapter wants a virtual switch this host does not have. " +
			"Create the switch with the same name on this host, or import and then reconnect the adapter to an existing one."
	case code == 40010 || strings.Contains(lower, ".iso"):
		c.Kind = "ISO"
		c.Remedy = "the ISO this VM's DVD drive points at is not on this host. The VM imports and runs without it; " +
			"the drive comes in empty and can be pointed at the library afterwards."
	case strings.Contains(lower, "saved state") || strings.Contains(lower, "restore"):
		c.Kind = "SavedState"
		c.Remedy = "the saved state was captured on different hardware and cannot be restored here. " +
			"Discarding it starts the VM cold — anything unsaved inside the guest is lost, which is the same cost as pulling its power."
	case strings.Contains(lower, "processor") || strings.Contains(lower, "cpu"):
		c.Kind = "Processor"
		c.Remedy = "this host's processor does not offer a feature the VM was configured for. " +
			"Enabling processor compatibility on the VM lets it start here at the cost of the newer instructions."
	case strings.Contains(lower, "vhd") || strings.Contains(lower, "disk") || strings.Contains(lower, "path"):
		c.Kind = "Storage"
		c.Remedy = "a virtual disk this VM references was not found at the path recorded in its configuration. " +
			"Check the whole VM folder came across, not just the configuration."
	}
	return c
}
