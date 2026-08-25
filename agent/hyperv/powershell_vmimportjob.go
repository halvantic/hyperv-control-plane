package hyperv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

/* Importing one of the VMs the scan found.

   The scan reports; this acts. Everything dangerous about the feature lives
   here, and all of it is enforced in the SCRIPT rather than only in the
   console. A guard that exists only in the UI is not a guard: the job can be
   enqueued through the API, the console's view can be minutes stale, and the
   state that makes an import unsafe is exactly the state that changes while an
   operator is reading the page.

   Three refusals, in the order they are checked:

     1. THE VM IS ALREADY REGISTERED HERE. Importing a second copy of an ID
        this host already owns is not a duplicate, it is two claims on one set
        of VHDXs.
     2. ITS FILES ARE OPEN. Something outside this host is running it. Adopted
        storage often came from a cluster that is still up.
     3. COMPARE-VM SAYS IT CANNOT RUN HERE. Reported as the specific finding,
        not as "the import failed".

   Note what is NOT refused: a VM whose saved state cannot be restored. That is
   a real problem with a real cost, but discarding saved state is a decision an
   operator is entitled to make, and it is offered explicitly rather than being
   done quietly or blocked outright. */

// VMImport is one import request. The path is the identity; nothing here
// reconstructs it from a name.
type VMImport struct {
	// ConfigPath is the .vmcx, exactly as the scan reported it.
	ConfigPath string
	// Copy duplicates the files into this host's VM path and gives the copy a
	// new identity, instead of registering them where they already are.
	//
	// Wrong in both directions and expensively so. Registering in place when a
	// copy was wanted gives two clusters one set of files; copying when
	// registering was wanted writes the whole VM again onto the volume it is
	// already on, which on a 500GB VM is 500GB of the CSV an operator did not
	// ask to spend.
	Copy bool
	// Cluster adds the VM as a clustered role after importing, so it can fail
	// over. Only meaningful on a member, and refused with a plain reason on a
	// host that is not one.
	Cluster bool
	// DiscardSavedState imports a saved VM as though it had been shut down.
	// Explicit, because the cost is the guest's unsaved work — the same as
	// pulling its power — and no default is right for somebody else's VM.
	DiscardSavedState bool
}

