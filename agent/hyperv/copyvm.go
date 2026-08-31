package hyperv

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/joshua-fourie/ballast/api/types"
)

/* Moving a VM by copying it, for two hosts that were not built to know about
   each other.

   Move-VM asks a great deal of the pair: live migration enabled at both ends,
   Kerberos constrained delegation between the computer accounts, compatible
   processors, and a destination that can satisfy every reference the VM makes —
   its switches especially. That is a reasonable ask between two nodes of one
   fabric and an unreasonable one when the whole point is reaching a NEW
   environment, which is most of what an evacuation is for.

   This asks almost nothing. Export the VM onto the destination's own storage
   over SMB, import it there, reconnect its adapters to the switches the
   operator mapped, and only then take the original away. The guest is off for
   the duration, which is the price.

   WHY THIS PATH CAN FIX THE NETWORKS AND Move-VM COULD NOT: Compare-VM's report
   is resolvable on the host that holds the files. Ballast already relies on that
   to import a VM whose switch does not exist locally. A report from
   Compare-VM -DestinationHost is a different thing and did not accept the same
   fixes — three attempts, three no-ops. Here the import happens where the files
   are, so it is the form that works. */

// copyVMScript exports a VM to the destination, imports it there, and removes
// the original once the copy is confirmed.
func copyVMScript(vm, destHost, destPath, sourceCluster, targetCluster string, nics []types.EvacuationNIC) string {
	return fmt.Sprintf(`$ErrorActionPreference='Stop'
$vm = %[1]s; $dest = %[2]s; $path = %[3]s
%[4]s
if (-not (Get-VM -Name $vm -ErrorAction SilentlyContinue)) { throw ('VM ' + $vm + ' is not on this host') }

# A running guest cannot be exported. Said as the choice it is rather than as a
# cmdlet's refusal: the operator picked a strategy whose price is the outage,
# and the alternative is a live move that needs the two hosts arranged for it.
$state = [string](Get-VM -Name $vm).State
if ($state -ne 'Off') {
  throw ($vm + ' is ' + $state + '. A copy exports the VM, which needs it switched off — that is the cost of a ' +
    'strategy that asks nothing of the destination. Stop the guest and run this again, or evacuate with Move instead, ' +
    'which keeps it running but needs live migration configured between the two hosts')
}

%[5]s

# Where the files go, and WHO writes them.
#
# Export-VM copies as VMMS, which runs as LocalSystem and reaches the network as
# this host's COMPUTER ACCOUNT. The admin share is administrators-only and a
# computer account is not one, so exporting to \dest\I$ fails with
#
#   Failed to copy file ... to '\HVNEW06\I$\...': Access is denied. (0x80070005)
#
# The same trap the ISO library carries a note about: the share has to grant the
# node, not the operator. So a share is made for exactly this move, granted to
# exactly this one computer account, and taken away again at the end.
$srcAcct = $env:COMPUTERNAME + '$'
if ($env:USERDOMAIN -and $env:USERDOMAIN -ne $env:COMPUTERNAME) { $srcAcct = $env:USERDOMAIN + '\' + $env:COMPUTERNAME + '$' }
# The AGENT's own account too, not only the computer account.
#
# Export-VM writes as the computer account, so that one must be granted or the
# copy is denied. But the agent reads the folder afterwards to find the exported
# configuration, and a share granted only to the machine account denies its own
# creator: Test-Path came back "Access is denied" against a share Ballast had
# just made.
$agentAcct = $env:USERNAME
if ($env:USERDOMAIN -and $env:USERDOMAIN -ne $env:COMPUTERNAME) { $agentAcct = $env:USERDOMAIN + '\' + $env:USERNAME }
$shareName = 'BallastEvac$'

$share = Invoke-Command -ComputerName $dest -ArgumentList $path, $shareName, $srcAcct, $agentAcct -ScriptBlock {
  param($p, $name, $acct, $agent)
  $grantees = @($acct, $agent) | Where-Object { $_ } | Sort-Object -Unique
  $ErrorActionPreference = 'Stop'
  if (-not (Test-Path -LiteralPath $p)) { New-Item -ItemType Directory -Path $p -Force | Out-Null }
  # NTFS as well as the share. Granting one and not the other is the half of
  # this that looks configured and still denies the write.
  $acl = Get-Acl -LiteralPath $p
  foreach ($g in $grantees) {
    $rule = New-Object System.Security.AccessControl.FileSystemAccessRule($g, 'Modify', 'ContainerInherit,ObjectInherit', 'None', 'Allow')
    $acl.AddAccessRule($rule)
  }
  Set-Acl -LiteralPath $p -AclObject $acl
  $existing = Get-SmbShare -Name $name -ErrorAction SilentlyContinue
  if ($existing) { Remove-SmbShare -Name $name -Force -ErrorAction SilentlyContinue }
  # Temporary: it does not survive a restart, so a host that reboots mid-move
  # is not left permanently sharing a volume nobody meant to share.
  New-SmbShare -Name $name -Path $p -FullAccess $grantees -Temporary | Out-Null
  $true
}
if (-not $share) { throw ('could not make a share on ' + $dest + ' for ' + $srcAcct + ' to export into') }
Write-Output ('PROGRESS made a temporary share on ' + $dest + ' for ' + $srcAcct)

$unc = '\\' + $dest + '\' + $shareName

# From here on the share must come down whatever happens: a volume left shared
# to a computer account is a thing nobody would think to look for.
try {

# Export refuses to write into a folder that already holds this VM, and a
# leftover from an attempt that failed part way is exactly what would be there.
$target = Join-Path $unc $vm
if (Test-Path -LiteralPath $target) {
  throw ($vm + ' already exists under ' + $path + ' on ' + $dest + '. A previous copy was left there; remove it, or ' +
    'import it if it is complete, before copying again')
}

# What the export has to move, so progress has a denominator. The file length,
# not Get-VHD's virtual size: a dynamic 500GB disk holding 40GB copies 40GB, and
# reporting it against 500 would read as stalled for the whole run.
$totalBytes = 0
foreach ($hd in @(Get-VMHardDiskDrive -VMName $vm -ErrorAction SilentlyContinue)) {
  $f = Get-Item -LiteralPath $hd.Path -ErrorAction SilentlyContinue
  if ($f) { $totalBytes += [int64]$f.Length }
}
$totalGB = [math]::Round($totalBytes / 1GB, 1)
Write-Output ('PROGRESS exporting ' + $vm + ' to ' + $dest +
  $(if ($totalBytes -gt 0) { ' (' + [string]$totalGB + ' GB)' } else { '' }))

# The export runs in a child so this one can WATCH it.
#
# Export-VM is synchronous and says nothing until it finishes, so a copy of any
# real size looked identical to a hung one: one line, then silence, then -- for
# the ten minutes the budget used to allow -- a failure that named a time limit
# and not a single byte. An operator could not tell a slow link from a stuck
# job, which is the difference that decides whether to wait or intervene.
#
# $ErrorActionPreference is set INSIDE the block on purpose. A child job does
# not inherit it, and this package has already shipped one bug where a job
# swallowed its own errors and the caller reported success.
$job = Start-Job -ArgumentList $vm, $unc -ScriptBlock {
  param($v, $u)
  $ErrorActionPreference = 'Stop'
  Export-VM -Name $v -Path $u
}
try {
  $lastBytes = -1
  $lastSaid = [datetime]::MinValue
  while ($job.State -eq 'Running') {
    Start-Sleep -Seconds 5
    $done = 0
    if (Test-Path -LiteralPath $target) {
      $m = Get-ChildItem -LiteralPath $target -Recurse -Force -ErrorAction SilentlyContinue |
        Where-Object { -not $_.PSIsContainer } | Measure-Object -Property Length -Sum
      if ($m -and $m.Sum) { $done = [int64]$m.Sum }
    }
    # Every 30s, and only when it has actually moved. A line repeating the same
    # number is what makes a console look busy while nothing happens.
    if ($done -ne $lastBytes -and ([datetime]::UtcNow - $lastSaid).TotalSeconds -ge 30) {
      $doneGB = [math]::Round($done / 1GB, 1)
      $pct = ''
      if ($totalBytes -gt 0) { $pct = ' (' + [string][math]::Round(100.0 * $done / $totalBytes) + '%%)' }
      Write-Output ('PROGRESS copied ' + [string]$doneGB + ' GB of ' + [string]$totalGB + ' GB' + $pct)
      $lastBytes = $done; $lastSaid = [datetime]::UtcNow
    }
  }
  # Receive-Job is what re-throws the child's failure here. Without it the job
  # ends Failed, the loop exits, and the copy carries on to an import with
  # nothing to import.
  Receive-Job -Job $job -Wait -ErrorAction Stop | Out-Null
  if ($job.State -ne 'Completed') { throw ('the export of ' + $vm + ' ended ' + [string]$job.State) }
} finally {
  Remove-Job -Job $job -Force -ErrorAction SilentlyContinue
}

# The configuration, as the DESTINATION will see it: a local path, because that
# is where the import runs.
$cfgUnc = @(Get-ChildItem -LiteralPath $target -Recurse -Filter *.vmcx -ErrorAction SilentlyContinue)[0]
if (-not $cfgUnc) { throw ('the export produced no configuration file under ' + $target + ', so there is nothing to import') }
# Derived by taking the path RELATIVE to the share and hanging it off the
# destination's own path. The first version built it with a regex replace and a
# ternary - the ternary is PowerShell 7 and the agent runs 5.1, so it would have
# failed to parse on every host it ever ran on.
$rel = $cfgUnc.FullName.Substring($unc.Length).TrimStart('\')
$cfgLocal = Join-Path $path $rel

Write-Output ('PROGRESS importing ' + $vm + ' on ' + $dest)
%[6]s

# Only now is the original removed. A copy that has not been confirmed on the
# other side is a backup, and deleting the source before then would turn a
# failed evacuation into a lost VM.
if (-not $imported) { throw ('the import on ' + $dest + ' did not report success, so ' + $vm + ' has been left here untouched') }

%[7]s
Remove-VM -Name $vm -Force -ErrorAction Stop

} finally {
  Invoke-Command -ComputerName $dest -ArgumentList $shareName -ScriptBlock {
    param($name)
    Get-SmbShare -Name $name -ErrorAction SilentlyContinue | Remove-SmbShare -Force -ErrorAction SilentlyContinue
  } -ErrorAction SilentlyContinue
}
Write-Output ('DONE copied ' + $vm + ' to ' + $dest)`,
		psQuote(vm), psQuote(destHost), psQuote(destPath),
		psSwitchMap(nics),
		ensureDestinationSMB(),
		remoteImport(targetCluster, nics),
		unclusterScript(vm, sourceCluster))
}

