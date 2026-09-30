package hyperv

import (
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

/* The guest-IP and replication reads moved from per-object cmdlets to one WMI
   query each. A reading that is wrong or missing here is worse than a slow one,
   so these run the shipped fragments against stubbed cmdlets and hold them to
   three things: the same answer as the cmdlet, the cmdlet's ORDER for IPs
   (a runbook gate probes the first routable IPv4), and falling back to the
   cmdlet whenever WMI cannot answer for certain. */

func runWMIHarness(t *testing.T, script string) []string {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("needs powershell.exe")
	}
	exe, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe not on PATH")
	}
	out, err := exec.Command(exe, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		lines = append(lines, strings.TrimSpace(l))
	}
	return lines
}

func resultLine(t *testing.T, lines []string, prefix string) string {
	t.Helper()
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return strings.TrimPrefix(l, prefix)
		}
	}
	t.Fatalf("no %s line in output:\n%s", prefix, strings.Join(lines, "\n"))
	return ""
}

const cimStub = `$ErrorActionPreference = 'Stop'
$script:gn = @(); $script:cs = @(); $script:cimThrows = $false
function Get-CimInstance { [CmdletBinding()] param($Namespace, $ClassName)
  if ($script:cimThrows) { throw 'WMI is not answering' }
  if ($ClassName -eq 'Msvm_GuestNetworkAdapterConfiguration') { $script:gn } else { $script:cs } }
`

// Two NICs on one VM, whose adapter objects carry their OWN (different) IPs so
// the test can tell which source answered.
const twoNICs = `$ads = @(
  [pscustomobject]@{ Id = 'Microsoft:aaaaaaaa-0000-0000-0000-000000000001\NIC-A'; IPAddresses = @('9.9.9.1') },
  [pscustomobject]@{ Id = 'Microsoft:aaaaaaaa-0000-0000-0000-000000000001\NIC-B'; IPAddresses = @('9.9.9.2') })
`

