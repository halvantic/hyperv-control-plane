package hyperv

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// VM template operations are pure disk work. Capture copies a VM's first VHDX
// into the library, optionally generalising the guest with sysprep first;
// deploy copies an image back out and injects the guest's unattend.xml. Neither
// creates or destroys a VM — the centre authors desired state and the ordinary
// reconciler builds the machine, so a deployed VM is indistinguishable from any
// other from the moment it exists.
//
// Both scripts are built by pure functions so their content is unit-tested
// without a host, and both take their operands through the environment rather
// than the command line: an unattend.xml carries the guest's administrator
// password, and a sysprep needs a guest credential.

// captureTemplateScript builds the capture script. generalise selects whether
// sysprep runs in the guest first.
func captureTemplateScript(generalise, discardSaved bool) string {
	var b strings.Builder
	b.WriteString(`$ErrorActionPreference = 'Stop'
$vm = $env:BALLAST_CAP_VM
$dest = $env:BALLAST_CAP_DEST
`)
	// A clustered VM that is Off is an OFFLINE ROLE, and an offline role is not
	// registered with Hyper-V at all — so the very state a capture requires is the
	// state in which the VM cannot be found. See clusteredVMRegisterPrelude.
	b.WriteString(clusteredVMRegisterPrelude("$vm"))
	b.WriteString(`$v = $__vm
`)
	if generalise {
		b.WriteString(`if ([string]$v.State -ne 'Running') {
  throw ('sysprep runs inside the guest, so ' + $vm + ' must be Running to be generalised (it is ' + [string]$v.State + '). Start it, let the guest finish booting, then capture again.')
}
$gsec = ConvertTo-SecureString $env:BALLAST_CAP_PW -AsPlainText -Force
$gcred = New-Object System.Management.Automation.PSCredential($env:BALLAST_CAP_USER, $gsec)
# Sysprep shuts the guest down itself, which tears down the PowerShell Direct
# session carrying the command. Waiting on it inside the guest would therefore
# surface a broken session rather than a finished sysprep, so start it detached
# and watch the VM's power state from the host instead.
Invoke-Command -VMName $vm -Credential $gcred -ScriptBlock {
  $sp = Join-Path $env:SystemRoot 'System32\Sysprep\sysprep.exe'
  if (-not (Test-Path -LiteralPath $sp)) {
    throw 'sysprep.exe is not present in this guest; generalising requires a Windows guest'
  }
  Start-Process -FilePath $sp -ArgumentList '/generalize','/oobe','/shutdown','/quiet'
}
$deadline = (Get-Date).AddMinutes(60)
while ($true) {
  # A CLUSTERED VM shutting itself down takes its role offline, which deregisters
  # it from Hyper-V — so the VM vanishing here means sysprep finished, not that
  # something went wrong. Waiting for State -eq 'Off' would never be satisfied.
  $g = Get-VM -Name $vm -ErrorAction SilentlyContinue
  if (-not $g) { break }
  $st = [string]$g.State
  if ($st -eq 'Off') { break }
  if ((Get-Date) -gt $deadline) {
    throw ('timed out after 60 minutes waiting for ' + $vm + ' to shut down after sysprep (it is ' + $st + '). Sysprep logs its own failures in the guest at C:\Windows\System32\Sysprep\Panther.')
  }
  Start-Sleep -Seconds 10
}
# Re-register if the shutdown deregistered it, so its disks can be read.
$v = Ensure-BallastVMRegistered $vm
if (-not $v) { throw ('after sysprep, ' + $vm + ' is neither registered with Hyper-V nor an offline cluster role on this host') }
`)
	}
	// Discarding a saved state is part of the same job when the operator asked for
	// it, rather than a separate one they have to sequence: the capture is the
	// thing they want, and "make it Off first" is a step of it. Only Saved is
	// touched — an Off VM needs nothing and a Running one is a different refusal.
	if discardSaved {
		b.WriteString(`if ([string]$v.State -eq 'Saved') {
  Remove-VMSavedState -VMName $vm -ErrorAction Stop
  $v = Get-VM -Name $vm -ErrorAction SilentlyContinue
  if (-not $v) { throw ('discarded the saved state of ' + $vm + ' but Hyper-V no longer reports the VM') }
}
`)
	}
	// Hyper-V has more than two power states, and they fail for different reasons.
	// Saying "a running VM holds its VHDX open" about a SAVED VM is simply untrue —
	// a saved VM is not running and does not hold the file open — and it sends the
	// operator looking for something to stop that is already stopped.
	//
	// Saved matters here because it is where a clustered VM usually lands:
	// AutomaticStopAction defaults to Save, so taking a role offline saves the VM
	// rather than shutting it down.
	b.WriteString(`$state = [string]$v.State
if ($state -eq 'Saved') {
  throw ('VM ' + $vm + ' is in a SAVED state, not Off. Its disk is not locked, but it holds writes that were still in memory when it was saved, so an image copied now would behave like one taken from a machine that crashed. Either start it and shut the guest down cleanly, or discard the saved state (Remove-VMSavedState -VMName ''' + $vm + ''') to leave it Off — that throws away the saved memory, which for a template source is usually what you want. Note a clustered VM saves rather than shuts down when its role goes offline, because AutomaticStopAction defaults to Save.')
}
if ($state -ne 'Off') {
  if ($state -eq 'Running' -or $state -eq 'Paused') {
    throw ('VM ' + $vm + ' must be Off to capture: it is ' + $state + ', and holds its VHDX open, so the copy would be of a disk being written to.')
  }
  throw ('VM ' + $vm + ' must be Off to capture; it is ' + $state + '. Wait for it to settle, then capture again.')
}
$disks = @(Get-VMHardDiskDrive -VMName $vm | ForEach-Object { [string]$_.Path })
if ($disks.Count -eq 0) { throw ('VM ' + $vm + ' has no disks to capture') }
$src = $disks[0]
if (-not (Test-Path -LiteralPath $src)) {
  throw ('the VM''s disk ' + $src + ' cannot be read from this host — if it is on a CSV, the volume may be offline or owned elsewhere')
}
$dir = Split-Path -Parent $dest
if (-not (Test-Path -LiteralPath $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }
# Test-Path is false both for a missing directory and for one on a volume that
# cannot be read, so prove the library directory is reachable before writing a
# multi-gigabyte file at a path that may not resolve where it appears to.
if (-not (Test-Path -LiteralPath $dir)) {
  throw ('the library directory ' + $dir + ' is not reachable from this host (an offline CSV, or a volume owned by another node, reads exactly like a missing folder)')
}
if (Test-Path -LiteralPath $dest) {
  throw ('a template image already exists at ' + $dest + '; delete it first, or capture under a different template name')
}
# Copy to a temp name and move into place, so an interrupted capture never
# leaves something at the template's path that looks like a finished image.
$tmp = $dest + '.copying'
if (Test-Path -LiteralPath $tmp) {
  try { Remove-Item -LiteralPath $tmp -Force -ErrorAction Stop }
  catch { throw ('a capture to ' + $dest + ' is already in progress on this host (' + $tmp + ' is open in another process); wait for it to finish, or delete that file if it was abandoned') }
}
Copy-Item -LiteralPath $src -Destination $tmp -Force
Move-Item -LiteralPath $tmp -Destination $dest -Force
'BYTES=' + [string]((Get-Item -LiteralPath $dest).Length)
'RESULT=OK'
`)
	b.WriteString(clusteredVMRestoreSuffix)
	return b.String()
}

