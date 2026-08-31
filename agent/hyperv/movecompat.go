package hyperv

import (
	"fmt"
	"sort"
	"strings"

	"github.com/joshua-fourie/ballast/api/types"
)

/* Moving a VM onto a host that names its switches differently.

   A VM carries the NAME of the switch its adapters are on, and Move-VM refuses
   outright when the destination has no switch by that name:

     The virtual machine 'HVNew01' is not compatible with physical computer
     'HVNEW06'. Could not find Ethernet switch 'ConvergedSwitch2'.

   Two hosts built separately do not agree on switch names, which is most of the
   reason an evacuation exists — it is how workloads reach a NEW environment. So
   the move asks Hyper-V what the destination objects to, fixes the adapters
   against a mapping the operator gave, and then moves against that same report.
   That is Hyper-V's own mechanism for this: Compare-VM, adjust, Move-VM
   -CompatibilityReport.

   AN UNMAPPED NETWORK ARRIVES DISCONNECTED. Not attached to whichever switch
   happens to be there: a VM silently on the wrong network is worse than one
   obviously on none, and the second is noticed in seconds. */

// moveWithNetworkMap builds the move command, remapping adapters where the
// destination cannot match a switch by name.
func moveWithNetworkMap(nics []types.EvacuationNIC) string {
	var b strings.Builder
	b.WriteString(psSwitchMap(nics))
	b.WriteString(`
$rep = Compare-VM -Name $vm -DestinationHost $dest -IncludeStorage -DestinationStoragePath $path
$fixed = @()
# What the comparison actually returned, recorded as it is walked.
#
# Reasoning about this API has been wrong twice: once about whether the list
# clears as entries are fixed, once about which entries carry an adapter. So the
# move now says what it saw — the id, the type of the Source object and whether
# it looked like an adapter — and carries it into any failure. A guess about
# somebody else's object model is not something to make a third time.
$seen = @()
foreach ($inc in @($rep.Incompatibilities)) {
  $ad = $inc.Source
  $kind = if ($ad) { $ad.GetType().Name } else { '<none>' }
  $hasSwitch = [bool]($ad -and ($ad.PSObject.Properties.Name -contains 'SwitchName'))
  $seen += ('#' + [string]$inc.MessageId + ' source=' + $kind + ' switchName=' + $hasSwitch)
  # Identified by the adapter's own shape rather than by MessageId 33012.
  # The id is right today and is a number in somebody else's product; an object
  # carrying a SwitchName is an adapter whatever the id happens to be.
  if (-not $hasSwitch) { continue }
  $from = [string]$ad.SwitchName
  $to = $netMap[$from]
  if ($to) {
    Connect-VMNetworkAdapter -VMNetworkAdapter $ad -SwitchName $to
    $v = $vlanMap[$from]
    if ($v -and [int]$v -gt 0) { Set-VMNetworkAdapterVlan -VMNetworkAdapter $ad -Access -VlanId ([int]$v) }
    $fixed += ($from + ' -> ' + $to)
  } else {
    # Deliberate, and said out loud. Arriving on the wrong network is the one
    # outcome worse than arriving on none.
    Disconnect-VMNetworkAdapter -VMNetworkAdapter $ad
    $fixed += ($from + ' -> disconnected (no mapping given)')
  }
}
if ($fixed.Count -gt 0) { Write-Output ('PROGRESS networks: ' + ($fixed -join ', ')) }

# What was in the report, REPORTED and not judged.
#
# Incompatibilities do not clear as they are fixed — the list is a snapshot —
# and it always carries generic wrappers whose Source is the VM itself:
# "failed at migration destination", "is not compatible with physical computer".
# Treating anything that is not an adapter as unhandled therefore threw on those
# wrappers after the only real problem, a switch name, had just been remapped.
#
# So Move-VM decides. It validates again for itself and refuses with the
# specific reason when something genuinely remains, which is the message an
# operator needs; this only carries the report forward so a failure can be read
# against what was seen beforehand.
if ($seen.Count -gt 0) { Write-Output ('PROGRESS the destination objected to: ' + ($seen -join '; ')) }

# The findings ride into the failure, not only into the progress notes.
#
# A progress line is gone by the time somebody reads a failed job, and the whole
# question when this fails is what the comparison offered and what was done with
# it. Without that the message is Hyper-V's alone and says nothing about whether
# Ballast even tried.
try {
  Move-VM -CompatibilityReport $rep
} catch {
  $what = if ($fixed.Count -gt 0) { ($fixed -join ', ') } else { 'nothing - no incompatibility carried an adapter' }
  throw ([string]$_.Exception.Message + ' -- Ballast saw ' + [string]$seen.Count + ' incompatibilities (' +
    ($seen -join '; ') + ') and applied: ' + $what)
}`)
	return b.String()
}

/*
psSwitchMap renders the mapping as two PowerShell hashtables.

	Built here rather than passed as JSON and parsed there: the values are switch
	names an operator typed, and the quoting is the only thing standing between a
	name with an apostrophe in it and a script that does something else entirely.
	psQuote is the same doubling used everywhere in this package.
*/
func psSwitchMap(nics []types.EvacuationNIC) string {
	byName := map[string]types.EvacuationNIC{}
	for _, n := range nics {
		if strings.TrimSpace(n.SourceSwitch) != "" {
			byName[n.SourceSwitch] = n
		}
	}
	names := make([]string, 0, len(byName))
	for k := range byName {
		names = append(names, k)
	}
	// Sorted so the generated script is stable, which is what makes it testable
	// and what keeps a diff about a mapping change rather than about map order.
	sort.Strings(names)

	var sw, vl strings.Builder
	sw.WriteString("$netMap = @{")
	vl.WriteString("$vlanMap = @{")
	for i, name := range names {
		n := byName[name]
		if i > 0 {
			sw.WriteString("; ")
			vl.WriteString("; ")
		}
		sw.WriteString(fmt.Sprintf("%s = %s", psQuote(name), psQuote(n.TargetSwitch)))
		vl.WriteString(fmt.Sprintf("%s = %d", psQuote(name), n.VLANID))
	}
	sw.WriteString("}\n")
	vl.WriteString("}\n")
	return sw.String() + vl.String()
}