/*
importVMScript registers one VM.

	Compare-VM runs FIRST and its report object is what Import-VM is given. This
	is not the same as importing by path and hoping: passing the report imports
	exactly what was inspected, and it is the only form that can carry a
	resolution (a reconnected adapter, a discarded saved state) with it.
*/
func importVMScript(v VMImport) string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	return `
$ErrorActionPreference = 'Stop'
$path = ` + q(v.ConfigPath) + `
$doCopy = $` + fmt.Sprint(v.Copy) + `
$doCluster = $` + fmt.Sprint(v.Cluster) + `
$discardSaved = $` + fmt.Sprint(v.DiscardSavedState) + `
$out = [ordered]@{ name = ''; id = ''; imported = $false; clustered = $false; note = '' }

if (-not (Test-Path -LiteralPath $path)) {
  throw "the configuration $path is not on this host's storage any more. It may have been imported already, or the volume may not be mounted here."
}

# Compare-VM before anything else. It is both the safety check and the object
# Import-VM is given, so what is inspected is exactly what is imported.
$report = Compare-VM -Path $path -ErrorAction Stop
$vm = $report.VM
$out.name = [string]$vm.Name
$out.id = [string]$vm.Id

# REFUSAL 1: already registered here. Checked against the live host, not against
# whatever the console last saw — the window between reading a page and clicking
# a button is exactly where this goes wrong.
$existing = @(Get-VM -ErrorAction SilentlyContinue | Where-Object { ([string]$_.Id) -eq ([string]$vm.Id) })
if ($existing.Count -gt 0) {
  throw ("this host already has " + $vm.Name + " registered (id " + $vm.Id + "). Importing it again would give two registrations one set of virtual disks, which corrupts them. If the intent is a second copy, import with Copy, which writes new files and a new identity.")
}

# REFUSAL 2: the files are open, so something else is running it.
$vmDir = Split-Path (Split-Path $path -Parent) -Parent
$busy = ''
foreach ($f in @(Get-ChildItem -LiteralPath $vmDir -Recurse -Force -File -Include '*.vhdx','*.vhd' -ErrorAction SilentlyContinue)) {
  try { $h = [System.IO.File]::Open($f.FullName, 'Open', 'ReadWrite', 'None'); $h.Close() }
  catch { $busy = $f.Name; break }
}
if ($busy -ne '' -and -not $doCopy) {
  throw ("a virtual disk in this VM's folder is open (" + $busy + "), which means another host is running it. Importing it here would give two hosts one set of disks. Shut the VM down where it is running, or remove that host's access to this storage, before importing.")
}

# REFUSAL 3: Compare-VM found something that stops it running here. Reported as
# the finding, not as "import failed" — the whole point of checking first.
if ($report.Incompatibilities.Count -gt 0) {
  $msgs = @()
  foreach ($i in $report.Incompatibilities) {
    $msgs += ('[' + [string]$i.MessageId + '] ' + ([string]$i.Message).Trim())
  }
  # Saved state is the one an operator can decide to spend, so it is offered
  # rather than refused — but only when they said so.
  $onlySaved = $true
  foreach ($i in $report.Incompatibilities) {
    if (([string]$i.Message) -notmatch 'saved state|restore') { $onlySaved = $false }
  }
  if (-not ($onlySaved -and $discardSaved)) {
    throw ("this VM cannot run on this host as configured: " + ($msgs -join ' | '))
  }
}

if ($discardSaved) {
  # Done through the report, so the import that follows carries the resolution.
  try { $report.VM | Remove-VMSavedState -ErrorAction SilentlyContinue } catch {}
}

if ($doCopy) {
  # Copy writes the whole VM again and gives it a new identity. Import-VM's own
  # -Copy takes the PATH, not the report.
  $new = Import-VM -Path $path -Copy -GenerateNewId -ErrorAction Stop
} else {
  # Register in place: the report carries the exact configuration inspected.
  $new = Import-VM -CompatibilityReport $report -ErrorAction Stop
}
$out.imported = $true
if ($new) { $out.name = [string]$new.Name; $out.id = [string]$new.Id }

if ($doCluster) {
  $svc = Get-Service -Name ClusSvc -ErrorAction SilentlyContinue
  if (-not $svc -or $svc.Status -ne 'Running') {
    $out.note = 'imported, but this host is not a cluster member, so no clustered role was added'
  } else {
    try {
      # By ID, not by name. Two VMs can carry the same name on one cluster —
      # importing somebody else's DC01 next to your own is precisely the case
      # this feature creates — and a name lookup would then add the role to
      # whichever one Windows found first.
      Add-ClusterVirtualMachineRole -VMId $out.id -ErrorAction Stop | Out-Null
      $out.clustered = $true
    } catch {
      # The VM IS imported at this point. Reporting the whole job as failed
      # would tell an operator to import again, which is refusal 1 waiting to
      # happen. The partial outcome is stated instead.
      $out.note = 'imported, but adding the clustered role failed: ' + ([string]$_.Exception.Message).Trim()
    }
  }
}

'RESULT=' + ($out | ConvertTo-Json -Compress -Depth 4)
`
}

// ImportVM registers a VM whose files are already on this host's storage.
func (p *PowerShell) ImportVM(ctx context.Context, v VMImport) (string, error) {
	if strings.TrimSpace(v.ConfigPath) == "" {
		return "", fmt.Errorf("import vm: no configuration path was given, and this job will not guess one")
	}
	raw, err := p.run(ctx, importVMScript(v))
	if err != nil {
		return "", fmt.Errorf("import %s: %w", v.ConfigPath, err)
	}
	var res struct {
		Name      string `json:"name"`
		ID        string `json:"id"`
		Imported  bool   `json:"imported"`
		Clustered bool   `json:"clustered"`
		Note      string `json:"note"`
	}
	body := bytes.TrimSpace(raw)
	if i := bytes.LastIndex(body, []byte("RESULT=")); i >= 0 {
		body = bytes.TrimSpace(body[i+len("RESULT="):])
	}
	if jerr := json.Unmarshal(body, &res); jerr != nil {
		// The import may well have SUCCEEDED and only the reply been unreadable.
		// Saying so is the difference between an operator checking and an
		// operator importing again into refusal 1.
		return "", fmt.Errorf("import %s: the host's reply could not be read (%v). Check whether the VM is now registered before importing again", v.ConfigPath, jerr)
	}
	if !res.Imported {
		return "", fmt.Errorf("import %s: the host did not report the VM as imported", v.ConfigPath)
	}
	name := res.Name
	if name == "" {
		name = v.ConfigPath
	}
	switch {
	case res.Note != "":
		return "imported " + name + " — " + res.Note, nil
	case res.Clustered:
		return "imported " + name + " and added it as a clustered role", nil
	case v.Copy:
		return "imported a copy of " + name + " as a new VM", nil
	default:
		return "imported " + name + ", registered in place", nil
	}
}
