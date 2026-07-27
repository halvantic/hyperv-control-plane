package hyperv

import (
	"context"
	"fmt"
	"strings"

	"github.com/joshua-fourie/ballast/api/types"
)

// resultOutcome interprets the output of a script that ends with an explicit
// RESULT=UPDATED / RESULT=NOOP marker. A script can die mid-run with exit code
// 0 and no marker — PowerShell's Select-Object -First pipeline stop does
// exactly this after FailoverClusters cmdlets, aborting the script silently —
// and treating that as "no change" is how a VM's replication read green for a
// week while never being configured. Marker missing ⇒ error, never a no-op.
func resultOutcome(out []byte, op string) (Outcome, error) {
	s := string(out)
	if strings.Contains(s, "RESULT=UPDATED") {
		return OutcomeUpdated, nil
	}
	if strings.Contains(s, "RESULT=NOOP") {
		return OutcomeUnchanged, nil
	}
	trimmed := strings.TrimSpace(s)
	if len(trimmed) > 300 {
		trimmed = trimmed[:300] + "…"
	}
	return OutcomeUnchanged, fmt.Errorf("%s: script ended without a result marker — output was truncated mid-run (partial output: %q)", op, trimmed)
}

// replicaDefaults fills the conventional Hyper-V Replica defaults: Kerberos
// integrated auth on port 80.
func replicaAuthPort(auth string, port int) (string, int) {
	if auth == "" {
		auth = "Kerberos"
	}
	if port == 0 {
		if strings.EqualFold(auth, "Certificate") {
			port = 443
		} else {
			port = 80
		}
	}
	return auth, port
}

