package hyperv

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* Read before write: VLAN, static MAC, MAC spoofing and the start action.

   These were written on every pass whether or not they differed: eight VMMS
   modify calls per pass for each four-NIC VM on HVNEW06, all of them no-ops.
   Now each reads first and writes only on a difference.

   The risk is the comparison, not the idea. A check that is structurally
   reasonable and wrong either skips a write that was needed or, if it ever
   counted as a change, reports the VM changed on every pass for ever. String
   assertions cannot catch either, so these run the fragments that actually ship
   against stubbed cmdlets. The stubs return real enums where Hyper-V does, so
   the [string] comparisons are exercised against the types they will meet. */

func runGuardHarness(t *testing.T, setup, fragment string) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("needs powershell.exe")
	}
	exe, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe not on PATH")
	}
	// READS records every adapter or VLAN read the fragment makes, so a test can
	// hold it to the reads it is meant to save. $script:fresh is what a re-read
	// of the adapter returns, for the cases where one is owed.
	harness := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
Add-Type -TypeDefinition 'namespace BallastTest { public enum VlanMode { Untagged, Access, Trunk } public enum OnOff { On, Off } public enum StartAction { Nothing, StartIfRunning, Start } }'
$script:writes = @()
$script:reads = @()
$script:vlanThrows = $false
$script:fresh = @()
$changed = $false
$__wrote = $false
$__touched = $false
$__vlanById = @{}
$__ads = @()
$ads = @()
function Get-VMNetworkAdapterVlan { [CmdletBinding()] param($VMNetworkAdapter)
  $script:reads += 'vlan'
  if ($script:vlanThrows) { throw 'the VLAN setting could not be read' }
  $VMNetworkAdapter.Vlan }
function Set-VMNetworkAdapterVlan { [CmdletBinding()] param($VMName, $VMNetworkAdapterName, [switch]$Access, $VlanId, [switch]$Untagged)
  $script:writes += 'vlan' }
function Get-VMNetworkAdapter { [CmdletBinding()] param($VMName, $Name)
  $script:reads += 'nic'
  $script:fresh }
function Add-VMNetworkAdapter { [CmdletBinding()] param($VMName, $Name, $SwitchName)
  $script:writes += 'add' }
function Connect-VMNetworkAdapter { [CmdletBinding()] param($VMName, $Name, $SwitchName)
  $script:writes += 'connect' }
function Set-VMNetworkAdapter { [CmdletBinding()] param($VMName, $Name, $VMNetworkAdapter, $StaticMacAddress, $MacAddressSpoofing)
  if ($PSBoundParameters.ContainsKey('StaticMacAddress')) { $script:writes += 'mac' } else { $script:writes += 'spoof' } }
function Set-VM { [CmdletBinding()] param($Name, $AutomaticStartAction)
  $script:writes += 'start' }
