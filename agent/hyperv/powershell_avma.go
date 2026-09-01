package hyperv

import (
	"context"
	"fmt"
	"strings"
)

// avmaScript installs an AVMA key inside a guest so it activates against its host.
//
// The HOST is checked first, and refused before the guest is touched. AVMA works
// by the guest asking its Hyper-V host to vouch for it, and only a Datacenter host
// that is itself activated can: on Standard, on an evaluation, or on an
// unactivated host there is nothing to vouch for it and the key is declined. The
// resulting error names the guest, which sends an operator to the wrong machine —
// the guest is fine, and nothing done inside it can help.
//
// Installed over PowerShell Direct, which needs no network to the guest but does
// need a credential INSIDE it: the host's own account means nothing there.
const avmaScript = `
$ErrorActionPreference = 'Stop'
$vm = $env:BALLAST_AVMA_VM

# --- the host has to be able to vouch for it ------------------------------
$ed = ''
try { $ed = [string](Get-WindowsEdition -Online -ErrorAction Stop).Edition } catch {}
if ($ed -like '*Eval*') {
  throw ('this host runs ' + $ed + ', an evaluation edition. AVMA activates a guest against its host, and an evaluation host cannot vouch for one — convert and activate the host first. Nothing about ' + $vm + ' is wrong.')
}
if ($ed -and $ed -notlike '*Datacenter*') {
  throw ('this host runs ' + $ed + '. AVMA is a Datacenter feature: only a Datacenter host can activate its guests, so ' + $vm + ' needs a KMS host or its own key instead.')
}
$hp = @(Get-CimInstance SoftwareLicensingProduct -ErrorAction SilentlyContinue |
  Where-Object { $_.PartialProductKey -and $_.Name -like 'Windows*' })[0]
if (-not $hp -or [int]$hp.LicenseStatus -ne 1) {
  $st = if ($hp) { [string]$hp.LicenseStatus } else { 'none' }
  throw ('this host is not activated (licence status ' + $st + '), and AVMA activates a guest against its host — so ' + $vm + ' cannot be activated until the host itself is. Nothing about the guest is wrong.')
}

# --- the key must be an AVMA key, not this host's own -----------------------
#
# An AVMA key is a PUBLISHED, fixed value per Windows Server release and edition.
# A host's product key is not one, and installing it in a guest fails with the
# Software Licensing Service's own sentence — "the product SKU is not found" —
# which names neither the key nor what is wrong with it.
#
# Measured on HVNEW06, 2026-09-02: the key handed to BallastJumphost ended QYMT3,
# and so did the host's own installed product key. Ballast held both halves and
# said nothing, so the operator was sent to look at a guest that was fine.
#
# Compared on the last five characters because that is all Windows will tell us
# about an installed key, and it is enough: two different keys agreeing on their
# final block is not something worth building a second check for.
$hostProd = @(Get-CimInstance SoftwareLicensingProduct -ErrorAction SilentlyContinue |
  Where-Object { $_.PartialProductKey -and $_.Name -like 'Windows*' })[0]
$keyTail = $env:BALLAST_AVMA_KEY.Substring([math]::Max(0, $env:BALLAST_AVMA_KEY.Length - 5))
if ($hostProd -and [string]$hostProd.PartialProductKey -eq $keyTail) {
  throw ('that key ends ' + $keyTail + ', which is this host''s OWN product key — not an AVMA key. ' +
    'AVMA keys are published, fixed values, one per Windows Server release and edition, and they go in the ' +
    'guest rather than the host. Installing a host key in a guest fails with "the product SKU is not found", ' +
    'which is what would have happened here. Nothing about ' + $vm + ' is wrong.')
}

# --- the guest must be running and reachable ------------------------------
$v = Get-VM -Name $vm -ErrorAction SilentlyContinue
if (-not $v) { throw ('no VM named ' + $vm + ' on this host') }
if ([string]$v.State -ne 'Running') {
  throw ($vm + ' is ' + [string]$v.State + '. A key is installed inside the guest, so it has to be running.')
}

$gsec = ConvertTo-SecureString $env:BALLAST_GUEST_PW -AsPlainText -Force
$gcred = New-Object System.Management.Automation.PSCredential($env:BALLAST_GUEST_USER, $gsec)

$res = Invoke-Command -VMName $vm -Credential $gcred -ArgumentList $env:BALLAST_AVMA_KEY -ScriptBlock {
  param($key)
  $svc = Get-CimInstance SoftwareLicensingService -ErrorAction Stop
  function Get-GuestProduct {
    @(Get-CimInstance SoftwareLicensingProduct -ErrorAction SilentlyContinue |
      Where-Object { $_.PartialProductKey -and $_.Name -like 'Windows*' })[0]
  }
  $before = Get-GuestProduct
  $tail = $key.Substring([math]::Max(0, $key.Length - 5))
  # Already carrying this key and licensed: nothing to do. Reinstalling it would
  # be harmless but the reconcile would report a change on every pass, which is
  # how a settled VM comes to look like one that never settles.
  if ($before -and [int]$before.LicenseStatus -eq 1 -and [string]$before.PartialProductKey -eq $tail) {
    return 'NOOP'
  }
  try {
    Invoke-CimMethod -InputObject $svc -MethodName InstallProductKey -Arguments @{ ProductKey = $key } -ErrorAction Stop | Out-Null
  } catch {
    # "The Software Licensing Service reported that the product SKU is not
    # found" means this key does not match what this guest is running. An AVMA
    # key is per RELEASE and per EDITION, and the guest's own release is the one
    # fact the message leaves out.
    $m = [string]$_.Exception.Message
    if ($m -match 'product SKU is not found') {
      $os = ''
      try { $os = [string](Get-CimInstance Win32_OperatingSystem -ErrorAction SilentlyContinue).Caption } catch {}
      throw ('SKU_MISMATCH:' + $os)
    }
    throw
  }
  Invoke-CimMethod -InputObject $svc -MethodName RefreshLicenseStatus -ErrorAction SilentlyContinue | Out-Null
  $p = Get-GuestProduct
  if (-not $p) { throw 'the key was installed but the guest reports no Windows licensing product' }
  # AVMA activates through the host, so no Activate() call is made here — the key
  # alone is the request. Judged on the state that follows.
  if ([int]$p.LicenseStatus -ne 1) {
    return 'PENDING:' + [string]$p.LicenseStatus
  }
  return 'OK'
}

$r = [string]$res
if ($r -eq 'NOOP') { 'RESULT=NOOP'; return }
if ($r -eq 'OK') { 'RESULT=UPDATED'; return }
if ($r -like 'SKU_MISMATCH:*') {
  $guestOS = $r.Substring(13)
  $said = if ($guestOS) { ' The guest reports itself as ' + $guestOS + '.' } else { '' }
  throw ('that key is not an AVMA key for what ' + $vm + ' is running.' + $said +
    ' An AVMA key is published per Windows Server RELEASE and EDITION — a 2022 key will not activate a 2025 guest, ' +
    'and a Datacenter key will not activate a Standard one. Use the published AVMA key matching the guest''s own ' +
    'release and edition. The host is fine and so is the guest; only the key is wrong.')
}
if ($r -like 'PENDING:*') {
  throw ('the AVMA key was installed in ' + $vm + ' but it is not activated yet (licence status ' + $r.Substring(8) + '). AVMA needs the guest''s Data Exchange integration service running, and the key must be the one for that guest''s Windows version - an AVMA key is per release.')
}
throw ('installing the AVMA key in ' + $vm + ' returned an unexpected result: ' + $r)
`

// EnsureGuestAVMA installs an AVMA key inside a guest so it activates against
// this host. Idempotent: a guest already licensed with that key is a no-op.
//
// Refuses on a host that cannot vouch for a guest — Standard, evaluation, or not
// itself activated — before touching the guest at all, because the failure is the
// host's and an error naming the guest sends an operator to the wrong machine.
func (p *PowerShell) EnsureGuestAVMA(ctx context.Context, vmName, avmaKey, guestUser, guestPass string) (Outcome, error) {
	if strings.TrimSpace(avmaKey) == "" {
		return OutcomeUnchanged, nil
	}
	out, err := p.runWithEnvOut(ctx, avmaScript, []string{
		"BALLAST_AVMA_VM=" + vmName,
		"BALLAST_AVMA_KEY=" + avmaKey,
		"BALLAST_GUEST_USER=" + guestUser,
		"BALLAST_GUEST_PW=" + guestPass,
	})
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("activate guest %q: %w", vmName, err)
	}
	return resultOutcome(out, fmt.Sprintf("activate guest %q", vmName))
}