// EnsureReplicaServer configures this host to accept Hyper-V Replica traffic.
// Idempotent: applies Set-VMReplicationServer only when the observed config
// differs, and enabling the firewall listener rule is a natural no-op.
func (p *PowerShell) EnsureReplicaServer(ctx context.Context, spec types.ReplicaServerSpec) (Outcome, error) {
	auth, port := replicaAuthPort(spec.AuthenticationType, spec.Port)
	storage := spec.DefaultStorageLocation
	if storage == "" {
		storage = `C:\Hyper-V\Replica`
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$enabled = %[1]s
$auth = %[2]s
$port = %[3]d
$storage = %[4]s
# On a cluster member the replication-server configuration hangs off the
# Hyper-V Replica Broker; before the broker role is online even the query
# fails with a misleading ObjectNotFound. Say what is actually being waited
# on — the reconcile retries every pass until the former's broker lands.
$clussvc = Get-Service ClusSvc -ErrorAction SilentlyContinue
if ($enabled -and $clussvc -and $clussvc.Status -eq 'Running') {
  # @(...)[0], NOT "| Select-Object -First 1": the -First pipeline stop can
  # abort the whole script (exit 0, no output marker) after cluster cmdlets.
  $broker = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'Virtual Machine Replication Broker' })[0]
  if (-not $broker) { throw 'a cluster node accepts replica traffic only via the Hyper-V Replica Broker - waiting for the cluster''s broker to be provisioned' }
  if ([string]$broker.State -ne 'Online') { throw ('waiting for the Hyper-V Replica Broker to come online (currently ' + $broker.State + ')') }
}
$rs = Get-VMReplicationServer -ErrorAction Stop
$changed = $false
if (-not $enabled) {
  if ($rs.ReplicationEnabled) { Set-VMReplicationServer -ReplicationEnabled $false -Confirm:$false; $changed = $true }
} else {
  if (-not (Test-Path $storage)) { New-Item -ItemType Directory -Path $storage -Force | Out-Null; $changed = $true }
  $wrongPort = $true
  try { $wrongPort = ([int]$rs.KerberosAuthenticationPort -ne $port -and [int]$rs.CertificateAuthenticationPort -ne $port) } catch {}
  if (-not $rs.ReplicationEnabled -or [string]$rs.AllowedAuthenticationType -notlike ('*' + $auth + '*') -or -not $rs.ReplicationAllowedFromAnyServer -or $wrongPort -or [string]$rs.DefaultStorageLocation -ne $storage) {
    if ($auth -eq 'Kerberos') {
      Set-VMReplicationServer -ReplicationEnabled $true -AllowedAuthenticationType Kerberos -KerberosAuthenticationPort $port -ReplicationAllowedFromAnyServer $true -DefaultStorageLocation $storage -Confirm:$false
    } else {
      Set-VMReplicationServer -ReplicationEnabled $true -AllowedAuthenticationType Certificate -CertificateAuthenticationPort $port -ReplicationAllowedFromAnyServer $true -DefaultStorageLocation $storage -Confirm:$false
    }
    $changed = $true
  }
  # The inbound listener rule ships disabled; without it replication requests
  # never arrive. Enable both HTTP and HTTPS listener rules (idempotent).
  foreach ($rule in @(Get-NetFirewallRule -DisplayName 'Hyper-V Replica*Listener*' -ErrorAction SilentlyContinue)) {
    if ($rule.Enabled -ne 'True') { $rule | Enable-NetFirewallRule; $changed = $true }
  }
}
if ($changed) { 'RESULT=UPDATED' } else { 'RESULT=NOOP' }`,
		psBool(spec.Enabled), psQuote(auth), port, psQuote(storage))
	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure replica server: %w", err)
	}
	return resultOutcome(out, "ensure replica server")
}

// EnsureReplicaBroker provisions the Hyper-V Replica Broker role on the local
// cluster: a client access point group (name + optional static IP) with the
// broker resource, started. Run on the former. Idempotent: a present, online
// broker is a no-op.
func (p *PowerShell) EnsureReplicaBroker(ctx context.Context, spec types.ReplicaBrokerSpec) (Outcome, error) {
	if spec.Name == "" {
		return OutcomeUnchanged, fmt.Errorf("ensure replica broker: broker name is required")
	}
	staticIP := ""
	if spec.StaticIP != "" {
		staticIP = fmt.Sprintf(" -StaticAddress %s", psQuote(spec.StaticIP))
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
Import-Module FailoverClusters
$name = %[1]s
$grp = Get-ClusterGroup -Name $name -ErrorAction SilentlyContinue
$changed = $false
if (-not $grp) {
  Add-ClusterServerRole -Name $name%[2]s | Out-Null
  $changed = $true
  $grp = Get-ClusterGroup -Name $name -ErrorAction Stop
}
$res = Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'Virtual Machine Replication Broker' -and $_.OwnerGroup -eq $name }
if (-not $res) {
  $res = Add-ClusterResource -Name 'Virtual Machine Replication Broker' -Type 'Virtual Machine Replication Broker' -Group $name
  Set-ClusterResourceDependency -Resource 'Virtual Machine Replication Broker' -Dependency ('[' + $name + ']')
  $changed = $true
}
if ($grp.State -ne 'Online') { Start-ClusterGroup -Name $name | Out-Null; $changed = $true }
if ($changed) { 'RESULT=UPDATED' } else { 'RESULT=NOOP' }`,
		psQuote(spec.Name), staticIP)
	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure replica broker %q: %w", spec.Name, err)
	}
	return resultOutcome(out, "ensure replica broker")
}

