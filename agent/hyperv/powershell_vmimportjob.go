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
	// ApplyFixes resolves the findings Compare-VM reported that CAN be resolved
	// — disconnecting an adapter whose switch is absent, emptying a DVD drive
	// whose image is gone, removing a reference to a virtual disk that is not
	// here. Compare-VM's report is a work list, not a verdict: each finding
	// carries the offending object, and resolving them on the report is the
	// documented way to import a VM that does not fit the host as it stands.
	//
	// Explicit, because two of those change what the VM IS. Removing a disk
	// reference is why the console says such a VM imports without booting
	// rather than calling it harmless.
	ApplyFixes bool
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
$fix = $` + fmt.Sprint(v.ApplyFixes) + `
$out = [ordered]@{ name = ''; id = ''; imported = $false; clustered = $false; note = ''; fixed = @() }

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
  Discard-BallastPlanned
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
  Discard-BallastPlanned
  throw ("a virtual disk in this VM's folder is open (" + $busy + "), which means another host is running it. Importing it here would give two hosts one set of disks. Shut the VM down where it is running, or remove that host's access to this storage, before importing.")
}

# DISCARD THE PLANNED VM ON EVERY REFUSAL.
#
# Compare-VM does not merely inspect: it registers a PLANNED VM under
# C:\ProgramData\Microsoft\Windows\Hyper-V\Planned Virtual Machines and hands
# back a report bound to it. Import-VM consumes it. Throwing without consuming
# it leaves it registered, holding the configuration open — and the next
# Compare-VM on the same files then fails with "the process cannot access the
# file because it is being used by another process", which reads as another
# host running the VM when it is this host's own leftover.
#
# Seen on the rig 2026-08-26: TestPillVM reported exactly that after an earlier
# refused import of the same volume.
function Discard-BallastPlanned {
  try { if ($report -and $report.VM) { Remove-VM -VM $report.VM -Force -ErrorAction SilentlyContinue | Out-Null } } catch {}
}

# THE FINDINGS. Compare-VM's report is not a verdict, it is a WORK LIST: each
# incompatibility carries the offending object on $i.Source, and resolving them
# on the report is the documented way to import a VM that does not fit the host
# as it stands. Refusing outright was wrong — the console told the operator a
# missing switch could be reconnected afterwards, left the row selectable, and
# then the job refused the thing it had just offered.
#
# Nothing is resolved without being asked for. $fix is the operator ticking
# "import anyway", having been shown exactly what each finding costs.
$fixed = @()
$unresolved = @()
foreach ($i in @($report.Incompatibilities)) {
  $m = ([string]$i.Message).Trim()
  $id = [string]$i.MessageId
  $src = $null
  try { $src = $i.Source } catch {}

  # SAVED STATE. Its own consent, because the cost is the guest's unsaved work
  # rather than a configuration change.
  if ($m -match 'saved state|restore') {
    if ($discardSaved) {
      try { $report.VM | Remove-VMSavedState -ErrorAction Stop; $fixed += 'discarded the saved state' }
      catch { $unresolved += ('[' + $id + '] ' + $m) }
    } else { $unresolved += ('[' + $id + '] ' + $m) }
    continue
  }

  if (-not $fix) {
    $unresolved += ('[' + $id + '] ' + $m + ' - this can be resolved on import, but that was not asked for.')
    continue
  }

  $type = ''
  if ($src) { try { $type = [string]$src.GetType().Name } catch {} }
  try {
    # MATCHED ON THE TYPE THE REPORT HANDS BACK, not on a name built from the
    # cmdlet noun.
    #
    # The first version tested 'VMHardDiskDrive' and 'VMDvdDrive', invented by
    # prefixing VM to Remove-VMHardDiskDrive and Set-VMDvdDrive. Hyper-V's
    # actual types are Microsoft.HyperV.PowerShell.HardDiskDrive and .DvdDrive —
    # only the network adapter really is VMNetworkAdapter. So on the rig the
    # switch finding resolved and the missing disk fell to default, and the
    # import refused a VM the operator had just consented to fix.
    #
    # Constructing an identifier instead of reading one, again. The patterns are
    # now the part both spellings share.
    switch -Regex ($type) {
      'NetworkAdapter' {
        # The switch does not exist here. Disconnecting leaves the adapter on
        # the VM with nothing attached, which is exactly what the console said
        # would happen and what "reconnect it afterwards" means.
        Disconnect-VMNetworkAdapter -VMNetworkAdapter $src -ErrorAction Stop
        $fixed += ('disconnected the network adapter that wanted ' + $m)
      }
      'DvdDrive' {
        # A missing ISO. The drive stays and comes in empty.
        Set-VMDvdDrive -VMDvdDrive $src -Path $null -ErrorAction Stop
        $fixed += 'emptied the DVD drive whose image is not on this host'
      }
      'HardDiskDrive' {
        # A missing VHDX. Removing the reference is the only way the VM can be
        # registered at all, and it is why the console says the VM will import
        # WITHOUT BOOTING rather than calling this harmless.
        Remove-VMHardDiskDrive -VMHardDiskDrive $src -ErrorAction Stop
        $fixed += ('removed the reference to a virtual disk that is not on this host (' + $m + ')')
      }
      default {
        # Name the type. A refusal that says only "cannot run as configured"
        # after the operator ticked "apply these changes" does not say whether
        # the fix was refused, attempted, or never understood — and the type is
        # the one fact that tells whoever reads this what to add here.
        $seen = 'no object'
        if ($type) { $seen = 'a ' + $type }
        $unresolved += ('[' + $id + '] ' + $m + ' - Ballast does not know how to resolve this: the report offered ' + $seen + '.')
      }
    }
  } catch {
    $unresolved += ('[' + $id + '] ' + $m + ' — could not be resolved: ' + ([string]$_.Exception.Message).Trim())
  }
}

