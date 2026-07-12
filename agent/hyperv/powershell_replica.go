package hyperv

import (
	"context"
	"fmt"
	"strings"

	"github.com/joshua-fourie/ballast/api/types"
)

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
	if strings.Contains(string(out), "RESULT=UPDATED") {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
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
	if strings.Contains(string(out), "RESULT=UPDATED") {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
}

// EnsureVMReplication drives one VM's Hyper-V Replica relationship to spec.
// Absent + enabled: Enable-VMReplication then Start-VMInitialReplication.
// Present but drifted (server/frequency): Set-VMReplication. Present but
// spec disabled: Remove-VMReplication. A relationship stuck at
// ReadyForInitialReplication gets its initial replication (re)started.
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
} else {
  # A clustered primary can only replicate through its own cluster's Hyper-V
  # Replica Broker; without one Enable-VMReplication fails with a misleading
  # "object was not found". Check explicitly so the condition says what is
  # actually missing, and wait (retrying each pass) while the broker the
  # former is provisioning comes online.
  $isClustered = $false
  try { $isClustered = [bool](Get-VM -Name $vm -ErrorAction Stop).IsClustered } catch {}
  if ($isClustered) {
    $broker = Get-ClusterResource -ErrorAction SilentlyContinue | Where-Object { $_.ResourceType -eq 'Virtual Machine Replication Broker' } | Select-Object -First 1
    if (-not $broker) { throw 'a clustered VM replicates through its own cluster''s Hyper-V Replica Broker, and this cluster has none - declare a ReplicaBroker on this cluster (re-saving the Replication dialog does it automatically)' }
    if ([string]$broker.State -ne 'Online') { throw ('waiting for the Hyper-V Replica Broker to come online (currently ' + $broker.State + ')') }
  }
  if (-not $r) {
    Enable-VMReplication -VMName $vm -ReplicaServerName $server -ReplicaServerPort $port -AuthenticationType $auth -ReplicationFrequencySec $freq -ErrorAction Stop
    Start-VMInitialReplication -VMName $vm -ErrorAction Stop
    $changed = $true
  } else {
    if ([string]$r.ReplicaServer -ne $server -or [int]$r.FrequencySec -ne $freq) {
      Set-VMReplication -VMName $vm -ReplicaServerName $server -ReplicaServerPort $port -AuthenticationType $auth -ReplicationFrequencySec $freq -ErrorAction Stop
      $changed = $true
    }
    if ([string]$r.State -eq 'ReadyForInitialReplication') {
      Start-VMInitialReplication -VMName $vm -ErrorAction Stop
      $changed = $true
    }
  }
}
if ($changed) { 'RESULT=UPDATED' } else { 'RESULT=NOOP' }`,
		psQuote(vmName), psBool(spec.Enabled), psQuote(spec.ReplicaServer), port, psQuote(auth), freq)
	out, err := p.run(ctx, script)
	if err != nil {
		return OutcomeUnchanged, fmt.Errorf("ensure vm replication %q: %w", vmName, err)
	}
	if strings.Contains(string(out), "RESULT=UPDATED") {
		return OutcomeUpdated, nil
	}
	return OutcomeUnchanged, nil
}