// CaptureTemplate copies vmName's first VHDX to dest, optionally running sysprep
// /generalize in the guest first (which needs a guest-local administrator
// credential and leaves the source VM generalised — it is no longer a usable
// machine, which is what generalising means). It returns the captured image's
// size in bytes.
func (p *PowerShell) CaptureTemplate(ctx context.Context, vmName, dest string, generalise, discardSaved bool, guestUser, guestPass string) (uint64, error) {
	env := []string{
		"BALLAST_CAP_VM=" + vmName,
		"BALLAST_CAP_DEST=" + dest,
		"BALLAST_CAP_USER=" + guestUser,
		"BALLAST_CAP_PW=" + guestPass,
	}
	out, err := p.runWithEnvOut(ctx, captureTemplateScript(generalise, discardSaved), env)
	if err != nil {
		return 0, fmt.Errorf("capture template from %q: %w", vmName, err)
	}
	if !strings.Contains(string(out), "RESULT=OK") {
		return 0, fmt.Errorf("capture template from %q: script ended without a result marker, so the copy cannot be assumed complete (partial output: %q)",
			vmName, truncateOutput(out))
	}
	return parseMarkerUint(string(out), "BYTES="), nil
}

// deployFromTemplateScript builds the deploy script. withUnattend selects
// whether the copied image is mounted and an unattend.xml written into it.
func deployFromTemplateScript(withUnattend bool) string {
	var b strings.Builder
	b.WriteString(`$ErrorActionPreference = 'Stop'
$src = $env:BALLAST_DEP_SRC
$dest = $env:BALLAST_DEP_DEST
if (-not (Test-Path -LiteralPath $src)) {
  throw ('the template image ' + $src + ' cannot be read from this host — if it is on a CSV, the volume may be offline or owned by another node')
}
$dir = Split-Path -Parent $dest
if (-not (Test-Path -LiteralPath $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }
if (-not (Test-Path -LiteralPath $dir)) {
  throw ('the destination directory ' + $dir + ' is not reachable from this host (an offline CSV, or a volume owned by another node, reads exactly like a missing folder)')
}
if (Test-Path -LiteralPath $dest) {
  throw ('a disk already exists at ' + $dest + '; a deploy will not write over it')
}
$tmp = $dest + '.deploying'
if (Test-Path -LiteralPath $tmp) {
  try { Remove-Item -LiteralPath $tmp -Force -ErrorAction Stop }
  catch { throw ('a deploy to ' + $dest + ' is already in progress on this host (' + $tmp + ' is open in another process)') }
}
Copy-Item -LiteralPath $src -Destination $tmp -Force
`)
	if withUnattend {
		b.WriteString(`# Write the unattend into the copy before it is ever attached to a VM, so the
# specialise pass consumes it on first boot. Mounting is done on the temp file:
# a mount that fails must not leave a mounted, half-configured image sitting at
# the destination path where the reconciler would attach it.
$mounted = $false
try {
  Mount-VHD -Path $tmp -ErrorAction Stop
  $mounted = $true
  $disk = Get-VHD -Path $tmp | Get-Disk
  $letters = @(Get-Partition -DiskNumber $disk.Number |
    Where-Object { $_.DriveLetter } |
    ForEach-Object { [string]$_.DriveLetter })
  $target = ''
  foreach ($l in $letters) {
    if (Test-Path -LiteralPath ($l + ':\Windows\System32')) { $target = $l; break }
  }
  if (-not $target) {
    throw ('no Windows installation was found in the template image (partitions checked: ' + ($letters -join ', ') + '). Guest customisation needs one; deploy the template without a guest profile to skip it.')
  }
  $panther = $target + ':\Windows\Panther'
  if (-not (Test-Path -LiteralPath $panther)) { New-Item -ItemType Directory -Path $panther -Force | Out-Null }
  # Written as UTF-8 without a BOM: Windows Setup rejects an unattend whose XML
  # declaration is preceded by a byte order mark.
  [System.IO.File]::WriteAllText((Join-Path $panther 'Unattend.xml'), $env:BALLAST_DEP_UNATTEND, (New-Object System.Text.UTF8Encoding($false)))
} finally {
  if ($mounted) { Dismount-VHD -Path $tmp -ErrorAction SilentlyContinue }
}
`)
	}
	b.WriteString(`Move-Item -LiteralPath $tmp -Destination $dest -Force
'RESULT=OK'
`)
	return b.String()
}