if ($unresolved.Count -gt 0) {
  # Say what DID get resolved alongside what did not. Listing only the failure
  # made a partial success read as nothing having happened, and on the rig it
  # hid the fact that the network fix had worked and only the disk had not.
  $head = 'this VM cannot run on this host as configured'
  if ($fixed.Count -gt 0) {
    $head = ('resolved ' + ($fixed -join '; ') + ', but this VM still cannot run on this host')
  }
  Discard-BallastPlanned
  throw ($head + ': ' + ($unresolved -join ' | '))
}

# Re-check. Resolving one finding can expose another, and importing a report
# that still carries incompatibilities fails with a message far less useful
# than the ones above.
if ($fixed.Count -gt 0) {
  $report = Compare-VM -CompatibilityReport $report -ErrorAction Stop
  if ($report.Incompatibilities.Count -gt 0) {
    $rest = @()
    foreach ($i in @($report.Incompatibilities)) { $rest += ('[' + [string]$i.MessageId + '] ' + ([string]$i.Message).Trim()) }
    Discard-BallastPlanned
    throw ("after applying the fixes this VM still cannot run here: " + ($rest -join ' | '))
  }
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
$out.fixed = $fixed
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
		Name      string   `json:"name"`
		ID        string   `json:"id"`
		Imported  bool     `json:"imported"`
		Clustered bool     `json:"clustered"`
		Note      string   `json:"note"`
		Fixed     []string `json:"fixed"`
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
	/* What was CHANGED is part of the result, not a detail.

	   Two of the fixes alter what the VM is — a disconnected adapter, a removed
	   disk reference — and an import that quietly did that and reported plain
	   success would be worse than the refusal it replaced. The operator
	   consented to the change; they are also told it happened. */
	var out string
	switch {
	case res.Clustered:
		out = "imported " + name + " and added it as a clustered role"
	case v.Copy:
		out = "imported a copy of " + name + " as a new VM"
	default:
		out = "imported " + name + ", registered in place"
	}
	if len(res.Fixed) > 0 {
		out += " — " + strings.Join(res.Fixed, "; ")
	}
	if res.Note != "" {
		out += " — " + res.Note
	}
	return out, nil
}
