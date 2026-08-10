package hyperv

import (
	"context"
	"fmt"
	"strings"
)

// WindowsLicence is a host's observed Windows edition and activation state,
// mirroring types.WindowsLicenceStatus without the schema dependency here.
type WindowsLicence struct {
	Edition            string
	Description        string
	Evaluation         bool
	Status             string
	GraceDaysRemaining int
	Channel            string
	PartialProductKey  string
	KMSServer          string
	Message            string
}

// licenceScript reads the edition and the activation state.
//
// Two sources, because neither answers the whole question. DISM knows the
// EDITION and whether it is an evaluation — the fact that decides whether the
// host can be activated at all — while SoftwareLicensingProduct knows the
// activation STATE and the grace period. An evaluation edition reports itself as
// unlicensed with a countdown and cannot be activated by any key, so reading only
// the licensing state would show a problem with no achievable remedy.
//
// Get-WindowsEdition is preferred over DISM.exe: same answer, no native exit code
// to interpret, and it does not write to the servicing log on every reconcile.
const licenceScript = `
$ErrorActionPreference = 'SilentlyContinue'
$out = [ordered]@{ edition=''; description=''; evaluation=$false; status=''; graceDays=0; channel=''; partialKey=''; kmsServer=''; message='' }

try {
  $ed = Get-WindowsEdition -Online -ErrorAction Stop
  $out.edition = [string]$ed.Edition
} catch {}
if (-not $out.edition) {
  # The registry holds it when the servicing stack will not answer, which is worth
  # having: an edition Ballast cannot read is one it cannot reason about at all.
  try { $out.edition = [string](Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion' -ErrorAction Stop).EditionID } catch {}
}
try { $out.description = [string](Get-CimInstance Win32_OperatingSystem -ErrorAction Stop).Caption } catch {}

# Evaluation is decided on the EDITION ID, not on the friendly caption. The
# caption is localised and a match on the word would be wrong in any language but
# English; the edition ID carries the Eval suffix on every build.
$out.evaluation = ($out.edition -like '*Eval*') -or ($out.description -like '*Evaluation*')

# The Windows product itself, not the many per-feature licences alongside it. It
# is identified by having a PartialProductKey — the others do not — which is the
# documented way to pick it out and survives the name changing between releases.
$p = @(Get-CimInstance SoftwareLicensingProduct -ErrorAction SilentlyContinue |
  Where-Object { $_.PartialProductKey -and $_.Name -like 'Windows*' })[0]
if ($p) {
  $out.partialKey = [string]$p.PartialProductKey
  $out.kmsServer  = [string]$p.KeyManagementServiceMachine
  $out.channel    = [string]$p.ProductKeyChannel
  # Reported in words. Nothing downstream should have to know that 1 means
  # licensed, and a number in a console is a number somebody has to look up.
  switch ([int]$p.LicenseStatus) {
    0 { $out.status = 'Unlicensed' }
    1 { $out.status = 'Licensed' }
    2 { $out.status = 'InitialGrace' }
    3 { $out.status = 'AdditionalGrace' }
    4 { $out.status = 'NonGenuineGrace' }
    5 { $out.status = 'Notification' }
    6 { $out.status = 'ExtendedGrace' }
    default { $out.status = 'Unknown' }
  }
  $mins = 0
  try { $mins = [int]$p.GracePeriodRemaining } catch {}
  if ($mins -gt 0) { $out.graceDays = [int][math]::Floor($mins / 1440) }
} else {
  $out.status = 'Unknown'
  $out.message = 'no Windows licensing product reported a product key on this host'
}

# The one thing an operator most needs told, because it makes the obvious next
# action the wrong one: an evaluation edition cannot be activated by any key.
if ($out.evaluation -and -not $out.message) {
  $out.message = 'this is an evaluation edition and cannot be activated - it has to be converted to a retail or volume edition first, which needs a restart and cannot be undone'
}
$out | ConvertTo-Json -Compress
`

