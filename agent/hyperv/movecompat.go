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

# DISCONNECT for the move, reconnect at the destination afterwards.
#
# Reconnecting the adapters inside the compatibility report does not take.
# Measured: four adapters matched, Connect-VMNetworkAdapter returned no error
# for any of them, and Move-VM still refused with "Could not find Ethernet
# switch ConvergedSwitch2" four times. The connect reached the object and never
# reached the report Move-VM validates.
#
# Disconnecting IS documented to work on a report, and a VM crossing hosts is
# off its network for the duration regardless. So the switch it should end up on
# is remembered here and applied on the other side, where the name resolves
# against the host that actually has it.
$plan = @()
$seen = @()
foreach ($inc in @($rep.Incompatibilities)) {
  $ad = $inc.Source
  $kind = if ($ad) { $ad.GetType().Name } else { '<none>' }
  $hasSwitch = [bool]($ad -and ($ad.PSObject.Properties.Name -contains 'SwitchName'))
  $seen += ('#' + [string]$inc.MessageId + ' source=' + $kind + ' switchName=' + $hasSwitch)
  if (-not $hasSwitch) { continue }
  $from = [string]$ad.SwitchName
  $plan += [pscustomobject]@{
    From = $from
    To   = [string]$netMap[$from]
    Vlan = [int]$vlanMap[$from]
    Mac  = [string]$ad.MacAddress
  }
  Disconnect-VMNetworkAdapter -VMNetworkAdapter $ad
}
if ($seen.Count -gt 0) { Write-Output ('PROGRESS the destination objected to: ' + ($seen -join '; ')) }

try {
  Move-VM -CompatibilityReport $rep
} catch {
  $what = if ($plan.Count -gt 0) { 'disconnected ' + [string]$plan.Count + ' adapter(s) first' } else { 'nothing - no incompatibility carried an adapter' }
  throw ([string]$_.Exception.Message + ' -- Ballast saw ' + [string]$seen.Count + ' incompatibilities (' +
    ($seen -join '; ') + ') and applied: ' + $what)
}

# On the other side now. The VM is here, its adapters are disconnected, and the
# switch names resolve against the host that actually has them.
$wanted = @($plan | Where-Object { $_.To })
if ($wanted.Count -gt 0) {
  $dstAds = @(Get-VMNetworkAdapter -ComputerName $dest -VMName $vm -ErrorAction SilentlyContinue)
  $targets = @($wanted | ForEach-Object { $_.To } | Sort-Object -Unique)
  $done = @()
  foreach ($p in $wanted) {
    $ad = $null
    # By MAC where there is one to match on: it survives the move and is the
    # only thing that ties an adapter to the switch it came off.
    if ($p.Mac -and $p.Mac -ne '000000000000') {
      $ad = @($dstAds | Where-Object { [string]$_.MacAddress -eq $p.Mac })[0]
    }
    # A VM that has never started has no MAC yet. Where every adapter is going
    # to the SAME switch that ambiguity does not matter, so take the next one
    # still disconnected rather than refusing over a distinction with no
    # consequence.
    if (-not $ad -and $targets.Count -eq 1) {
      $ad = @($dstAds | Where-Object { -not $_.SwitchName -and $done -notcontains $_.Id })[0]
    }
    if (-not $ad) {
      Write-Warning ('could not work out which adapter on ' + $dest + ' came off ' + $p.From +
        ', so it has been left disconnected. Attach it to ' + $p.To + ' by hand, or re-run with one network at a time')
      continue
    }
    $done += $ad.Id
    Connect-VMNetworkAdapter -VMNetworkAdapter $ad -SwitchName $p.To
    if ($p.Vlan -gt 0) { Set-VMNetworkAdapterVlan -VMNetworkAdapter $ad -Access -VlanId $p.Vlan }
  }
  Write-Output ('PROGRESS networks on ' + $dest + ': ' + (($wanted | ForEach-Object { $_.From + ' -> ' + $_.To }) -join ', '))
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