// EnsureVMReplication drives one VM's Hyper-V Replica relationship to spec.
// Absent + enabled: Enable-VMReplication then Start-VMInitialReplication.
// Present but drifted (server/frequency): Set-VMReplication. Present but
// spec disabled: Remove-VMReplication. Wedged relationships are repaired
// (ReadyForInitialReplication → start initial; Suspended/Error → resume;
// resynchronise states → resume with resync) and anything else with Critical
// health fails the condition instead of no-opping green.
func (p *PowerShell) EnsureVMReplication(ctx context.Context, vmName string, spec types.VMReplicationSpec) (Outcome, error) {
	auth, port := replicaAuthPort(spec.AuthenticationType, spec.Port)
	freq := spec.FrequencySeconds
	if freq == 0 {
		freq = 300
	}
	if spec.Enabled && spec.ReplicaServer == "" {
		return OutcomeUnchanged, fmt.Errorf("ensure vm replication: replicaServer (target address) is required")
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$vm = %[1]s
$enabled = %[2]s
$server = %[3]s
$port = %[4]d
$auth = %[5]s
$freq = %[6]d
$r = Get-VMReplication -VMName $vm -ErrorAction SilentlyContinue
$changed = $false
if (-not $enabled) {
  if ($r) { Remove-VMReplication -VMName $vm -ErrorAction Stop; $changed = $true }
} elseif ($r -and [string]$r.Mode -eq 'Replica') {
  # The primary side owns an existing relationship's settings and health repairs.
  # If this host holds the Replica copy — which happens transiently right after a
  # planned/unplanned failover flips the roles, until this host drops the VM from
  # its assignment — every operation below (Set-VMReplication, the Resume repairs)
  # fails with "Replication is not enabled". Leave the replica side untouched; the
  # new primary reconciles the relationship, and this host stops owning the VM on
  # its next pull.
  $changed = $false
} else {
  # A clustered primary can only replicate through its own cluster's Hyper-V
  # Replica Broker; without one Enable-VMReplication fails with a misleading
  # "object was not found". Check explicitly so the condition says what is
  # actually missing, and wait (retrying each pass) while the broker the
  # former is provisioning comes online.
  $isClustered = $false
  try { $isClustered = [bool](Get-VM -Name $vm -ErrorAction Stop).IsClustered } catch {}
  if ($isClustered) {
    # @(...)[0], NOT "| Select-Object -First 1": the -First pipeline stop can
  # abort the whole script (exit 0, no output marker) after cluster cmdlets.
  $broker = @(Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'Virtual Machine Replication Broker' })[0]
    if (-not $broker) { throw 'a clustered VM replicates through its own cluster''s Hyper-V Replica Broker, and this cluster has none - declare a ReplicaBroker on this cluster (re-saving the Replication dialog does it automatically)' }
    if ([string]$broker.State -ne 'Online') { throw ('waiting for the Hyper-V Replica Broker to come online (currently ' + $broker.State + ')') }
  }
  if (-not $r) {
    try {
      Enable-VMReplication -VMName $vm -ReplicaServerName $server -ReplicaServerPort $port -AuthenticationType $auth -ReplicationFrequencySec $freq -ErrorAction Stop
    } catch {
      # "IncludedDisks is not valid" is Hyper-V rejecting the disk set it computed
      # for the VM — almost always because the VM carries a disk that cannot be
      # replicated (an orphaned/duplicate VHD, e.g. a leftover replica copy, or a
      # disk on an inaccessible path). Surface the disk list and the actual cause
      # instead of the raw, cryptic parameter error.
      if ($_.Exception.Message -like '*IncludedDisks*') {
        $paths = @(Get-VMHardDiskDrive -VMName $vm -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.Path })
        throw ('cannot enable replication for ' + $vm + ': Hyper-V rejected the disk set. The VM has a disk that cannot be replicated - typically an orphaned or duplicate disk (e.g. a leftover replica VHD) or one on an inaccessible path. Attached disks: ' + ($paths -join '; ') + '. Detach the extra disk so the VM has only the disks it should replicate, then retry.')
      }
      throw
    }
    Start-VMInitialReplication -VMName $vm -ErrorAction Stop
    $changed = $true
  } else {
    if ([string]$r.ReplicaServer -ne $server -or [int]$r.FrequencySec -ne $freq) {
      Set-VMReplication -VMName $vm -ReplicaServerName $server -ReplicaServerPort $port -AuthenticationType $auth -ReplicationFrequencySec $freq -ErrorAction Stop
      $changed = $true
    }
    # A relationship can wedge in states this reconcile must not report as
    # settled. Repair the ones with a known remedy; anything else unhealthy
    # is surfaced as a failing condition rather than a green no-op — a
    # desired-state system must never say "configured" while replication is
    # not actually flowing.
    $state = [string]$r.State
    $health = [string]$r.Health
    if ($state -eq 'ReadyForInitialReplication') {
      Start-VMInitialReplication -VMName $vm -ErrorAction Stop
      $changed = $true
    } elseif ($state -eq 'Suspended') {
      Resume-VMReplication -VMName $vm -ErrorAction Stop
      $changed = $true
    } elseif ($state -eq 'WaitingForStartResynchronize' -or $state -eq 'ResynchronizeSuspended') {
      Resume-VMReplication -VMName $vm -Resynchronize -ErrorAction Stop
      $changed = $true
    } elseif ($state -eq 'Error') {
      Resume-VMReplication -VMName $vm -ErrorAction Stop
      $changed = $true
    } elseif ($health -eq 'Critical') {
      throw ('replication is configured but unhealthy: state ' + $state + ', health ' + $health + ' - see Get-VMReplication on the primary')
    }
  }
}
if ($changed) { 'RESULT=UPDATED' } else { 'RESULT=NOOP' }`,
		psQuote(vmName), psBool(spec.Enabled), psQuote(spec.ReplicaServer), port, psQuote(auth), freq)
	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure vm replication %q: %w", vmName, err)
	}
	return resultOutcome(out, "ensure vm replication "+vmName)
}

// replicaModeGuard is a PowerShell prelude that loads the VM's replication
// relationship and fails clearly unless this host holds the Replica copy. Every
// failover op runs on the replica side; running one on the primary is a common
// mistake the guard turns into an actionable error instead of a cryptic cmdlet
// failure. %[1]s is the (already psQuote'd) VM name; %[2]s names the operation.
func replicaModeGuard(vmVar, op string) string {
	return fmt.Sprintf(`$r = Get-VMReplication -VMName %[1]s -ErrorAction SilentlyContinue