/*
ensureDestinationSMB is the firewall-and-reachability step, shared with the

	live move. Both paths depend on plain SMB to the destination and both meet
	the same 0x80070035 without it.
*/
func ensureDestinationSMB() string {
	return `$fwNote = ''
try {
  $fwSession = New-CimSession -ComputerName $dest -ErrorAction Stop
  $fwOff = @(Get-NetFirewallRule -CimSession $fwSession -DisplayGroup 'File and Printer Sharing' -ErrorAction Stop |
    Where-Object { -not $_.Enabled -or [string]$_.Enabled -eq 'False' })
  if ($fwOff.Count -gt 0) {
    Enable-NetFirewallRule -CimSession $fwSession -DisplayGroup 'File and Printer Sharing' -ErrorAction Stop
    Write-Output ('PROGRESS enabled File and Printer Sharing on ' + $dest)
  }
  Remove-CimSession $fwSession -ErrorAction SilentlyContinue
} catch {
  $fwNote = ' Ballast could not open the firewall on ' + $dest + ' itself: ' + $_.Exception.Message + '.'
}
$destShare = '\\' + $dest + '\admin$'
if (-not (Test-Path $destShare -ErrorAction SilentlyContinue)) {
  throw ('this host cannot reach ' + $destShare + ' over SMB, and a copy writes the VM onto the destination through ' +
    'its administrative share. Nothing has been changed here.' + $fwNote + ' Check this host can resolve and reach ' +
    $dest + ' on 445')
}
`
}