// DeployFromTemplate copies a template image to dest and, when unattend is
// non-empty, injects it into the copy so the guest customises itself on first
// boot. It deliberately does not create the VM: the centre authors the VM's
// desired state when this job succeeds and the reconcile loop builds it.
func (p *PowerShell) DeployFromTemplate(ctx context.Context, src, dest, unattend string) error {
	env := []string{
		"BALLAST_DEP_SRC=" + src,
		"BALLAST_DEP_DEST=" + dest,
		"BALLAST_DEP_UNATTEND=" + unattend,
	}
	out, err := p.runWithEnvOut(ctx, deployFromTemplateScript(unattend != ""), env)
	if err != nil {
		return fmt.Errorf("deploy template image to %q: %w", dest, err)
	}
	if !strings.Contains(string(out), "RESULT=OK") {
		return fmt.Errorf("deploy template image to %q: script ended without a result marker, so the copy cannot be assumed complete (partial output: %q)",
			dest, truncateOutput(out))
	}
	return nil
}

// runWithEnvOut is runWithEnv that also returns stdout. Like runWithEnv it
// bypasses the injectable run field, because the environment is how secrets
// reach the script; the scripts themselves are unit-tested as strings.
func (p *PowerShell) runWithEnvOut(ctx context.Context, script string, extraEnv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("powershell: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// parseMarkerUint reads the unsigned integer following the last occurrence of
// marker, returning 0 when it is absent or unparseable.
func parseMarkerUint(out, marker string) uint64 {
	i := strings.LastIndex(out, marker)
	if i < 0 {
		return 0
	}
	rest := out[i+len(marker):]
	if j := strings.IndexAny(rest, "\r\n"); j >= 0 {
		rest = rest[:j]
	}
	n, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// truncateOutput bounds script output quoted into an error message.
func truncateOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// discardSavedStateScript builds the script that turns a Saved VM into an Off
// one. It is shared with the capture, which can be asked to do this first.
//
// A clustered VM is Saved rather than Off whenever its role goes offline —
// AutomaticStopAction defaults to Save — and while the role is offline the VM is
// not registered at all, so the registration prelude runs first. That is what
// makes this something the centre can do: without it, the only way out of a
// saved clustered VM was PowerShell on a node.
const discardSavedStateBody = `$state = [string]$__vm.State
if ($state -eq 'Off') { 'RESULT=NOOP'; return }
if ($state -ne 'Saved') {
  throw ('VM ' + $vm + ' is ' + $state + ', not Saved — there is no saved state to discard.')
}
Remove-VMSavedState -VMName $vm -ErrorAction Stop
$after = [string](Get-VM -Name $vm -ErrorAction SilentlyContinue).State
if ($after -ne 'Off') {
  throw ('discarded the saved state of ' + $vm + ' but it is ' + $after + ', not Off')
}
'RESULT=OK'
`

func discardSavedStateScript() string {
	return "$ErrorActionPreference = 'Stop'\n$vm = $env:BALLAST_VM\n" +
		clusteredVMRegisterPrelude("$vm") + discardSavedStateBody + clusteredVMRestoreSuffix
}

// DiscardVMSavedState throws away vmName's saved memory image so it becomes Off.
// The memory state is lost, which is the point: for a VM about to be captured,
// cloned or moved, the alternative is starting it — and starting a generalised
// image specialises it, undoing the very thing being captured.
//
// Already-Off is a no-op rather than an error, so this is safe to run ahead of an
// operation that merely needs the VM Off.
func (p *PowerShell) DiscardVMSavedState(ctx context.Context, vmName string) error {
	out, err := p.runWithEnvOut(ctx, discardSavedStateScript(), []string{"BALLAST_VM=" + vmName})
	if err != nil {
		return fmt.Errorf("discard saved state of %q: %w", vmName, err)
	}
	if !strings.Contains(string(out), "RESULT=") {
		return fmt.Errorf("discard saved state of %q: script ended without a result marker (partial output: %q)", vmName, truncateOutput(out))
	}
	return nil
}
