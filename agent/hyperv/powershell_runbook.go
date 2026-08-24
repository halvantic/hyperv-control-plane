package hyperv

import (
	"fmt"
	"strconv"
	"strings"

	"context"
)

// Host-side steps a replication runbook needs: the isolated network a test
// failover runs inside, and the one-shot health probe that gates one group of a
// recovery against the next.
//
// Everything here runs on the DR-side host. Nothing here decides anything about
// the plan — the centre's runbook controller owns the ordering, the retries and
// the budgets; these are the individual questions it asks a host, and each of
// them answers only about the host it runs on.

// EnsureTestSwitch makes sure an isolated virtual switch exists for a test
// failover. switchType is "Private" (nothing can reach the test VMs and they can
// reach nothing) or "Internal" (the host, and only the host, gets an adapter on
// it so a TCP or ICMP gate can probe the copies).
//
// It REFUSES if a switch of that name exists and is external. That is the whole
// safety property of a test failover: the copies boot with the MAC and IP of
// machines that are still serving, and putting them on a switch with an uplink
// duplicates a live domain controller onto the production network. Silently
// reconfiguring somebody's external switch to make the test work would be worse
// than the test not running, so the refusal names the switch and what it is.
//
// Idempotent: an existing switch of the right type is left exactly as it is.
func (p *PowerShell) EnsureTestSwitch(ctx context.Context, name, switchType string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("ensure test switch: a switch name is required")
	}
	want := "Private"
	if strings.EqualFold(switchType, "Internal") {
		want = "Internal"
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$name = %[1]s
$want = %[2]s
$sw = Get-VMSwitch -Name $name -ErrorAction SilentlyContinue
if ($sw) {
  $have = [string]$sw.SwitchType
  if ($have -eq 'External') {
    throw ("refusing to use '" + $name + "' as a test network: it is an External switch carrying real traffic. " +
           "A test failover boots copies with the same addresses as the machines still serving, so it must run on an isolated switch. " +
           "Pick a different name for the bubble, or remove that switch if it is genuinely unused.")
  }
  if ($have -ne $want) {
    throw ("the switch '" + $name + "' already exists as " + $have + ", but this run asked for " + $want + ". " +
           "Changing it would affect anything else already using it, so it has been left alone - " +
           "either change the run to use " + $have + " isolation, or give the bubble a different name.")
  }
  'RESULT=exists'
} else {
  New-VMSwitch -Name $name -SwitchType $want | Out-Null
  'RESULT=created'
}`, psQuote(name), psQuote(want))

	out, err := p.run(ctx, script)
	if err != nil {
		return "", fmt.Errorf("ensure test switch %q: %w", name, err)
	}
	if strings.Contains(string(out), "RESULT=created") {
		return "created the isolated " + strings.ToLower(want) + " switch " + name, nil
	}
	return "the isolated switch " + name + " is already in place", nil
}

// RemoveTestSwitch removes a test bubble once a test has been torn down.
//
// It refuses on an external switch for the same reason EnsureTestSwitch does,
// and it refuses while VMs are still attached: a switch removed out from under a
// running VM leaves that VM with a disconnected adapter and no explanation. A
// switch that is already gone is a no-op, because tearing a test down twice must
// not fail.
func (p *PowerShell) RemoveTestSwitch(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("remove test switch: a switch name is required")
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$name = %[1]s
$sw = Get-VMSwitch -Name $name -ErrorAction SilentlyContinue
if (-not $sw) { 'RESULT=absent' } else {
  if ([string]$sw.SwitchType -eq 'External') {
    throw ("refusing to remove '" + $name + "': it is an External switch, not a test network")
  }
  $attached = @(Get-VMNetworkAdapter -VMName * -ErrorAction SilentlyContinue | Where-Object { [string]$_.SwitchName -eq $name })
  if ($attached.Count -gt 0) {
    $vms = (($attached | ForEach-Object { [string]$_.VMName } | Sort-Object -Unique) -join ', ')
    throw ("refusing to remove '" + $name + "': " + $attached.Count + " adapters are still connected to it (" + $vms + "). " +
           "Remove those VMs first, or the switch will be pulled out from under them.")
  }
  Remove-VMSwitch -Name $name -Force
  'RESULT=removed'
}`, psQuote(name))
	if _, err := p.run(ctx, script); err != nil {
		return fmt.Errorf("remove test switch %q: %w", name, err)
	}
	return nil
}