/*
remoteImport runs the import ON THE DESTINATION.

	One hop from here, so Kerberos carries it: the agent is a domain account and
	the destination is the only machine involved. The files are already on its
	local disk by this point, so nothing inside the session reaches a third
	place — which is the double-hop that would have needed CredSSP.

	The import itself resolves what the destination cannot satisfy. That is the
	form of Compare-VM that accepts a fix, and it is why a copy can land a VM
	whose switch does not exist there while a live move could not.
*/
func remoteImport(targetCluster string, nics []types.EvacuationNIC) string {
	cluster := "$null"
	if strings.TrimSpace(targetCluster) != "" {
		cluster = psQuote(targetCluster)
	}
	return fmt.Sprintf(`$imported = $false
$importOut = Invoke-Command -ComputerName $dest -ArgumentList $cfgLocal, $netMap, $vlanMap, %[1]s -ScriptBlock {
  param($cfg, $map, $vlans, $clusterName)
  $ErrorActionPreference = 'Stop'
  Import-Module Hyper-V -ErrorAction SilentlyContinue
  Import-Module FailoverClusters -ErrorAction SilentlyContinue

  $report = Compare-VM -Path $cfg -ErrorAction Stop
  $wanted = @{}
  foreach ($inc in @($report.Incompatibilities)) {
    $src = $inc.Source
    if (-not $src) { continue }
    if ([string]$src.GetType().Name -notmatch 'NetworkAdapter') { continue }
    # Remembered before the disconnect clears it, so the adapter can be put on
    # the switch the operator chose once the VM is registered here.
    $wanted[[string]$src.Id] = [string]$src.SwitchName
    Disconnect-VMNetworkAdapter -VMNetworkAdapter $src -ErrorAction Stop
  }
  if ($report.Incompatibilities.Count -gt 0) {
    throw ('this host cannot take the VM even with its networks disconnected: ' +
      ((@($report.Incompatibilities | ForEach-Object { [string]$_.Message })) -join ' | '))
  }

  $vmObj = Import-VM -CompatibilityReport $report -ErrorAction Stop
  foreach ($ad in @(Get-VMNetworkAdapter -VM $vmObj)) {
    $from = $wanted[[string]$ad.Id]
    if (-not $from) { continue }
    $to = $map[$from]
    # No mapping means arrive disconnected, deliberately: a VM quietly on the
    # wrong network is worse than one obviously on none.
    if (-not $to) { continue }
    Connect-VMNetworkAdapter -VMNetworkAdapter $ad -SwitchName $to
    $v = $vlans[$from]
    if ($v -and [int]$v -gt 0) { Set-VMNetworkAdapterVlan -VMNetworkAdapter $ad -Access -VlanId ([int]$v) }
  }
  if ($clusterName) { Add-ClusterVirtualMachineRole -Cluster $clusterName -VirtualMachine $vmObj.Name -ErrorAction Stop | Out-Null }
  [pscustomobject]@{ name = [string]$vmObj.Name; id = [string]$vmObj.Id }
}
if ($importOut -and $importOut.id) {
  $imported = $true
  Write-Output ('PROGRESS ' + $vm + ' is registered on ' + $dest)
}
`, cluster)
}

/* CopyBudget is how long a whole-VM storage copy may run.

   Twelve hours, deliberately generous, for the same reason a VMware migration
   pass gets twelve: the operation is bounded by somebody's disk and somebody's
   link, and a copy cut short at hour four wastes every byte of it. It lives
   here, beside the operation, so the budget and the work cannot drift apart the
   way the capture budget once did.

   Long is safe only because the copy now REPORTS. An export that has stopped
   moving is visible in the console within a minute; without that, a generous
   budget would just be a longer wait before the same unexplained failure. */
const CopyBudget = 12 * time.Hour

// CopyVM exports a VM to the destination, imports it there and removes the
// original. Run on the source host.
func (p *PowerShell) CopyVM(ctx context.Context, vm, destHost, destPath, sourceCluster, targetCluster string,
	networkMap []types.EvacuationNIC, onProgress ProgressFunc) (string, error) {

	result := "copied " + vm + " to " + destHost
	script := copyVMScript(vm, destHost, destPath, sourceCluster, targetCluster, networkMap)
	if err := p.runStream(ctx, script, migrationLineHandler(onProgress, &result)); err != nil {
		return "", fmt.Errorf("copy vm %q to %q: %w", vm, destHost, err)
	}
	return result, nil
}