if (-not $r) { throw ('%[2]s: no Hyper-V Replica relationship for ' + %[1]s + ' on this host - it runs on the replica target') }
if ([string]$r.Mode -ne 'Replica') { throw ('%[2]s runs on the Replica copy; this host reports mode ' + [string]$r.Mode) }`, vmVar, op)
}

// TestFailover starts a non-disruptive test failover of the replica VM: it
// builds a temporary "<vm> - Test" VM from the latest recovery point on an
// isolated network and starts it. The primary keeps running and replicating, so
// this is safe to run any time to prove the replica boots. Tear it down with
// StopTestFailover. Idempotent: an existing test VM is left running.
func (p *PowerShell) TestFailover(ctx context.Context, vmName, network string) (string, error) {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
%[1]s
$testName = %[2]s + ' - Test'
if (-not (Get-VM -Name $testName -ErrorAction SilentlyContinue)) {
  Start-VMFailover -VMName %[2]s -AsTest -Confirm:$false | Out-Null
}
# Connect the test VM's adapters to the chosen switch on this (replica) host — the
# source switch usually does not exist here, so without this the NIC is left with
# no switch and the VM has no network.
$net = %[3]s
if ($net) { Get-VMNetworkAdapter -VMName $testName -ErrorAction SilentlyContinue | Connect-VMNetworkAdapter -SwitchName $net -ErrorAction SilentlyContinue }
$tv = Get-VM -Name $testName -ErrorAction SilentlyContinue
if ($tv -and [string]$tv.State -ne 'Running') { Start-VM -Name $testName -ErrorAction SilentlyContinue | Out-Null }
'RESULT=OK'`,
		replicaModeGuard(psQuote(vmName), "test failover"), psQuote(vmName), psQuote(network))
	if _, err := p.run(ctx, script); err != nil {
		return "", fmt.Errorf("test failover %q: %w", vmName, err)
	}
	return "test VM \"" + vmName + " - Test\" running; tear down with Stop test failover", nil
}

