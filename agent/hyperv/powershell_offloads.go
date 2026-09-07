package hyperv

import (
	"context"
	"fmt"
	"strings"
)

/* Turning off the offloads that defeat jumbo frames.

   Receive Segment Coalescing and Large Send Offload both re-segment traffic in
   the NIC. On some drivers that interacts badly with a 9000-byte MTU behind a
   Hyper-V vSwitch, and the failure is invisible from every angle Ballast could
   previously see: the adapter reports 9000, the IP interface reports 9000, the
   switch reports 9000, and large frames do not arrive.

   Found on the rig 2026-09-07 — HPE 631FLR-SFP28 on Server 2025 — where jumbo
   only started working after Disable-NetAdapterRsc and Disable-NetAdapterLso on
   every host, done by hand at a PowerShell prompt. That prompt is the defect
   CLAUDE.md names, which is why this exists.

   Reads back rather than trusting the cmdlets. Disable-NetAdapterRsc returns
   nothing on success and nothing on a driver that quietly declines, and a
   report of success for an adapter still coalescing would send an operator to
   look at their switch for a fault on their host.
*/

// OffloadState is what one adapter reports after being asked.
type OffloadState struct {
	Name string `json:"name"`
	RSC  bool   `json:"rsc"`
	LSO  bool   `json:"lso"`
	// Found is false when there is no adapter by this name.
	Found bool `json:"found"`
}

/*
DisableNICOffloads turns RSC and LSO off on the named adapters.

	Returns a sentence for the job result saying what is off now, and an error
	only where something is still on — an adapter that refuses is a fact worth
	failing on, because the operator's next move depends on believing it.
*/
func (p *PowerShell) DisableNICOffloads(ctx context.Context, adapters []string) (string, error) {
	if len(adapters) == 0 {
		return "", fmt.Errorf("no adapters were named, so nothing was changed")
	}
	out, err := p.run(ctx, disableOffloadsScript(adapters))
	if err != nil {
		return "", fmt.Errorf("disable offloads on %s: %w", strings.Join(adapters, ", "), err)
	}
	var got []OffloadState
	if derr := decodeJSON(out, &got); derr != nil {
		return "", fmt.Errorf("disable offloads: %w", derr)
	}

	var off, missing, stuck []string
	for _, a := range got {
		switch {
		case !a.Found:
			missing = append(missing, a.Name)
		case a.RSC || a.LSO:
			var still []string
			if a.RSC {
				still = append(still, "RSC")
			}
			if a.LSO {
				still = append(still, "LSO")
			}
			stuck = append(stuck, a.Name+" ("+strings.Join(still, " and ")+")")
		default:
			off = append(off, a.Name)
		}
	}

	var said []string
	if len(off) > 0 {
		said = append(said, "RSC and LSO are off on "+strings.Join(off, ", "))
	}
	if len(missing) > 0 {
		said = append(said, "no adapter named "+strings.Join(missing, ", ")+" on this host")
	}
	if len(stuck) > 0 {
		// Read back and still on. Reporting this as done would send the operator
		// to their switch for a fault that is on their host.
		return strings.Join(said, "; "), fmt.Errorf(
			"still enabled after being disabled: %s. The driver declined, which usually means the setting is "+
				"held somewhere else — a driver update or the adapter's own utility", strings.Join(stuck, ", "))
	}
	if len(said) == 0 {
		return "nothing to change", nil
	}
	return strings.Join(said, "; ") + ". Run the jumbo path test to confirm a full frame now crosses", nil
}

/*
disableOffloadsScript disables both and reports what each adapter says after.

	-NoRestart is not used: these take effect on the miniport, and deferring the
	restart would leave the host reporting them off while they are still in
	force — the same split between "configured" and "true" the MTU work exists
	to close.
*/
func disableOffloadsScript(adapters []string) string {
	return fmt.Sprintf(`
$ErrorActionPreference = 'SilentlyContinue'
$out = @()
foreach ($n in %s) {
  $a = Get-NetAdapter -Name $n -ErrorAction SilentlyContinue
  if (-not $a) { $out += [pscustomobject]@{ name = $n; found = $false }; continue }
  Disable-NetAdapterRsc -Name $n -ErrorAction SilentlyContinue | Out-Null
  Disable-NetAdapterLso -Name $n -ErrorAction SilentlyContinue | Out-Null
  # Read back. Both cmdlets return nothing on success AND nothing on a driver
  # that quietly declines, so the only proof is asking again.
  $rsc = $false
  foreach ($r in @(Get-NetAdapterRsc -Name $n -ErrorAction SilentlyContinue)) {
    if ($r.IPv4Enabled -or $r.IPv6Enabled) { $rsc = $true }
  }
  $lso = $false
  foreach ($l in @(Get-NetAdapterLso -Name $n -ErrorAction SilentlyContinue)) {
    if ($l.V1IPv4Enabled -or $l.IPv4Enabled -or $l.IPv6Enabled) { $lso = $true }
  }
  $out += [pscustomobject]@{ name = $n; found = $true; rsc = $rsc; lso = $lso }
}
ConvertTo-Json -Compress -Depth 4 -InputObject @($out)
`, psStringList(adapters))
}
