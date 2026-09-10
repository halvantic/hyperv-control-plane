package hyperv

import (
	"context"
	"fmt"
	"strings"
)

// ISOLibraryState is what a host found at its declared library share.
type ISOLibraryState struct {
	Path     string
	Readable bool
	// MachineReadable is nil when the computer-account probe could not be run at
	// all. Unknown is a third state and must survive as one: reporting false for
	// a check that never happened condemns a working share.
	MachineReadable *bool
	Message         string
	ISOs            []string
}

// isoLibraryScript probes a share BOTH ways, because the two answers are
// different questions and routinely disagree.
//
// The agent runs as a service account. Hyper-V attaches an ISO from VMMS, which
// runs as LocalSystem, so it reaches the share as the node's COMPUTER ACCOUNT. A
// share granted to a user lists perfectly for the agent and cannot boot a VM;
// that combination is the normal outcome of "I gave my account access and it
// still doesn't work", and it is invisible unless both are tested.
//
// The witness taught this the expensive way: an ordinary Invoke-Command probe
// reached the NAS anonymously because the credential could not double-hop, so it
// false-failed a share that was fine. The only honest computer-account test is
// one that actually runs as LocalSystem, which means a scheduled task.
func isoLibraryScript(path, user, pass string) string {
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$share = %[1]s
$user = %[2]s
$pass = %[3]s
$out = [ordered]@{ readable = $false; machineReadable = $null; message = ''; isos = @() }

# 1. The agent's own read. This is what populates the list, and on its own it
#    proves nothing about whether a VM can boot from it.
#
#    THE CREDENTIAL APPLIES HERE AND NOWHERE ELSE, which is the whole subtlety
#    of it. It authenticates the agent's listing; it cannot change how Hyper-V
#    attaches media, which is always as the computer account. So a library
#    configured with a credential can list every image and still fail to boot a
#    VM, and step 2 below is what says so.
$mapped = ''
if ($user) {
  # New-SmbMapping wants the SHARE ROOT, not a folder inside it.
  $root = $share
  $m = [regex]::Match($share, '^(\\\\[^\\]+\\[^\\]+)')
  if ($m.Success) { $root = $m.Groups[1].Value }
  try {
    New-SmbMapping -RemotePath $root -UserName $user -Password $pass -ErrorAction Stop | Out-Null
    $mapped = $root
  } catch {
    $msg = [string]$_.Exception.Message
    # Windows permits ONE identity per server per session, so an existing
    # connection to the same file server refuses this one. That is a fact about
    # the session, not about the credential, and an operator told "the password
    # is wrong" would go and change a password that was fine.
    if ($msg -like '*multiple connections*' -or $msg -like '*Multiple connections*') {
      $out.message = 'this host already has a connection to ' + $root + ' as another identity, and Windows allows only one per server. The stored credential was not used; the listing below is whatever the existing connection can see.'
    } else {
      $out.message = 'the stored credential could not be used to reach ' + $root + ': ' + $msg
    }
  }
}
try {
  $files = @(Get-ChildItem -LiteralPath $share -Filter *.iso -File -ErrorAction Stop |
             Sort-Object LastWriteTime -Descending | ForEach-Object { [string]$_.Name })
  $out.readable = $true
  $out.isos = $files
} catch {
  $out.message = 'the agent cannot read ' + $share + ': ' + [string]$_.Exception.Message
} finally {
  if ($mapped) { Remove-SmbMapping -RemotePath $mapped -Force -ErrorAction SilentlyContinue }
}

# 2. The computer-account read, run as LocalSystem via a one-shot scheduled task.
#    Anything less answers a different question.
$tmp  = Join-Path $env:TEMP ('ballast-isolib-' + [guid]::NewGuid().ToString('N') + '.txt')
$task = 'BallastISOLibraryProbe-' + [guid]::NewGuid().ToString('N')
try {
  # Test-Path alone can succeed on a cached handle, so enumerate: that is what an
  # attach actually does.
  #
  # And return the FILE NAMES, not just a count. The computer account is the
  # identity that will attach the media, so it is also the right identity to list
  # what is attachable. Listing only as the agent meant a share granted to the
  # computer accounts — the grant this feature asks for — showed no images at all
  # and reported itself unreachable, while Hyper-V could boot from it perfectly.
  $probe = '$ErrorActionPreference=''Stop''; try { $f = @(Get-ChildItem -LiteralPath ''' + $share + ''' -Filter *.iso -File -Force -ErrorAction Stop | Sort-Object LastWriteTime -Descending | ForEach-Object { [string]$_.Name }); [pscustomobject]@{ ok = $true; isos = $f } | ConvertTo-Json -Compress | Set-Content -LiteralPath ''' + $tmp + ''' } catch { [pscustomobject]@{ ok = $false; error = [string]$_.Exception.Message } | ConvertTo-Json -Compress | Set-Content -LiteralPath ''' + $tmp + ''' }'
  $enc = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($probe))
  $act = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument ('-NonInteractive -NoProfile -EncodedCommand ' + $enc)
  $pri = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
  Register-ScheduledTask -TaskName $task -Action $act -Principal $pri -Force -ErrorAction Stop | Out-Null
  Start-ScheduledTask -TaskName $task -ErrorAction Stop
  $deadline = (Get-Date).AddSeconds(45)
  while ((Get-Date) -lt $deadline) {
    Start-Sleep -Milliseconds 500
    $ti = Get-ScheduledTaskInfo -TaskName $task -ErrorAction SilentlyContinue
    if ($ti -and $ti.LastTaskResult -ne 267009) { break }   # 267009 = still running
  }
  if (Test-Path -LiteralPath $tmp) {
    $res = $null
    try { $res = (Get-Content -LiteralPath $tmp -Raw) | ConvertFrom-Json } catch {}
    if ($res -and $res.ok) {
      $out.machineReadable = $true
      # The computer account's listing WINS when the agent could not read the
      # share: it is the identity that attaches the media, so what it can see is
      # what a VM can actually boot. Only fall back to the agent's list when the
      # machine probe returned nothing to say.
      $mi = @($res.isos)
      if ((-not $out.readable) -or ($out.isos.Count -eq 0)) { $out.isos = $mi }
    } elseif ($res) {
      $out.machineReadable = $false
      $out.message = 'Hyper-V attaches boot media as this node''s computer account, and that account cannot read ' + $share + ': ' + [string]$res.error + '. Grant the node computer accounts (or a group containing them) read access on the share - a grant to a user account does not apply here, and a NAS that is not domain-joined cannot authenticate them at all.'
    }
  }
  # else: leave machineReadable null. The probe did not report, so nothing is known.
} catch {
  # Registering or running the task failed; that says nothing about the share.
  if (-not $out.message) { $out.message = 'could not run the computer-account check: ' + [string]$_.Exception.Message }
} finally {
  Unregister-ScheduledTask -TaskName $task -Confirm:$false -ErrorAction SilentlyContinue
  Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
}

# A share the agent can read and the machine cannot is the case worth spelling
# out, because the library looks perfectly healthy in the console and every boot
# from it fails.
if ($out.readable -and $out.machineReadable -eq $false -and -not $out.message) {
  if ($user) {
    # The exact trap the credential creates: it makes the list look healthy and
    # changes nothing about the attach.
    $out.message = 'the stored credential lets Ballast list this share, but the node computer account cannot read it, so VMs still will not boot from it. A credential only affects this listing - Hyper-V attaches media as the computer account either way.'
  } else {
    $out.message = 'the agent can read this share but the node computer account cannot, so VMs will not boot from it'
  }
}
# The opposite, which is the NORMAL result of following this feature's own
# advice: the share is granted to the computer accounts and not to the agent's
# service account. VMs boot from it perfectly and the images are listed by the
# machine probe, so this is not a fault - it is worth one line rather than an
# alarm, and it must not read as unreachable.
if ((-not $out.readable) -and $out.machineReadable -eq $true) {
  $out.message = 'this share is granted to the node computer accounts but not to the agent''s service account, which is the expected result of granting Domain Computers. VMs boot from it normally; Ballast lists it through the computer account instead. Grant the agent''s account read as well only if you want Ballast to read it directly.'
}
[pscustomobject]$out | ConvertTo-Json -Compress`, psQuote(path), psQuote(user), psQuote(pass))
}

// CheckISOLibrary probes the declared share and reports what it found. It never
// mounts a drive: Hyper-V references the UNC directly, so a mapping would be one
// more piece of per-session state to keep in step and would not change what the
// attach can reach.
func (p *PowerShell) CheckISOLibrary(ctx context.Context, path, user, pass string) (ISOLibraryState, error) {
	st := ISOLibraryState{Path: path}
	if strings.TrimSpace(path) == "" {
		return st, nil
	}
	out, err := p.run(ctx, isoLibraryScript(path, user, pass))
	if err != nil {
		return st, fmt.Errorf("check iso library %q: %w", path, err)
	}
	var res struct {
		Readable        bool     `json:"readable"`
		MachineReadable *bool    `json:"machineReadable"`
		Message         string   `json:"message"`
		ISOs            []string `json:"isos"`
	}
	if err := decodeJSON(out, &res); err != nil {
		return st, fmt.Errorf("check iso library %q: %w", path, err)
	}
	st.Readable, st.MachineReadable, st.Message, st.ISOs = res.Readable, res.MachineReadable, res.Message, res.ISOs
	return st, nil
}