%s
%s
'RESULT=WRITES:' + ($script:writes -join ',') + '|READS:' + ($script:reads -join ',') + '|WROTE:' + [bool]$__wrote + '|TOUCHED:' + [bool]$__touched + '|CHANGED:' + [bool]$changed
`, setup, fragment)
	out, err := exec.Command(exe, "-NoProfile", "-NonInteractive", "-Command", harness).CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "RESULT=") {
			return l
		}
	}
	t.Fatalf("no result line in script output:\n%s", out)
	return ""
}

func vlanSetting(mode string, id int) string {
	return fmt.Sprintf("[pscustomobject]@{ OperationMode = [BallastTest.VlanMode]::%s; AccessVlanId = %d }", mode, id)
}

// vnic is a fake VM network adapter named net0 with the given Id. Its own VLAN
// read (the stubbed Get-VMNetworkAdapterVlan) returns vlan; mapped also puts
// vlan in the VM's VLAN map under that Id, as the one-call read does.
func vnic(id, vlan string, mapped bool) (adapter, setup string) {
	adapter = fmt.Sprintf("[pscustomobject]@{ Name = 'net0'; Id = '%s'; SwitchName = 'sw'; Vlan = %s }", id, vlan)
	if mapped {
		setup = fmt.Sprintf("$__vlanById['%s'] = %s; ", id, vlan)
	}
	return adapter, setup
}

// one builds a setup holding a single adapter.
func one(id, vlan string, mapped bool) string {
	a, s := vnic(id, vlan, mapped)
	return s + "$ads = @(" + a + ")"
}

func TestVLANIsWrittenOnlyWhenItDiffers(t *testing.T) {
	a41, m41 := vnic("id-a", vlanSetting("Access", 41), true)
	a40, m40 := vnic("id-b", vlanSetting("Access", 40), true)
	for _, tc := range []struct {
		name      string
		want      int
		setup     string
		write     bool
		readsVLAN bool
	}{
		// From the VM's VLAN map: no read spent on the NIC.
		{"access already on the tag", 41, one("id-a", vlanSetting("Access", 41), true), false, false},
		// Missing from the map: the NIC reads its own, and still settles.
		{"not in the map", 41, one("id-a", vlanSetting("Access", 41), false), false, true},
		// Added or reconnected this pass: the map predates that, so it is not used,
		// even though here it would have said the VLAN matched.
		{"touched this pass", 41, "$__touched = $true; " + one("id-a", vlanSetting("Access", 41), true), false, true},
		{"touched, and its own reading differs", 41,
			"$__touched = $true; $__vlanById['id-a'] = " + vlanSetting("Access", 41) + "; $ads = @([pscustomobject]@{ Name = 'net0'; Id = 'id-a'; Vlan = " + vlanSetting("Access", 40) + " })", true, true},
		{"access on another tag", 41, one("id-a", vlanSetting("Access", 40), true), true, false},
		{"untagged, wants a tag", 41, one("id-a", vlanSetting("Untagged", 0), true), true, false},
		{"untagged already", 0, one("id-a", vlanSetting("Untagged", 0), true), false, false},
		{"tagged, wants untagged", 0, one("id-a", vlanSetting("Access", 41), true), true, false},
		{"trunk, wants untagged", 0, one("id-a", vlanSetting("Trunk", 0), false), true, true},
		// Unreadable is not "matches". It writes, as every pass did before.
		{"own read throws", 41, "$script:vlanThrows = $true; " + one("id-a", "$null", false), true, true},
		{"no adapter", 41, "$ads = @()", true, false},
		// An adapter with no Id cannot be looked up, so it reads its own.
		{"no Id", 41, "$ads = @([pscustomobject]@{ Name = 'net0'; Vlan = " + vlanSetting("Access", 41) + " })", false, true},
		// The write applies to every adapter of the name, so every one is checked.
		{"one of two same-named adapters differs", 41, m41 + m40 + "$ads = @(" + a41 + ", " + a40 + ")", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runGuardHarness(t, tc.setup, vlanFragment("'Web01'", "'net0'", tc.want))
			if wrote := strings.Contains(got, "WRITES:vlan"); wrote != tc.write {
				t.Fatalf("wrote=%v, want %v\n%s", wrote, tc.write, got)
			}
			if read := strings.Contains(got, "READS:vlan"); read != tc.readsVLAN {
				t.Fatalf("read the VLAN=%v, want %v\n%s", read, tc.readsVLAN, got)
			}
			// A write marks the NIC for a fresh read before spoofing acts on it.
			if tc.write && !strings.Contains(got, "WROTE:True") {
				t.Fatalf("a VLAN write must mark the adapter written:\n%s", got)
			}
			if !strings.HasSuffix(got, "CHANGED:False") {
				t.Fatalf("a VLAN write must not count as a change, as it never did:\n%s", got)
			}
		})
	}
}

func nic(mac string, dynamic bool, spoof, sw string) string {
	return fmt.Sprintf("[pscustomobject]@{ Name = 'net0'; MacAddress = '%s'; DynamicMacAddressEnabled = $%t; MacAddressSpoofing = [BallastTest.OnOff]::%s; SwitchName = '%s' }",
		mac, dynamic, spoof, sw)
}

func TestStaticMACIsWrittenOnlyWhenItDiffers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup string
		write bool
	}{
		// Hyper-V reports bare hex; the spec may be written with separators and in
		// lower case. The same address in two spellings is not a difference.
		{"same address, different spelling", "$ads = @(" + nic("00155D010203", false, "Off", "sw") + ")", false},
		{"still dynamic", "$ads = @(" + nic("00155D010203", true, "Off", "sw") + ")", true},
		{"another address", "$ads = @(" + nic("00155D0102FF", false, "Off", "sw") + ")", true},
		{"no adapter", "$ads = @()", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runGuardHarness(t, tc.setup, staticMACFragment("'Web01'", "'net0'", "00-15-5d-01-02-03"))
			if wrote := strings.Contains(got, "WRITES:mac"); wrote != tc.write {
				t.Fatalf("wrote=%v, want %v\n%s", wrote, tc.write, got)
			}
			if !strings.Contains(got, "READS:|") {
				t.Fatalf("the MAC check reads the adapter already in hand, not the host:\n%s", got)
			}
			if tc.write && !strings.Contains(got, "WROTE:True") {
				t.Fatalf("a MAC write must mark the adapter written:\n%s", got)
			}
			if !strings.HasSuffix(got, "CHANGED:False") {
				t.Fatalf("a MAC write must not count as a change, as it never did:\n%s", got)
			}
		})
	}
}

func TestMACSpoofingIsWrittenOnlyWhenItDiffers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		want  string
		setup string
		write bool
		reads bool
	}{
		{"on already", "On", "$ads = @(" + nic("00155D010203", true, "On", "sw") + ")", false, false},
		{"off, wants on", "On", "$ads = @(" + nic("00155D010203", true, "Off", "sw") + ")", true, false},
		{"off already", "Off", "$ads = @(" + nic("00155D010203", true, "Off", "sw") + ")", false, false},
		{"on, wants off", "Off", "$ads = @(" + nic("00155D010203", true, "On", "sw") + ")", true, false},
		// No switch, no port, nothing to set: explained, not attempted.
		{"disconnected", "On", "$ads = @(" + nic("00155D010203", true, "Off", "") + ")", false, false},
		// After a write this pass the adapter in hand is from before it, so it is
		// read again and the fresh reading decides.
		{"stale after a write, fresh says on", "On",
			"$__wrote = $true; $ads = @(" + nic("00155D010203", true, "Off", "sw") + "); $script:fresh = @(" + nic("00155D010203", true, "On", "sw") + ")", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runGuardHarness(t, tc.setup, fmt.Sprintf(spoofScript, "'Web01'", "'net0'", psQuote(tc.want)))
			if wrote := strings.Contains(got, "WRITES:spoof"); wrote != tc.write {
				t.Fatalf("wrote=%v, want %v\n%s", wrote, tc.write, got)
			}
			if read := strings.Contains(got, "READS:nic"); read != tc.reads {
				t.Fatalf("re-read the adapter=%v, want %v\n%s", read, tc.reads, got)
			}
			if !strings.HasSuffix(got, "CHANGED:False") {
				t.Fatalf("a spoofing write must not count as a change, as it never did:\n%s", got)
			}
		})
	}
}

// Finding the NIC costs nothing when it is there and on its switch; adding or
// reconnecting it counts as a change and reads it back, because what follows
// acts on the adapter object.
func TestAdapterSelectReadsOnlyAfterAWrite(t *testing.T) {
	connected := "[pscustomobject]@{ Name = 'net0'; SwitchName = 'sw' }"
	for _, tc := range []struct {
		name    string
		setup   string
		writes  string
		reads   string
		changed bool
	}{
		{"present and connected", "$__ads = @(" + connected + ")", "", "", false},
		{"absent", "$__ads = @([pscustomobject]@{ Name = 'net1'; SwitchName = 'sw' })", "add", "nic", true},
		{"on another switch", "$__ads = @([pscustomobject]@{ Name = 'net0'; SwitchName = 'other' })", "connect", "nic", true},
		{"disconnected", "$__ads = @([pscustomobject]@{ Name = 'net0'; SwitchName = $null })", "connect", "nic", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runGuardHarness(t, tc.setup+"; $script:fresh = @("+connected+")", fmt.Sprintf(adapterSelectScript, "'Web01'", "'net0'", "'sw'"))
			if !strings.Contains(got, "WRITES:"+tc.writes+"|") {
				t.Fatalf("writes: want %q\n%s", tc.writes, got)
			}
			if !strings.Contains(got, "READS:"+tc.reads+"|") {
				t.Fatalf("reads: want %q\n%s", tc.reads, got)
			}
			if tc.changed != strings.HasSuffix(got, "CHANGED:True") {
				t.Fatalf("changed: want %v\n%s", tc.changed, got)
			}
			// The re-read has happened, so nothing later is owed another.
			if !strings.Contains(got, "WROTE:False") {
				t.Fatalf("the select must leave the adapter fresh:\n%s", got)
			}
			// A NIC added or reconnected must not take its VLAN from the map.
			if touched := strings.Contains(got, "TOUCHED:True"); touched != tc.changed {
				t.Fatalf("touched=%v, want %v\n%s", touched, tc.changed, got)
			}
		})
	}
}

// In the script that ships, the VM's adapters are read once, and every other
// adapter read is conditional on a write having happened to that NIC.
func TestEnsureVMScriptReadsTheAdaptersOnce(t *testing.T) {
	vm := nestedVM(true)
	vm.Spec.NetworkAdapters[0].MACAddress = "00-15-5D-01-02-03"
	vm.Spec.NetworkAdapters[1].VLANID = 41
	s := psCode(newTestPS(&fakeRunner{}).ensureVMScript(vm, 2))

	unconditional := 0
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if !strings.Contains(l, "Get-VMNetworkAdapter ") {
			continue
		}
		switch {
		case strings.HasPrefix(l, "$__ads = @(Get-VMNetworkAdapter -VMName 'Web01')"):
			unconditional++
		case strings.HasPrefix(l, "if ($__wrote)"):
		default:
			t.Errorf("an adapter read that is not owed by a write: %s", l)
		}
	}
	if unconditional != 1 {
		t.Fatalf("the VM's adapters must be read exactly once, got %d", unconditional)
	}
	// Every NIC's VLAN in one call; a NIC reads its own only as the fallback.
	if got := strings.Count(s, "Get-VMNetworkAdapterVlan -VMName 'Web01'"); got != 1 {
		t.Fatalf("the VM's VLANs must be read in one call, got %d", got)
	}
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "Get-VMNetworkAdapterVlan -VMNetworkAdapter") && !strings.Contains(l, "$__vlanById.ContainsKey($__k)) { $__vlanById[$__k] } else {") {
			t.Errorf("a per-NIC VLAN read outside the fallback: %s", strings.TrimSpace(l))
		}
	}
	if !strings.Contains(s, "foreach ($ad in $__ads)") {
		t.Errorf("the removal sweep must use the list already read:\n%s", s)
	}
}

func TestStartActionIsWrittenOnlyWhenItDiffers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		have  string
		want  types.VMStartAction
		write bool
	}{
		{"matches", "StartIfRunning", types.VMStartIfWasRunning, false},
		{"differs", "Nothing", types.VMStartIfWasRunning, true},
		{"always, matches", "Start", types.VMStartAlways, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := "$cur = [pscustomobject]@{ AutomaticStartAction = [BallastTest.StartAction]::" + tc.have + " }"
			got := runGuardHarness(t, setup, fmt.Sprintf(startActionScript, "'Web01'", psQuote(automaticStartActionValue(tc.want))))
			wrote := strings.Contains(got, "WRITES:start")
			if wrote != tc.write {
				t.Fatalf("wrote=%v, want %v\n%s", wrote, tc.write, got)
			}
			if !strings.HasSuffix(got, "CHANGED:False") {
				t.Fatalf("a start-action write must not count as a change, as it never did:\n%s", got)
			}
		})
	}
}

// The script that ships uses the guarded fragments, and none of the four writes
// is left unconditional anywhere in it. Runs everywhere, unlike the harness.
func TestEnsureVMScriptHasNoUnconditionalNoOpWrites(t *testing.T) {
	vm := types.VM{
		Meta: types.ObjectMeta{Name: "Web01"},
		Spec: types.VMSpec{
			MemoryStartupBytes:   4294967296,
			NestedVirtualisation: true,
			AutomaticStartAction: types.VMStartIfWasRunning,
			NetworkAdapters: []types.VMNetworkAdapterSpec{
				{Name: "net0", SwitchName: "ConvergedSwitch", VLANID: 41, MACAddress: "00-15-5D-01-02-03"},
				{Name: "net1", SwitchName: "ConvergedSwitch"},
			},
		},
	}
	s := psCode(newTestPS(&fakeRunner{}).ensureVMScript(vm, 2))
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		for write, guard := range map[string]string{
			"Set-VMNetworkAdapterVlan":                   "if (-not $vlanOK)",
			"-StaticMacAddress":                          "if (-not $macOK)",
			"-MacAddressSpoofing":                        "if ([string]$ad.MacAddressSpoofing -ne",
			"Set-VM -Name 'Web01' -AutomaticStartAction": "if ([string]$cur.AutomaticStartAction -ne",
		} {
			if strings.Contains(l, write) && !strings.HasPrefix(l, guard) {
				t.Errorf("%s is written without reading first: %s", write, l)
			}
		}
	}
	for _, want := range []string{
		"-Access -VlanId 41",
		"-Untagged",
		"-StaticMacAddress '00-15-5D-01-02-03'",
		"-ne '00155D010203'",
		"-AutomaticStartAction 'StartIfRunning'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
}

/*
Get-VMProcessor is read once per VM, not once per check.

	466ms on HVNEW06, the slowest read in the script, and it was taken twice:
	once for the processor count and again for nested virtualisation. Both
	checks now use one reading taken ahead of them.
*/
func TestEnsureVMScriptReadsTheProcessorOnce(t *testing.T) {
	vm := nestedVM(true)
	vm.Spec.ProcessorCount = 4
	s := psCode(newTestPS(&fakeRunner{}).ensureVMScript(vm, 2))

	if got := strings.Count(s, "Get-VMProcessor"); got != 1 {
		t.Fatalf("Get-VMProcessor: found %d, want 1:\n%s", got, s)
	}
	read := strings.Index(s, "$vp = Get-VMProcessor -VMName 'Web01'")
	count := strings.Index(s, "if ($vp.Count -ne 4)")
	nested := strings.Index(s, "if ($null -ne $vp.ExposeVirtualizationExtensions)")
	if read < 0 || count < 0 || nested < 0 {
		t.Fatalf("both checks must use the one reading (read=%d count=%d nested=%d):\n%s", read, count, nested, s)
	}
	if read > count || read > nested {
		t.Fatalf("the processor must be read before either check uses it:\n%s", s)
	}
}

// The marks are cumulative; the log wants each section's own cost, and what the
// call spent outside the script.
func TestEnsureTimingFieldsAreDeltas(t *testing.T) {
	ms := map[string]int64{"lookup": 100, "cpumem": 150, "firmware": 150, "video": 150, "nested": 160,
		"start": 160, "disks": 400, "adapters": 900, "iso": 950, "boot": 950}
	got := fmt.Sprint(ensureTimingFields(ms, 1200*time.Millisecond))
	want := "[lookup 100 cpumem 50 firmware 0 video 0 nested 10 start 0 disks 240 adapters 500 iso 50 boot 0 script 950 outside 250]"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestNormaliseMAC(t *testing.T) {
	for in, want := range map[string]string{
		"00-15-5d-01-02-03": "00155D010203",
		"00:15:5D:01:02:03": "00155D010203",
		"00155D010203":      "00155D010203",
		"":                  "",
	} {
		if got := normaliseMAC(in); got != want {
			t.Errorf("normaliseMAC(%q) = %q, want %q", in, got, want)
		}
	}
}