// StopTestFailover tears down a test failover, removing the temporary test VM.
// The clean path is Stop-VMFailover on the relationship (which deletes the test
// VM and its differencing disks). But that is a no-op when the relationship no
// longer tracks the test VM — e.g. after a replication reset or reconfiguration
// — leaving the "<vm> - Test" clone orphaned on the host. So if the clone is
// still present afterwards, remove it directly. A test VM is a throwaway clone
// running off differencing disks over the replica's recovery point, so removing
// it and those child disks never touches the replica's base VHDs.
//
// When there is no test failover in progress at all (the replica is simply
// replicating), there is nothing to tear down — Stop-VMFailover errors with "the
// current replication state" and is swallowed, and the run completes as a no-op.
// The trailing RESULT marker is load-bearing: without a successful final
// statement the swallowed terminating error leaves $? false, and powershell.exe
// then exits 1 with no stderr — surfacing a spurious "exit status 1:" failure.
func (p *PowerShell) StopTestFailover(ctx context.Context, vmName string) error {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$testName = %[1]s + ' - Test'
# Clean path: cancel the test failover through the relationship if it is tracked.
# A no test-failover-in-progress error here is expected — nothing to stop.
try { Stop-VMFailover -VMName %[1]s -Confirm:$false -ErrorAction SilentlyContinue } catch {}
# Fallback: the clone can survive Stop-VMFailover when the relationship no longer
# owns it. Remove it (and its own differencing disks) so it does not linger.
$tv = Get-VM -Name $testName -ErrorAction SilentlyContinue
if ($tv) {
  $disks = @()
  try { $disks = @(Get-VMHardDiskDrive -VMName $testName -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.Path }) } catch {}
  try { if ([string]$tv.State -ne 'Off') { Stop-VM -Name $testName -TurnOff -Force -ErrorAction SilentlyContinue } } catch {}
  try { Remove-VM -Name $testName -Force -ErrorAction SilentlyContinue } catch {}
  if (Get-VM -Name $testName -ErrorAction SilentlyContinue) {
    throw ('failed to remove test VM ' + $testName + ' - it still exists after Remove-VM')
  }
  foreach ($d in $disks) { if ($d -and (Test-Path -LiteralPath $d)) { Remove-Item -LiteralPath $d -Force -ErrorAction SilentlyContinue } }
}
'RESULT=OK'`, psQuote(vmName))
	// p.run (not run2): the trailing RESULT=OK statement is what keeps a swallowed
	// terminating error from leaving $? false and making powershell.exe exit 1.
	if _, err := p.run(ctx, script); err != nil {
		return fmt.Errorf("stop test failover %q: %w", vmName, err)
	}
	return nil
}

// PlannedFailover performs a zero-data-loss planned failover from primaryHost to
// this (replica) host and reverses replication so the old primary becomes the
// new replica. Sequence: stop the primary and flush its final delta
// (Start-VMFailover -Prepare on the primary), complete failover here, reverse
// the relationship, and start the new primary. The primary must be reachable;
// for a clustered primary this stops the running VM — the cluster role may need
// to be taken offline first on some rigs.
func (p *PowerShell) PlannedFailover(ctx context.Context, vmName, primaryHost, network string) (string, error) {
	if strings.TrimSpace(primaryHost) == "" {
		return "", fmt.Errorf("planned failover %q: primary host is required", vmName)
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
%[1]s
$primary = %[3]s
# 1. Turn the primary off (planned failover requires it) and flush the final
#    delta to the replica so no writes are lost.
$pv = Get-VM -ComputerName $primary -Name %[2]s -ErrorAction Stop
if ([string]$pv.State -eq 'Running') { Stop-VM -ComputerName $primary -Name %[2]s -Force -ErrorAction Stop }
Start-VMFailover -ComputerName $primary -VMName %[2]s -Prepare -Confirm:$false -ErrorAction Stop
# 2. Complete failover here: this replica becomes the new primary.
Start-VMFailover -VMName %[2]s -Confirm:$false -ErrorAction Stop
# 3. Reverse the relationship so the old primary becomes the new replica.
Set-VMReplication -VMName %[2]s -Reverse -Confirm:$false -ErrorAction Stop
# 4. Connect the VM's adapters to the chosen switch on this host (the source
#    switch may not exist here), then bring the new primary up.
$net = %[4]s
if ($net) { Get-VMNetworkAdapter -VMName %[2]s -ErrorAction SilentlyContinue | Connect-VMNetworkAdapter -SwitchName $net -ErrorAction SilentlyContinue }
Start-VM -Name %[2]s -ErrorAction Stop
'RESULT=OK'`,
		replicaModeGuard(psQuote(vmName), "planned failover"), psQuote(vmName), psQuote(primaryHost), psQuote(network))
	if _, err := p.run(ctx, script); err != nil {
		return "", fmt.Errorf("planned failover %q: %w", vmName, err)
	}
	return "planned failover complete; " + vmName + " is primary here, replicating back to " + primaryHost, nil
}

