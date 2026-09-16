package hyperv

import (
	"fmt"
	"sort"
	"strings"

	"github.com/halvantic/hyperv-control-plane/api/types"
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
# Compare first, for DIAGNOSIS only. The report is never mutated and never
# handed to Move-VM.
#
# Fixing adapters inside a compatibility report does not take. Measured three
# times over: connect by name, connect by switch object, and finally disconnect
# — the documented example — each returning no error and each leaving Move-VM
# refusing with "Could not find Ethernet switch ConvergedSwitch2" exactly as
# before. Whatever the report is, it is not what Move-VM validates.
#
# So the work happens on the real VM with ordinary cmdlets, and the comparison
# is kept only for what it is good at: saying what ELSE the destination objects
# to, early, in its own words.
$seen = @()
try {
  $rep = Compare-VM -Name $vm -DestinationHost $dest -IncludeStorage -DestinationStoragePath $path
  foreach ($inc in @($rep.Incompatibilities)) {
    $src = $inc.Source
    $kind = if ($src) { $src.GetType().Name } else { '<none>' }
    $seen += ('#' + [string]$inc.MessageId + ' ' + $kind + ': ' + [string]$inc.Message)
  }
} catch {
  $seen += ('the comparison itself failed: ' + [string]$_.Exception.Message)
}
if ($seen.Count -gt 0) { Write-Output ('PROGRESS the destination objected to: ' + ($seen -join '; ')) }

# The adapters the destination cannot match, taken off the SOURCE VM.
#
# Only the ones being remapped. An adapter on a switch the destination also has
# is left connected and never notices this happened — there is no reason to
# interrupt it. For the rest, a moment disconnected is strictly better than a
# move that fails, which is the only other outcome available.
$plan = @()
foreach ($ad in @(Get-VMNetworkAdapter -VMName $vm -ErrorAction SilentlyContinue)) {
  $from = [string]$ad.SwitchName
  if (-not $from -or -not $netMap.ContainsKey($from)) { continue }
  $oldVlan = 0
  try { $oldVlan = [int](Get-VMNetworkAdapterVlan -VMNetworkAdapter $ad -ErrorAction SilentlyContinue).AccessVlanId } catch {}
  $plan += [pscustomobject]@{
    Name = [string]$ad.Name; Mac = [string]$ad.MacAddress
    From = $from; To = [string]$netMap[$from]
    Vlan = [int]$vlanMap[$from]; OldVlan = $oldVlan
  }
  Disconnect-VMNetworkAdapter -VMNetworkAdapter $ad
}
if ($plan.Count -gt 0) {
  Write-Output ('PROGRESS taking ' + [string]$plan.Count + ' adapter(s) off ' +
    ((@($plan | ForEach-Object { $_.From }) | Sort-Object -Unique) -join ', ') + ' for the move')
}

try {
  Move-VM -Name $vm -DestinationHost $dest -IncludeStorage -DestinationStoragePath $path
} catch {
  # Put the source VM back as it was found. A failed move that also left the VM
  # off its network would turn a move that did not happen into an outage that
  # did.
  foreach ($p in $plan) {
    $back = @(Get-VMNetworkAdapter -VMName $vm -ErrorAction SilentlyContinue |
      Where-Object { $p.Mac -and [string]$_.MacAddress -eq $p.Mac })[0]
    if (-not $back) { $back = @(Get-VMNetworkAdapter -VMName $vm -Name $p.Name -ErrorAction SilentlyContinue)[0] }
    if ($back) {
      try {
        Connect-VMNetworkAdapter -VMNetworkAdapter $back -SwitchName $p.From
        if ($p.OldVlan -gt 0) { Set-VMNetworkAdapterVlan -VMNetworkAdapter $back -Access -VlanId $p.OldVlan }
      } catch {
        Write-Warning ('the move failed AND ' + $p.Name + ' could not be put back on ' + $p.From + ': ' + $_.Exception.Message)
      }
    }
  }
  $what = if ($plan.Count -gt 0) { 'took ' + [string]$plan.Count + ' adapter(s) off first and put them back' } else { 'no adapter needed remapping' }
  throw ([string]$_.Exception.Message + ' -- Ballast ' + $what + '. The destination objected to: ' +
    $(if ($seen.Count -gt 0) { ($seen -join '; ') } else { 'nothing the comparison could name' }))
}

# On the other side. The VM is here and the switch names resolve against the
# host that actually has them.
$wanted = @($plan | Where-Object { $_.To })
if ($wanted.Count -gt 0) {
  $dstAds = @(Get-VMNetworkAdapter -ComputerName $dest -VMName $vm -ErrorAction SilentlyContinue)
  $targets = @($wanted | ForEach-Object { $_.To } | Sort-Object -Unique)
  $done = @()
  foreach ($p in $wanted) {
    $ad = $null
    # By MAC, which survives the move and is the only thing tying an adapter to
    # the switch it came off.
    if ($p.Mac -and $p.Mac -ne '000000000000') {
      $ad = @($dstAds | Where-Object { [string]$_.MacAddress -eq $p.Mac })[0]
    }
    # A VM that has never started has no MAC yet. Where every adapter is going
    # to the same switch that ambiguity has no consequence.
    if (-not $ad -and $targets.Count -eq 1) {
      $ad = @($dstAds | Where-Object { -not $_.SwitchName -and $done -notcontains $_.Id })[0]
    }
    if (-not $ad) {
      Write-Warning ('the VM moved, but which adapter on ' + $dest + ' came off ' + $p.From +
        ' could not be worked out, so it is disconnected. Attach it to ' + $p.To + ' by hand')
      continue
    }
    $done += $ad.Id
    Connect-VMNetworkAdapter -VMNetworkAdapter $ad -SwitchName $p.To
    if ($p.Vlan -gt 0) { Set-VMNetworkAdapterVlan -VMNetworkAdapter $ad -Access -VlanId $p.Vlan }
  }
  Write-Output ('PROGRESS networks on ' + $dest + ': ' + ((@($wanted | ForEach-Object { $_.From + ' -> ' + $_.To }) | Sort-Object -Unique) -join ', '))
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
