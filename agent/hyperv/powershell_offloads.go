package hyperv

import (
	"context"
	"fmt"
	"strings"
)

/* Large Send Offload, which defeats jumbo frames.

   LSO re-segments traffic in the NIC, and on some drivers that interacts badly
   with a 9000-byte MTU behind a Hyper-V vSwitch. The failure is invisible from
   every angle: the adapter reports 9000, the IP interface reports 9000, the
   switch reports 9000, and large frames do not arrive.

   LSO SPECIFICALLY, and not RSC. The first version flagged and disabled both,
   on the strength of a command that had disabled both. The operator then said
   they had only ever disabled LSO — and the readings agree: on HVNEW01-03,
   where jumbo works, RSC reads enabled on every uplink and LSO reads off. RSC
   is therefore not the blocker on this hardware, and flagging it put an amber
   action on three hosts that were already correct.

   It is still OBSERVED and reported, because it costs nothing and a future card
   may behave differently. It is not treated as a fault and nothing offers to
   change it, because nothing here has ever shown it to be one.

   SILENCE IS NOT "OFF". Get-NetAdapterLso returns nothing for an adapter that
   does not expose it, and the first version read that as disabled — so the job
   reported LSO off on four uplinks it had not changed, and the operator was
   told the fix had been applied when it had not. That is this codebase's most
   expensive bug class, written fresh in the very job meant to close an
   invisible fault. Every reading now says whether it could be read at all.
*/

// OffloadState is what one adapter reports after being asked.
type OffloadState struct {
	Name string `json:"name"`
	// LSO is whether Large Send Offload is still enabled; LSOKnown is whether
	// the adapter reported it at all. Absent is not off.
	LSO      bool `json:"lso"`
	LSOKnown bool `json:"lsoKnown"`
	// Found is false when there is no adapter by this name.
	Found bool `json:"found"`
}

/*
DisableLSO turns Large Send Offload off across a switch's whole stack.

	adapters are the physical uplinks; switches contributes the management vNICs
	layered over them, because the operator's own working command had no -Name
	and therefore hit both.

	Verified against a fresh read after a settle, not against what the cmdlet
	returned. Disable-NetAdapterLso is silent on success and silent on a driver
	that declines, and on the rig it declined on every uplink of HVNEW04 while
	the job reported success.
*/
func (p *PowerShell) DisableLSO(ctx context.Context, adapters, switches []string) (string, error) {
	if len(adapters) == 0 && len(switches) == 0 {
		return "", fmt.Errorf("no adapters or switches were named, so nothing was changed")
	}
	out, err := p.run(ctx, disableLSOScript(adapters, switches))
	if err != nil {
		return "", fmt.Errorf("disable LSO: %w", err)
	}
	var got []OffloadState
	if derr := decodeJSON(out, &got); derr != nil {
		return "", fmt.Errorf("disable LSO: %w", derr)
	}
	return summariseLSO(got)
}

/*
summariseLSO turns the readings into what an operator needs to know.

	Pure, so the reporting rule is testable without a Windows host — and the
	rule is where the fault was. What the job DID was never the problem; what it
	claimed about the result was.
*/
func summariseLSO(got []OffloadState) (string, error) {
	var off, missing, stuck, unreadable []string
	for _, o := range got {
		switch {
		case !o.Found:
			missing = append(missing, o.Name)
		case !o.LSOKnown:
			// Nothing was reported. Not off, not on — unknown, and it must not
			// be counted towards a claim of success.
			unreadable = append(unreadable, o.Name)
		case o.LSO:
			stuck = append(stuck, o.Name)
		default:
			off = append(off, o.Name)
		}
	}

	var said []string
	if len(off) > 0 {
		said = append(said, "LSO is off on "+strings.Join(off, ", "))
	}
	if len(unreadable) > 0 {
		said = append(said, strings.Join(unreadable, ", ")+" report no LSO setting to read")
	}
	if len(missing) > 0 {
		said = append(said, "not present on this host: "+strings.Join(missing, ", "))
	}

	if len(stuck) > 0 {
		// Read back and still on. This is the case that was reported as success
		// on HVNEW04, which sent the operator looking at their switch for a
		// fault on their host.
		return strings.Join(said, "; "), fmt.Errorf(
			"LSO is still enabled on %s after being disabled. The driver accepted the call and declined it, so this "+
				"host still cannot carry jumbo frames — it usually means the setting is held in the adapter's own "+
				"utility or needs a driver update", strings.Join(stuck, ", "))
	}
	if len(off) == 0 {
		return "", fmt.Errorf("nothing reported an LSO setting that could be read, so nothing was verified as "+
			"changed%s", ifAny(unreadable, ": "+strings.Join(unreadable, ", ")))
	}
	return strings.Join(said, "; ") + ". Run the jumbo path test to confirm a full frame now crosses", nil
}

func ifAny(list []string, s string) string {
	if len(list) == 0 {
		return ""
	}
	return s
}

/*
disableLSOScript disables LSO and reports what each adapter says afterwards.

	The verification is a FRESH read after a settle, deliberately. Reading back
	in the same breath returns what was requested rather than what took, which
	is how a job that changed nothing reported four adapters disabled.
*/
func disableLSOScript(adapters, switches []string) string {
	return fmt.Sprintf(`
$ErrorActionPreference = 'SilentlyContinue'

# The uplinks, and the management vNICs over the named switches. The operator's
# own working command had no -Name and so hit both.
$names = @()
foreach ($n in @(%[1]s)) { $names += $n }
foreach ($sw in @(%[2]s)) {
  foreach ($v in @(Get-VMNetworkAdapter -ManagementOS -SwitchName $sw -ErrorAction SilentlyContinue)) {
    $names += ('vEthernet (' + [string]$v.Name + ')')
  }
}
$names = @($names | Select-Object -Unique)

foreach ($n in $names) {
  if (Get-NetAdapter -Name $n -ErrorAction SilentlyContinue) {
    # Every sub-setting named explicitly. Without them the cmdlet's defaults
    # vary by driver, and V1IPv4 in particular is commonly left enabled.
    Disable-NetAdapterLso -Name $n -IPv4 -IPv6 -ErrorAction SilentlyContinue | Out-Null
    Disable-NetAdapterLso -Name $n -V1IPv4 -ErrorAction SilentlyContinue | Out-Null
  }
}

# Settle, then read fresh. Asking in the same breath returns what was requested
# rather than what took — which is how four unchanged adapters were reported off.
Start-Sleep -Seconds 3

$out = @()
foreach ($n in $names) {
  if (-not (Get-NetAdapter -Name $n -ErrorAction SilentlyContinue)) {
    $out += [pscustomobject]@{ name = $n; found = $false }; continue
  }
  # $null means the adapter reported nothing, which is NOT the same as off.
  $lso = $null
  foreach ($l in @(Get-NetAdapterLso -Name $n -ErrorAction SilentlyContinue)) {
    if ($lso -eq $null) { $lso = $false }
    if ($l.V1IPv4Enabled -or $l.IPv4Enabled -or $l.IPv6Enabled) { $lso = $true }
  }
  $out += [pscustomobject]@{ name = $n; found = $true; lso = [bool]$lso; lsoKnown = ($lso -ne $null) }
}
ConvertTo-Json -Compress -Depth 4 -InputObject @($out)
`, psStringList(adapters), psStringList(switches))
}
