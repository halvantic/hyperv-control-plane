package hyperv

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// The boot-order scripts decide whether a VM's firmware matches desired state, so
// a false difference there reports a VM as never-settling. String assertions on
// the generated script cannot catch that — the defect this guards was a
// comparison that was structurally reasonable and wrong. These tests execute the
// script that actually ships, against stubbed cmdlets.
//
// Skips where powershell.exe is unavailable (non-Windows CI); the string-level
// tests in powershell_vm_test.go still run everywhere.
func runBootOrderScript(t *testing.T, harness string) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("needs powershell.exe")
	}
	exe, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe not on PATH")
	}
	out, err := exec.Command(exe, "-NoProfile", "-NonInteractive", "-Command", harness).CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	// The harness emits its result as the final line; Add-Type and the like write
	// warnings ahead of it that are not part of the outcome.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "PENDING=") {
			return l
		}
	}
	t.Fatalf("no result line in script output:\n%s", out)
	return ""
}

// gen2Harness stubs Get-VMFirmware with the given entries and Set-VMFirmware with
// a recorder, then runs the real script. entries is a PowerShell array literal of
// tagged boot entries; classification reads $_.Device's type name, so the fake
// device classes are declared with the names the script matches on.
func gen2Harness(entries, want string, running bool) string {
	run := "$false"
	if running {
		run = "$true"
	}
	return fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
Add-Type -TypeDefinition 'public class DvdDrive {} public class HardDiskDrive {} public class VMNetworkAdapter {}'
$script:applied = $null
function Get-VMFirmware { param($VMName) [pscustomobject]@{ BootOrder = $script:entries } }
function Set-VMFirmware { param($VMName, $BootOrder) $script:applied = @($BootOrder | ForEach-Object { $_.Tag }) }
$script:entries = @(%s)
$running = %s
$pending = $false
$pendingWhat = @()
$changed = $false
%s
$order = if ($script:applied) { $script:applied -join ',' } else { '' }
'PENDING=' + [bool]$pending + '|WHAT=' + ($pendingWhat -join '; ') + '|CHANGED=' + [bool]$changed + '|APPLIED=' + $order
`, entries, run, fmt.Sprintf(gen2BootOrderScript, "'Web01'", want))
}

func entry(tag, class string) string {
	dev := "$null"
	if class != "" {
		dev = "(New-Object " + class + ")"
	}
	return fmt.Sprintf("[pscustomobject]@{ Tag = '%s'; Device = %s }", tag, dev)
}

// The reported defect. A Gen 2 Windows VM's boot list starts with the UEFI file
// entry for the Windows Boot Manager, which has no .Device and classifies as
// 'Other'. The declared categories DVD, Drive, Network sit after it in exactly
// the declared order, so desired state is already satisfied and there is nothing
// to defer. This previously reported "want DVD,Drive,Network,Other, have
// Other,DVD,Drive,Network" on every pass, for ever.
func TestGen2BootOrderIgnoresUndeclaredEntryPosition(t *testing.T) {
	entries := strings.Join([]string{
		entry("bootmgr", ""), // the Windows Boot Manager file entry
		entry("dvd", "DvdDrive"),
		entry("disk", "HardDiskDrive"),
		entry("net", "VMNetworkAdapter"),
	}, ",")
	got := runBootOrderScript(t, gen2Harness(entries, "'DVD','Drive','Network'", true))
	if !strings.HasPrefix(got, "PENDING=False") {
		t.Fatalf("declared order DVD,Drive,Network is already satisfied; nothing should be pending\ngot: %s", got)
	}
}

// A genuine violation must still be reported, and must name only the declared
// categories — not the undeclared entry whose position is nobody's intent.
func TestGen2BootOrderReportsRealViolation(t *testing.T) {
	entries := strings.Join([]string{
		entry("bootmgr", ""),
		entry("disk", "HardDiskDrive"), // Drive before DVD: violates the declared order
		entry("dvd", "DvdDrive"),
		entry("net", "VMNetworkAdapter"),
	}, ",")
	got := runBootOrderScript(t, gen2Harness(entries, "'DVD','Drive','Network'", true))
	if !strings.HasPrefix(got, "PENDING=True") {
		t.Fatalf("Drive before DVD violates the declared order and must be reported\ngot: %s", got)
	}
	if !strings.Contains(got, "want DVD,Drive,Network, have Drive,DVD,Network") {
		t.Fatalf("the reported difference should cover only declared categories\ngot: %s", got)
	}
	if strings.Contains(got, "Other") {
		t.Fatalf("an undeclared entry must not appear in the difference\ngot: %s", got)
	}
}

// Applying the fix reorders the declared entries among the slots they already
// hold and leaves the undeclared entry exactly where it was.
func TestGen2BootOrderApplyKeepsUndeclaredEntryInPlace(t *testing.T) {
	entries := strings.Join([]string{
		entry("bootmgr", ""),
		entry("disk", "HardDiskDrive"),
		entry("dvd", "DvdDrive"),
		entry("net", "VMNetworkAdapter"),
	}, ",")
	got := runBootOrderScript(t, gen2Harness(entries, "'DVD','Drive','Network'", false))
	if !strings.Contains(got, "APPLIED=bootmgr,dvd,disk,net") {
		t.Fatalf("bootmgr must stay at its position while dvd/disk swap\ngot: %s", got)
	}
	if !strings.Contains(got, "CHANGED=True") {
		t.Fatalf("applying the order should mark the VM changed\ngot: %s", got)
	}
}

// A VM with only one declared entry has no relative order to enforce, so it must
// never report a difference regardless of where the undeclared entries sit.
func TestGen2BootOrderSingleDeclaredEntryIsNeverPending(t *testing.T) {
	entries := strings.Join([]string{
		entry("bootmgr", ""),
		entry("disk", "HardDiskDrive"),
	}, ",")
	got := runBootOrderScript(t, gen2Harness(entries, "'DVD','Drive','Network'", true))
	if !strings.HasPrefix(got, "PENDING=False") {
		t.Fatalf("one declared entry cannot be out of order\ngot: %s", got)
	}
}

// Gen 1 BIOS: same rule against the StartupOrder enum. Floppy is undeclared here,
// so its position is not the agent's to decide, and Set-VMBios still receives the
// complete four-device set.
func gen1Harness(startupOrder, want string, running bool) string {
	run := "$false"
	if running {
		run = "$true"
	}
	return fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$script:applied = $null
function Get-VMBios { param($VMName) [pscustomobject]@{ StartupOrder = @(%s) } }
function Set-VMBios { param($VMName, $StartupOrder) $script:applied = @($StartupOrder) }
$running = %s
$pending = $false
$pendingWhat = @()
$changed = $false
%s
$order = if ($script:applied) { $script:applied -join ',' } else { '' }
'PENDING=' + [bool]$pending + '|WHAT=' + ($pendingWhat -join '; ') + '|CHANGED=' + [bool]$changed + '|APPLIED=' + $order
`, startupOrder, run, fmt.Sprintf(gen1BootOrderScript, "'Legacy01'", want))
}

func TestGen1BootOrderIgnoresUndeclaredFloppyPosition(t *testing.T) {
	// Floppy first, then the declared CD before IDE — already satisfied.
	got := runBootOrderScript(t, gen1Harness("'Floppy','CD','IDE','LegacyNetworkAdapter'", "'DVD','Drive'", true))
	if !strings.HasPrefix(got, "PENDING=False") {
		t.Fatalf("CD before IDE satisfies the declared order; Floppy's position is undeclared\ngot: %s", got)
	}
}

func TestGen1BootOrderAppliesFullSetKeepingUndeclaredInPlace(t *testing.T) {
	got := runBootOrderScript(t, gen1Harness("'Floppy','IDE','CD','LegacyNetworkAdapter'", "'DVD','Drive'", false))
	// IDE and CD swap into the slots they already held; Floppy and the network
	// adapter do not move, and all four devices are still supplied.
	if !strings.Contains(got, "APPLIED=Floppy,CD,IDE,LegacyNetworkAdapter") {
		t.Fatalf("only the declared devices should move, and the full set must be supplied\ngot: %s", got)
	}
}