// Failover performs an unplanned failover: brings this replica up as primary
// from its latest received data, or the named recovery point, after the primary
// is lost. Replication is left broken (the old primary is gone) until reversed
// with ReverseReplication once it returns. Cancel with CancelFailover to revert.
func (p *PowerShell) Failover(ctx context.Context, vmName, recoveryPoint, network string) (string, error) {
	rp := ""
	detail := "unplanned failover complete; " + vmName + " running here from the latest replica data"
	if strings.TrimSpace(recoveryPoint) != "" {
		rp = fmt.Sprintf(`$snap = @(Get-VMSnapshot -VMName %[1]s -ErrorAction SilentlyContinue | Where-Object { [string]$_.Name -eq %[2]s })[0]
if (-not $snap) { throw ('recovery point ' + %[2]s + ' not found on ' + %[1]s) }
Start-VMFailover -VMName %[1]s -VMRecoverySnapshot $snap -Confirm:$false -ErrorAction Stop`,
			psQuote(vmName), psQuote(recoveryPoint))
		detail = "unplanned failover complete; " + vmName + " running here from recovery point \"" + recoveryPoint + "\""
	} else {
		rp = fmt.Sprintf(`Start-VMFailover -VMName %[1]s -Confirm:$false -ErrorAction Stop`, psQuote(vmName))
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
%[1]s
%[2]s
$net = %[4]s
if ($net) { Get-VMNetworkAdapter -VMName %[3]s -ErrorAction SilentlyContinue | Connect-VMNetworkAdapter -SwitchName $net -ErrorAction SilentlyContinue }
Start-VM -Name %[3]s -ErrorAction Stop
'RESULT=OK'`,
		replicaModeGuard(psQuote(vmName), "unplanned failover"), rp, psQuote(vmName), psQuote(network))
	if _, err := p.run(ctx, script); err != nil {
		return "", fmt.Errorf("unplanned failover %q: %w", vmName, err)
	}
	return detail, nil
}

// CancelFailover reverts a test or unplanned failover on this host
// (Stop-VMFailover), returning the VM to the replica state. A no-op error is
// surfaced rather than swallowed so the operator sees when there was nothing to
// cancel.
func (p *PowerShell) CancelFailover(ctx context.Context, vmName string) error {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
# Reverting a failed-over replica returns it to the ready-for-replication state,
# which requires the VM to be off; Stop-VMFailover refuses while it is running.
# A test failover leaves the base VM off (only the '- Test' clone runs), so this
# only turns off a real unplanned failover before reverting it.
$vm = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
if ($vm -and [string]$vm.State -eq 'Running') { Stop-VM -Name %[1]s -Force -ErrorAction Stop }
Stop-VMFailover -VMName %[1]s -Confirm:$false`, psQuote(vmName))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("cancel failover %q: %w", vmName, err)
	}
	return nil
}