func TestGuestIPsComeFromWMIInAdapterOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup string
		want  string
	}{
		// WMI lists NIC-B first; the answer keeps the adapters' order, and upper
		// and lower case GUIDs are the same key.
		{"from WMI, adapter order", `$script:gn = @(
  [pscustomobject]@{ InstanceID = 'Microsoft:GuestNetwork\AAAAAAAA-0000-0000-0000-000000000001\NIC-B'; IPAddresses = @('10.0.60.60') },
  [pscustomobject]@{ InstanceID = 'Microsoft:GuestNetwork\AAAAAAAA-0000-0000-0000-000000000001\NIC-A'; IPAddresses = @('192.168.1.12', 'fd00::12') })`,
			"192.168.1.12,fd00::12,10.0.60.60"},
		// A NIC WMI has no record of reads its own.
		{"one NIC missing from WMI", `$script:gn = @(
  [pscustomobject]@{ InstanceID = 'Microsoft:GuestNetwork\AAAAAAAA-0000-0000-0000-000000000001\NIC-A'; IPAddresses = @('192.168.1.12') })`,
			"192.168.1.12,9.9.9.2"},
		// A record with no addresses is an answer (the guest reports none), not a
		// reason to ask again.
		{"WMI says no addresses", `$script:gn = @(
  [pscustomobject]@{ InstanceID = 'Microsoft:GuestNetwork\AAAAAAAA-0000-0000-0000-000000000001\NIC-A'; IPAddresses = @() },
  [pscustomobject]@{ InstanceID = 'Microsoft:GuestNetwork\AAAAAAAA-0000-0000-0000-000000000001\NIC-B'; IPAddresses = @() })`,
			""},
		// WMI not answering: every NIC reads its own, as before.
		{"WMI throws", `$script:cimThrows = $true`, "9.9.9.1,9.9.9.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := runWMIHarness(t, cimStub+tc.setup+"\n"+guestIPsScript+twoNICs+
				`'IPS=' + (@(foreach ($a in $ads) { __ips $a }) -join ',')`)
			if got := resultLine(t, lines, "IPS="); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReplicationIsSkippedOnlyOnADefiniteNone(t *testing.T) {
	const host = `[pscustomobject]@{ Name = 'HVNEW06'; ReplicationMode = $null }`
	vm := func(n, mode string) string {
		return `[pscustomobject]@{ Name = 'AAAAAAAA-0000-0000-0000-00000000000` + n + `'; ReplicationMode = ` + mode + ` }`
	}
	for _, tc := range []struct {
		name  string
		setup string
		want  string // __replicated for VMs 1, 2, 3
	}{
		// The host's own entry has no mode and must not count; VM 3 is absent from
		// the answer, so it is asked about.
		{"mode known per VM", "$script:cs = @(" + host + ", " + vm("1", "0") + ", " + vm("2", "1") + ")", "False,True,True"},
		// One VM without a mode means WMI cannot be taken at its word for any.
		{"a VM with no mode", "$script:cs = @(" + vm("1", "0") + ", " + vm("2", "$null") + ")", "True,True,True"},
		{"WMI throws", "$script:cimThrows = $true", "True,True,True"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := runWMIHarness(t, cimStub+tc.setup+"\n"+replicationModeScript+
				`'REPL=' + ((@('aaaaaaaa-0000-0000-0000-000000000001', 'aaaaaaaa-0000-0000-0000-000000000002', 'aaaaaaaa-0000-0000-0000-000000000003') | ForEach-Object { [string](__replicated $_) }) -join ',')`)
			if got := resultLine(t, lines, "REPL="); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// The whole live-read script as it ships: IPs from WMI in adapter order, and
// Get-VMReplication not called at all when WMI says nothing is replicated.
func TestVMLiveScriptEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name      string
		modes     string
		replCalls string
	}{
		{"nothing replicated", "0", "0"},
		{"one VM replicated", "1", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubs := cimStub + `$script:replCalls = 0
function Get-VM { [CmdletBinding()] param()
  [pscustomobject]@{ Name = 'Web01'; Id = [guid]'aaaaaaaa-0000-0000-0000-000000000001'; State = 'Running'; MemoryAssigned = 4; MemoryDemand = 2; MemoryStatus = 'OK'; CPUUsage = 5; Uptime = [timespan]::FromSeconds(60) } }
function Get-VMReplication { [CmdletBinding()] param() $script:replCalls++ }
function Get-VMNetworkAdapter { [CmdletBinding()] param($VMName)
  [pscustomobject]@{ VMName = 'Web01'; Id = 'Microsoft:AAAAAAAA-0000-0000-0000-000000000001\NIC-A'; IPAddresses = @('9.9.9.1') }
  [pscustomobject]@{ VMName = 'Web01'; Id = 'Microsoft:AAAAAAAA-0000-0000-0000-000000000001\NIC-B'; IPAddresses = @('9.9.9.2') } }
$script:cs = @([pscustomobject]@{ Name = 'AAAAAAAA-0000-0000-0000-000000000001'; ReplicationMode = ` + tc.modes + ` })
$script:gn = @(
  [pscustomobject]@{ InstanceID = 'Microsoft:GuestNetwork\AAAAAAAA-0000-0000-0000-000000000001\NIC-B'; IPAddresses = @('10.0.60.60') },
  [pscustomobject]@{ InstanceID = 'Microsoft:GuestNetwork\AAAAAAAA-0000-0000-0000-000000000001\NIC-A'; IPAddresses = @('192.168.1.12', 'fe80::1') })
`
			lines := runWMIHarness(t, stubs+vmLiveScript([]string{"Web01"})+"\n'REPLCALLS=' + $script:replCalls")
			if got := resultLine(t, lines, "REPLCALLS="); got != tc.replCalls {
				t.Fatalf("Get-VMReplication called %s times, want %s", got, tc.replCalls)
			}
			var obs map[string]vmLiveObs
			for _, l := range lines {
				if strings.HasPrefix(l, "{") {
					if err := json.Unmarshal([]byte(l), &obs); err != nil {
						t.Fatalf("%v\n%s", err, l)
					}
				}
			}
			// Adapter order (NIC-A then NIC-B), link-local dropped, from WMI.
			if got := obs["web01"].IPAddress; got != "192.168.1.12, 10.0.60.60" {
				t.Fatalf("ipAddress %q, want %q", got, "192.168.1.12, 10.0.60.60")
			}
		})
	}
}