// HealthProbe answers one question about one VM, once, and says what it saw.
//
// One shot is deliberate. A gate that needs twenty minutes is twenty minutes of
// probes driven by the centre, not one job holding an agent worker open — so a
// centre restart mid-gate resumes the gate rather than stranding it, and a guest
// that never boots does not tie up the host that has to run the rest of the
// recovery.
//
// A failed probe is returned as an error carrying what actually happened
// ("connection refused", "no reply in 4s"), because that string is what the
// console shows an operator watching a tier that will not come up. "The probe
// failed" would tell them nothing they did not already know.
func (p *PowerShell) HealthProbe(ctx context.Context, vmName, check, address string, port, expectExit int, script string) (string, error) {
	switch {
	case strings.TrimSpace(vmName) == "" && check == "Heartbeat":
		return "", fmt.Errorf("health probe: a VM name is required for a heartbeat check")
	case check == "TCP" && (port <= 0 || port > 65535):
		return "", fmt.Errorf("health probe: a TCP check needs a port, got %d", port)
	case check == "Script" && strings.TrimSpace(script) == "":
		return "", fmt.Errorf("health probe: a script check needs a script path")
	case (check == "TCP" || check == "ICMP") && strings.TrimSpace(address) == "":
		return "", fmt.Errorf("health probe: a %s check needs an address to probe", check)
	}

	var ps string
	switch check {
	case "Heartbeat":
		// The integration-services heartbeat is the only gate that works with no
		// guest network at all, which makes it the one that can gate a
		// cross-subnet failover before the guest has been re-addressed — and the
		// only one that can gate a copy inside a private bubble.
		//
		// OkApplicationsUnknown is accepted alongside Ok: it means the heartbeat
		// is healthy and the guest simply has not reported application state,
		// which is what a Server Core guest with no reporting agent looks like
		// for ever. Treating it as unhealthy would hold every such tier until it
		// timed out.
		ps = fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$vm = Get-VM -Name %[1]s -ErrorAction SilentlyContinue
if (-not $vm) { throw ('there is no VM called ' + %[1]s + ' on this host') }
if ([string]$vm.State -ne 'Running') { throw (%[1]s + ' is ' + [string]$vm.State + ', not Running') }
$hb = [string]$vm.Heartbeat
if (-not $hb) { throw (%[1]s + ' is running but reports no heartbeat yet - integration services may still be starting, or may not be installed in the guest') }
if ($hb -ne 'Ok' -and $hb -ne 'OkApplicationsUnknown') { throw ('heartbeat is ' + $hb) }
'RESULT=' + $hb`, psQuote(vmName))

	case "TCP":
		ps = fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$addr = %[1]s
$port = %[2]d
$c = New-Object System.Net.Sockets.TcpClient
try {
  $iar = $c.BeginConnect($addr, $port, $null, $null)
  if (-not $iar.AsyncWaitHandle.WaitOne(4000, $false)) { throw ($addr + ':' + $port + ' did not answer within 4s') }
  $c.EndConnect($iar)
} catch {
  throw ($addr + ':' + $port + ' - ' + $_.Exception.Message)
} finally { $c.Close() }
'RESULT=open'`, psQuote(address), port)

	case "ICMP":
		ps = fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$addr = %[1]s
if (-not (Test-Connection -ComputerName $addr -Count 1 -Quiet -ErrorAction SilentlyContinue)) {
  throw ($addr + ' did not answer a ping')
}
'RESULT=reply'`, psQuote(address))

	case "Script":
		// The script's own exit code is the answer, and its last line of output is
		// carried back so a gate can explain itself ("waiting for AD replication
		// to converge") rather than only saying it failed.
		ps = fmt.Sprintf(`$ErrorActionPreference = 'Continue'
$path = %[1]s
if (-not (Test-Path -LiteralPath $path)) { throw ('the gate script ' + $path + ' does not exist on this host') }
$out = & $path %[2]s 2>&1
$code = $LASTEXITCODE
if ($null -eq $code) { $code = 0 }
$tail = ''
if ($out) { $tail = ([string[]]$out)[-1] }
if ($code -ne %[3]d) { throw ('the gate script exited ' + $code + ' (expected %[3]d)' + $(if ($tail) { ': ' + $tail } else { '' })) }
'RESULT=' + $(if ($tail) { $tail } else { 'exit ' + $code })`,
			psQuote(script), psQuote(vmName), expectExit)

	default:
		return "", fmt.Errorf("health probe: unknown check %q", check)
	}

	out, err := p.run(ctx, ps)
	if err != nil {
		// The agent's own message is what the console shows, so it names the VM
		// and the check rather than leaving the operator to infer both.
		return "", fmt.Errorf("%s check on %s: %w", strings.ToLower(check), probeSubject(vmName, address, port), err)
	}
	return probeDetail(check, vmName, address, port, string(out)), nil
}

// probeSubject names what was probed, for a failure message.
func probeSubject(vmName, address string, port int) string {
	if address != "" {
		if port > 0 {
			return address + ":" + strconv.Itoa(port)
		}
		return address
	}
	return vmName
}

// probeDetail says what a passing probe actually saw, so a settled gate reads as
// evidence rather than as an assertion.
func probeDetail(check, vmName, address string, port int, out string) string {
	result := ""
	for _, line := range strings.Split(out, "\n") {
		if v, found := strings.CutPrefix(strings.TrimSpace(line), "RESULT="); found {
			result = v
		}
	}
	switch check {
	case "Heartbeat":
		return vmName + " is running and its heartbeat reports " + result
	case "TCP":
		return address + ":" + strconv.Itoa(port) + " answered"
	case "ICMP":
		return address + " answered a ping"
	case "Script":
		if result != "" {
			return "the gate script reports healthy: " + result
		}
		return "the gate script reports healthy"
	}
	return result
}