// ReverseReplication commits a pending unplanned failover (if any) and reverses
// the replication direction so this host — the new primary — replicates back to
// the former primary. The former primary must be reachable and configured as a
// replica server (a cluster's broker already accepts; a standalone old primary
// gets a ReplicaServer spec from the centre's topology rewrite).
func (p *PowerShell) ReverseReplication(ctx context.Context, vmName string) (string, error) {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$r = Get-VMReplication -VMName %[1]s -ErrorAction SilentlyContinue
if (-not $r) { throw ('reverse replication: no relationship for ' + %[1]s + ' on this host') }
# A failover that has not been committed leaves recovery points pending; commit
# them before reversing so the new relationship starts from a clean point.
if ([string]$r.State -eq 'FailedOverWaitingCompletion') { Complete-VMFailover -VMName %[1]s -Confirm:$false -ErrorAction Stop }
try {
  Set-VMReplication -VMName %[1]s -Reverse -Confirm:$false -ErrorAction Stop
} catch {
  # Hyper-V's own message here ("Could not reverse replication") is generic; the
  # cause is almost always the reverse target — the OTHER endpoint of this
  # relationship — being unreachable or not yet a Replica server. Probe it and
  # surface a specific, actionable reason instead of the bare cmdlet error. The
  # target is whichever endpoint is not this host, so we don't depend on which of
  # PrimaryServer/ReplicaServer has flipped after the failover.
  $reason = $_.Exception.Message
  $local = $env:COMPUTERNAME
  $target = @([string]$r.PrimaryServer, [string]$r.ReplicaServer) |
    Where-Object { $_ -and (($_ -split '\.')[0] -ne $local) } | Select-Object -First 1
  $port = 0; try { $port = [int]$r.ReplicaServerPort } catch {}
  if ($port -le 0) { $port = 80 }
  $hint = ''
  if ($target) {
    $reachable = $false
    try { $reachable = [bool](Test-NetConnection -ComputerName $target -Port $port -InformationLevel Quiet -WarningAction SilentlyContinue) } catch {}
    if (-not $reachable) {
      $hint = "the former primary '" + $target + "' is not reachable on the replica port (" + $port + "); bring it online, then retry"
    } else {
      $enabled = $null
      try { $enabled = [bool](Get-VMReplicationServer -ComputerName $target -ErrorAction Stop).ReplicationEnabled } catch { $enabled = $null }
      if ($enabled -eq $false) {
        $hint = "the former primary '" + $target + "' is reachable but its Replica server role is not enabled; the centre enables it after a failover, so wait for that host's agent to reconcile, then retry"
      } elseif ($null -eq $enabled) {
        $hint = "could not query the Replica server role on '" + $target + "'; verify it is online and configured to receive replicas, then retry"
      } else {
        $hint = "the former primary '" + $target + "' is reachable with its Replica server role enabled; check the Hyper-V-VMMS-Admin event log on both hosts for the specific reason"
      }
    }
  }
  if ($hint) { throw ($reason + ' - ' + $hint) } else { throw $reason }
}
'RESULT=OK'`, psQuote(vmName))
	if _, err := p.run(ctx, script); err != nil {
		return "", fmt.Errorf("reverse replication %q: %w", vmName, err)
	}
	return "replication reversed; " + vmName + " now replicates from here to the former primary", nil
}

// RemoveReplicaVM cleans up an orphaned replica copy on this (replica) host:
// removes the replica-side relationship, deletes the replica VM, and removes its
// replica VHDs. This is what unblocks re-enabling replication after a broken
// disable/enable cycle left a stale copy on the target (Enable-VMReplication then
// refuses because the target already has that VM). It refuses to run unless the
// VM here is actually a Replica, so it can never delete a primary/standalone VM.
func (p *PowerShell) RemoveReplicaVM(ctx context.Context, vmName string) error {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$vm = %[1]s
$v = Get-VM -Name $vm -ErrorAction SilentlyContinue
if ($v) {
  # Take the first relationship: extended replication (a replica that is itself
  # replicated onward) makes Get-VMReplication return an array, and [string]$r.Mode
  # would then join to "Replica Replica" and fail a plain -ne 'Replica' check.
  $r = @(Get-VMReplication -VMName $vm -ErrorAction SilentlyContinue)[0]
  if (-not $r -or [string]$r.Mode -ne 'Replica') {
    throw ('refusing to remove ' + $vm + ': it is not a replica copy on this host (mode ' + [string]$r.Mode + ') - use Delete VM instead')
  }
  $disks = @()
  try { $disks = @(Get-VMHardDiskDrive -VMName $vm -ErrorAction SilentlyContinue | ForEach-Object { [string]$_.Path }) } catch {}
  # Substep errors are tolerated and success is judged by the VM being gone
  # afterwards: Remove-VM can emit a trailing (non-fatal) error record even after
  # it has already deregistered the replica, which would otherwise fail the job
  # despite the cleanup having worked.
  try { Remove-VMReplication -VMName $vm -ErrorAction SilentlyContinue } catch {}
  try { if ([string]$v.State -ne 'Off') { Stop-VM -Name $vm -TurnOff -Force -ErrorAction SilentlyContinue } } catch {}
  try { Remove-VM -Name $vm -Force -ErrorAction SilentlyContinue } catch {}
  if (Get-VM -Name $vm -ErrorAction SilentlyContinue) {
    throw ('failed to remove replica copy ' + $vm + ' - it still exists after Remove-VM')
  }
  foreach ($d in $disks) { if ($d -and (Test-Path -LiteralPath $d)) { Remove-Item -LiteralPath $d -Force -ErrorAction SilentlyContinue } }
}`, psQuote(vmName))
	if err := p.run2(ctx, script); err != nil {
		return fmt.Errorf("remove replica copy %q: %w", vmName, err)
	}
	return nil
}