// editionScript converts the host to the target edition.
//
// IRREVERSIBLE: there is no way back to an evaluation edition and no way down
// from a higher one. So it refuses everything it is not certain about, and asks
// Windows which conversions are legal rather than reasoning about it — the
// servicing stack knows, and a rule written here would be a guess that ages badly
// across releases.
//
// The key arrives in the environment, never in the script text: the script is
// what appears in an error message and in a log line, and a product key cannot be
// rotated once it has been used.
const editionScript = `
$ErrorActionPreference = 'Stop'
$target = %[1]s
$key = $env:BALLAST_PRODUCT_KEY

$cur = ''
try { $cur = [string](Get-WindowsEdition -Online -ErrorAction Stop).Edition } catch {}
if (-not $cur) { throw 'the current Windows edition could not be read, so a conversion cannot be judged safe' }
if ($cur -eq $target) { 'RESULT=NOOP'; return }

if (-not $key) { throw ('converting ' + $cur + ' to ' + $target + ' needs a product key, and none was delivered to this host') }

# A domain controller cannot be converted — DISM refuses, and it refuses late,
# after the operator has been told the change is under way.
try {
  $role = [int](Get-CimInstance Win32_ComputerSystem -ErrorAction Stop).DomainRole
  if ($role -eq 4 -or $role -eq 5) {
    throw ('this host is a domain controller, and Windows cannot change the edition of one. Demote it first, or convert a different host.')
  }
} catch {
  if ($_.Exception.Message -like '*domain controller*') { throw }
}

# Ask Windows which targets it will accept. This is the authoritative answer to
# "is this conversion legal", and it makes an impossible request fail with the
# list of possible ones instead of a servicing error.
$valid = @()
try { $valid = @(Get-WindowsEdition -Online -Target -ErrorAction Stop | ForEach-Object { [string]$_.Edition }) } catch {}
if ($valid.Count -gt 0 -and ($valid -notcontains $target)) {
  throw ('Windows will not convert ' + $cur + ' to ' + $target + '. From here it offers: ' + ($valid -join ', ') + '. An edition cannot be converted downwards, and an evaluation converts only to its own retail or volume equivalent.')
}

$r = Set-WindowsEdition -Online -ProductKey $key -NoRestart -ErrorAction Stop
# The conversion is staged and applied by the restart; nothing has changed on disk
# for the operator to see until then, which is why the reboot is reported rather
# than assumed to have happened.
'RESULT=REBOOT'
`

// EnsureWindowsEdition converts the host to the target edition when it differs.
//
// Returns OutcomeUnchanged when the host is already there, and OutcomeUpdated with
// rebootRequired true when a conversion was staged — the change only takes effect
// on restart, which the reconciler governs by RebootPolicy.
func (p *PowerShell) EnsureWindowsEdition(ctx context.Context, targetEdition, productKey string) (Outcome, bool, error) {
	target := strings.TrimSpace(targetEdition)
	if target == "" {
		return OutcomeUnchanged, false, nil
	}
	out, err := p.runWithEnvOut(ctx, fmt.Sprintf(editionScript, psQuote(target)),
		[]string{"BALLAST_PRODUCT_KEY=" + productKey})
	if err != nil {
		return OutcomeUnchanged, false, fmt.Errorf("set windows edition: %w", err)
	}
	s := string(out)
	if strings.Contains(s, "RESULT=NOOP") {
		return OutcomeUnchanged, false, nil
	}
	if strings.Contains(s, "RESULT=REBOOT") {
		return OutcomeUpdated, true, nil
	}
	// No marker means the script died part-way. Treating that as success would
	// report an edition change that may not have been staged at all.
	return OutcomeUnchanged, false, fmt.Errorf("set windows edition: the conversion ended without a result marker, so whether it was staged is unknown")
}

// GetWindowsLicence observes the host's Windows edition and activation state. A
// pure read; it never changes licensing.
func (p *PowerShell) GetWindowsLicence(ctx context.Context) (WindowsLicence, error) {
	raw, err := p.run(ctx, licenceScript)
	if err != nil {
		return WindowsLicence{}, fmt.Errorf("get windows licence: %w", err)
	}
	var res struct {
		Edition     string `json:"edition"`
		Description string `json:"description"`
		Evaluation  bool   `json:"evaluation"`
		Status      string `json:"status"`
		GraceDays   int    `json:"graceDays"`
		Channel     string `json:"channel"`
		PartialKey  string `json:"partialKey"`
		KMSServer   string `json:"kmsServer"`
		Message     string `json:"message"`
	}
	if derr := decodeJSON(raw, &res); derr != nil {
		return WindowsLicence{}, fmt.Errorf("get windows licence: %w", derr)
	}
	return WindowsLicence{
		Edition:            strings.TrimSpace(res.Edition),
		Description:        strings.TrimSpace(res.Description),
		Evaluation:         res.Evaluation,
		Status:             res.Status,
		GraceDaysRemaining: res.GraceDays,
		Channel:            res.Channel,
		PartialProductKey:  res.PartialKey,
		KMSServer:          res.KMSServer,
		Message:            res.Message,
	}, nil
}
