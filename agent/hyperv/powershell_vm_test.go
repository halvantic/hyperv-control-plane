package hyperv

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

// A VM with a disk on a datastore is created in a per-VM folder: the script
// derives the disk's parent directory, creates it, and points New-VM -Path at it
// — so the VM's config lands on the datastore alongside its disk rather than on
// the host's default VM path (which may be unset or missing → New-VM 0x80070002).
func TestEnsureVMScriptCreatesPerVMFolder(t *testing.T) {
	vm := types.VM{
		Meta: types.ObjectMeta{Name: "Web01"},
		Spec: types.VMSpec{
			MemoryStartupBytes: 4294967296,
			Disks:              []types.VMDiskSpec{{Path: `C:\ClusterStorage\vol1\Web01\Web01.vhdx`, SizeBytes: 64424509440, Dynamic: true}},
		},
	}
	s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 2)
	if !strings.Contains(s, `Split-Path 'C:\ClusterStorage\vol1\Web01\Web01.vhdx' -Parent`) {
		t.Fatalf("script does not derive the VM folder from the disk:\n%s", s)
	}
	if !strings.Contains(s, "New-Item -ItemType Directory -Path $vmDir -Force") {
		t.Fatalf("script does not create the VM folder:\n%s", s)
	}
	if !strings.Contains(s, "New-VM -Name 'Web01' -Generation 2 -MemoryStartupBytes 4294967296 -NoVHD -Path $vmDir") {
		t.Fatalf("New-VM does not place config in the VM folder (-Path $vmDir):\n%s", s)
	}
}

// A host whose Hyper-V cannot enumerate VMs must fail the reconcile, not report
// the VM as already matching desired state. One corrupt VM registration makes
// Get-VM throw for EVERY VM on that host; the old lookup swallowed that with
// -ErrorAction SilentlyContinue, the cluster-group check then read the null as
// "owned by another node", and the pass returned unchanged — so a blind host
// reported every clustered VM green while nothing could be read or applied.
func TestEnsureVMScriptDistinguishesMissingFromBrokenHyperV(t *testing.T) {
	vm := types.VM{
		Meta: types.ObjectMeta{Name: "Web01"},
		Spec: types.VMSpec{MemoryStartupBytes: 4294967296},
	}
	s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 2)

	// The lookup must be a guarded Stop, not a silent swallow.
	if strings.Contains(s, `Get-VM -Name 'Web01' -ErrorAction SilentlyContinue`) {
		t.Error("VM lookup still swallows every error; a broken host reads as 'VM absent'")
	}
	if !strings.Contains(s, `try { $vm = Get-VM -Name 'Web01' -ErrorAction Stop }`) {
		t.Errorf("VM lookup should be a guarded -ErrorAction Stop:\n%s", s)
	}
	// Only a genuine "not found" may be treated as absent; anything else throws.
	for _, want := range []string{
		`$msg -notlike '*unable to find*'`,
		"cannot enumerate VMs on this host",
		"throw (",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q — a Hyper-V fault must be raised, not ignored:\n%s", want, s)
		}
	}
}

// A VM with no disk has no datastore folder to derive, so it falls back to the
// host default path (no -Path) — behaviour unchanged.
func TestEnsureVMScriptNoDiskNoPath(t *testing.T) {
	vm := types.VM{
		Meta: types.ObjectMeta{Name: "Web01"},
		Spec: types.VMSpec{MemoryStartupBytes: 4294967296},
	}
	s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 2)
	if strings.Contains(s, "-Path $vmDir") {
		t.Fatalf("a diskless VM should not set -Path:\n%s", s)
	}
}

// A Gen 2 VM with a boot order rebuilds the UEFI BootOrder from the declared
// device-category priority (DVD before Drive), and only while the VM is off.
func TestEnsureVMScriptGen2BootOrder(t *testing.T) {
	vm := types.VM{
		Meta: types.ObjectMeta{Name: "Web01"},
		Spec: types.VMSpec{MemoryStartupBytes: 4294967296, BootOrder: []string{"DVD", "Drive", "Network"}},
	}
	s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 2)
	if !strings.Contains(s, "$want = @('DVD','Drive','Network')") { // wrapped array literal

		t.Fatalf("script does not set the desired boot order:\n%s", s)
	}
	if !strings.Contains(s, "Set-VMFirmware -VMName 'Web01' -BootOrder $ordered") {
		t.Fatalf("script does not apply the Gen 2 boot order:\n%s", s)
	}
	if !strings.Contains(s, "if ($running) { $pending = $true; $pendingWhat += ('boot order") {
		t.Fatalf("boot order should defer while the VM runs, recording why:\n%s", s)
	}
	// The deferral must name what differs. A bare "$pending = $true" left the
	// operator with four candidate causes and no way to spot a check drifting
	// falsely against a VM that already matches desired.
	if !strings.Contains(s, "pendingDetail = ($pendingWhat -join '; ')") {
		t.Fatalf("the pending reason must be reported back:\n%s", s)
	}
}

// A Gen 1 VM maps the categories onto the BIOS StartupOrder enum and always
// supplies the full device set. Floppy is only valid here, not on Gen 2.
func TestEnsureVMScriptGen1BootOrderMapsBios(t *testing.T) {
	vm := types.VM{
		Meta: types.ObjectMeta{Name: "Legacy01"},
		Spec: types.VMSpec{MemoryStartupBytes: 2147483648, BootOrder: []string{"DVD", "Drive"}},
	}
	s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 1)
	if !strings.Contains(s, "Set-VMBios -VMName 'Legacy01' -StartupOrder $ord") {
		t.Fatalf("Gen 1 boot order should use Set-VMBios StartupOrder:\n%s", s)
	}
	if strings.Contains(s, "Set-VMFirmware") {
		t.Fatalf("Gen 1 must not touch UEFI firmware:\n%s", s)
	}
	// Gen 2 drops Floppy; Gen 1 keeps it.
	if got := normaliseBootOrder([]string{"Floppy", "Drive"}, 2); len(got) != 1 || got[0] != "Drive" {
		t.Fatalf("Gen 2 should drop Floppy, got %v", got)
	}
	if got := normaliseBootOrder([]string{"Floppy", "Drive"}, 1); len(got) != 2 {
		t.Fatalf("Gen 1 should keep Floppy, got %v", got)
	}
}

// MigrateVM issues a shared-nothing Move-VM (backgrounded for progress polling)
// to the destination host, streams progress from PROGRESS lines, and returns the
// DONE summary.
func TestMigrateVMScript(t *testing.T) {
	f := &fakeRunner{streamLines: []string{"PROGRESS 0", "PROGRESS 45", "PROGRESS 100", "DONE migrated Web01 to hvnew02"}}
	var pct []string
	out, err := newTestPS(f).MigrateVM(context.Background(), "Web01", "hvnew02", `C:\VMs\Web01`, "", "", nil,
		func(note string) { pct = append(pct, note) })
	if err != nil {
		t.Fatal(err)
	}
	if out != "migrated Web01 to hvnew02" {
		t.Fatalf("out = %q", out)
	}
	// Progress notes came from the PROGRESS lines (the DONE line is not a note).
	want := []string{"live migration 0%", "live migration 45%", "live migration 100%"}
	if len(pct) != len(want) {
		t.Fatalf("progress notes = %v, want %v", pct, want)
	}
	for i := range want {
		if pct[i] != want[i] {
			t.Fatalf("progress notes = %v, want %v", pct, want)
		}
	}
	s := f.streamScript
	/* Compare first, then move against that report — Hyper-V's own mechanism for
	   a destination that objects. The shared-nothing arguments live on the
	   Compare-VM now; Move-VM takes the report it produced. */
	if !strings.Contains(s, "Compare-VM -Name $vm -DestinationHost $dest -IncludeStorage -DestinationStoragePath $path") {
		t.Fatalf("script missing the shared-nothing compatibility check: %s", s)
	}
	if !strings.Contains(s, "Move-VM -Name $vm -DestinationHost $dest -IncludeStorage -DestinationStoragePath $path") {
		t.Fatalf("script missing the shared-nothing Move-VM: %s", s)
	}
	if !strings.Contains(s, "Start-Job") || !strings.Contains(s, "Msvm_MigrationJob") {
		t.Fatalf("script missing background-job progress poll:\n%s", s)
	}
	// Service-initiated migration must switch off CredSSP, or it fails with
	// "no credentials available".
	if !strings.Contains(s, "-VirtualMachineMigrationAuthenticationType Kerberos") {
		t.Fatalf("script does not set Kerberos migration auth:\n%s", s)
	}
	if !strings.Contains(s, "$dest = 'hvnew02'") || !strings.Contains(s, `$path = 'C:\VMs\Web01'`) {
		t.Fatalf("script args wrong:\n%s", s)
	}
}

// A failed migration surfaces the streamed error.
func TestMigrateVMScriptFailure(t *testing.T) {
	f := &fakeRunner{streamLines: []string{"PROGRESS 10"}, streamErr: fmt.Errorf("powershell: exit status 1: (Live migration) transport failed")}
	_, err := newTestPS(f).MigrateVM(context.Background(), "Web01", "hvnew02", "", "", "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "transport failed") {
		t.Fatalf("expected transport failure, got %v", err)
	}
}

// A disk is only created when its directory is demonstrably reachable. Test-Path
// returns false both for a missing file and for a path whose volume cannot be
// read — a CSV offline, mid-rebuild, or owned by another node — and treating the
// second as "the disk is missing" would write a blank VHDX over the real one's
// path. Observed live: a degraded CSV produced New-VHD's bare "Failed to create
// the virtual hard disk" on VMs that already existed.
func TestEnsureVMScriptWillNotCreateADiskOnAnUnreadableVolume(t *testing.T) {
	vm := types.VM{
		Meta: types.ObjectMeta{Name: "Windows"},
		Spec: types.VMSpec{
			MemoryStartupBytes: 4294967296,
			Disks:              []types.VMDiskSpec{{Path: `C:\ClusterStorage\Datastore 1\Windows\Windows.vhdx`, SizeBytes: 137438953472}},
		},
	}
	s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 2)
	for _, want := range []string{
		// The directory is checked before anything is created.
		"$dir = Split-Path",
		"New-Item -ItemType Directory -Path $dir -Force -ErrorAction Stop",
		// And an unreachable one is an error naming the real cause.
		"the volume may be offline, still rebuilding, or owned by another node",
		"Not creating a new disk over a path that cannot be read",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("disk creation guard missing %q\n---\n%s", want, s)
		}
	}
	// The bare form — create whenever Test-Path is false — must not come back.
	if strings.Contains(s, ")) { New-VHD -Path ") {
		t.Error("New-VHD must not run on a Test-Path miss alone; an unreadable volume looks identical to a missing file")
	}
}

// A size typed onto an ALREADY-ATTACHED disk used to be accepted by the
// settings dialog and then discarded: SizeBytes was only ever read inside the
// "disk not present" branch, so raising the number and saving genuinely
// changed nothing on the host — no error, no drift, the VHDX just stayed its
// old size forever. The script must grow it, guarded to the disk that is the
// live leaf rather than an ancestor reached through a checkpoint chain, where
// Resize-VHD would fail outright.
func TestEnsureVMScriptGrowsAnExistingDisk(t *testing.T) {
	vm := types.VM{
		Meta: types.ObjectMeta{Name: "Web01"},
		Spec: types.VMSpec{
			MemoryStartupBytes: 4294967296,
			Disks:              []types.VMDiskSpec{{Path: `C:\ClusterStorage\vol1\Web01\Web01.vhdx`, SizeBytes: 137438953472, Dynamic: true}},
		},
	}
	s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 2)
	for _, want := range []string{
		"$presentLeaf",
		"elseif ($presentLeaf)",
		"$cur = (Get-VHD -Path 'C:\\ClusterStorage\\vol1\\Web01\\Web01.vhdx' -ErrorAction Stop).Size",
		"if ($cur -lt 137438953472) { Resize-VHD -Path 'C:\\ClusterStorage\\vol1\\Web01\\Web01.vhdx' -SizeBytes 137438953472 -ErrorAction Stop; $changed = $true }",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("script missing %q — an existing disk's declared size is not enforced:\n%s", want, s)
		}
	}
}

// The declared size must never drive a shrink: Resize-VHD can destroy data on
// the way down, and nothing here can tell an operator's typo from an intended
// one. A disk larger than declared is left alone, with only a warning.
func TestEnsureVMScriptNeverShrinksADisk(t *testing.T) {
	vm := types.VM{
		Meta: types.ObjectMeta{Name: "Web01"},
		Spec: types.VMSpec{
			MemoryStartupBytes: 4294967296,
			Disks:              []types.VMDiskSpec{{Path: `C:\ClusterStorage\vol1\Web01\Web01.vhdx`, SizeBytes: 68719476736}},
		},
	}
	s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 2)
	for _, want := range []string{
		"if ($cur -lt 68719476736) { Resize-VHD",
		"elseif ($cur -gt 68719476736) { Write-Warning",
		"Ballast never shrinks a disk automatically because Resize-VHD can destroy data",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("script missing %q — a smaller declared size must warn, not silently do nothing or shrink:\n%s", want, s)
		}
	}
}

// A disk with no SizeBytes declared (attached as-is, size unmanaged) must not
// gain a resize check at all — nothing was declared to enforce.
func TestEnsureVMScriptDoesNotResizeAnUnsizedDisk(t *testing.T) {
	vm := types.VM{
		Meta: types.ObjectMeta{Name: "Web01"},
		Spec: types.VMSpec{
			MemoryStartupBytes: 4294967296,
			Disks:              []types.VMDiskSpec{{Path: `C:\ClusterStorage\vol1\Web01\Web01.vhdx`}},
		},
	}
	s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 2)
	if strings.Contains(s, "Resize-VHD") {
		t.Errorf("a disk declared with no size must not be resized:\n%s", s)
	}
}

// Stopping a CLUSTERED VM must shut the guest down before the group goes
// offline. Stop-ClusterGroup obeys the Virtual Machine resource's OfflineAction,
// which Windows defaults to Save — so the same "stop" that cleanly shut down a
// standalone VM saved a clustered one's memory image instead.
//
// That is not just inconsistent. A saved image cannot be restored on a node with
// a different CPU feature set, which is exactly the failure this file already
// goes to lengths to diagnose (event 24000) and whose only remedy is
// destructive. Saving on every stop manufactures it across a mixed cluster.
//
// The script is searched with COMMENTS STRIPPED. The first version of this test
// matched the prose above the commands — both mention the cmdlets by name — and
// reported the ordering backwards while the code was correct.
func TestClusteredStopShutsTheGuestDownBeforeGoingOffline(t *testing.T) {
	p := &PowerShell{}
	nl := string(rune(10))
	commandsOnly := func(script string) string {
		var out []string
		for _, ln := range strings.Split(script, nl) {
			if strings.HasPrefix(strings.TrimSpace(ln), "#") {
				continue
			}
			out = append(out, ln)
		}
		return strings.Join(out, nl)
	}

	stop := commandsOnly(p.vmPowerScript("testVM", types.VMPowerOff))
	stopVM := strings.Index(stop, "Stop-VM")
	stopGroup := strings.Index(stop, "Stop-ClusterGroup")
	if stopVM < 0 || stopGroup < 0 {
		t.Fatalf("expected both Stop-VM and Stop-ClusterGroup in the stop script")
	}
	if stopVM > stopGroup {
		t.Error("Stop-ClusterGroup runs before Stop-VM, so taking the group offline still saves the VM's memory image")
	}
	// Bounded: a guest that will not shut down must not hold the job open.
	if !strings.Contains(stop, "AddSeconds(120)") {
		t.Error("the wait for the guest to stop is unbounded")
	}
	// The shutdown is gated on the requested state, so a start never runs it.
	start := commandsOnly(p.vmPowerScript("testVM", types.VMPowerRunning))
	if !strings.Contains(start, "Start-ClusterGroup") {
		t.Error("the start script does not use Start-ClusterGroup")
	}
	if !strings.Contains(start, "'Running' -eq 'Off'") {
		t.Error("the guest-shutdown block is not gated to the Off request, so a start could run it")
	}
}

/* Memory, split by what Hyper-V will actually accept on a RUNNING VM.

   Every memory change used to be gated wholly on $running, so raising a dynamic
   ceiling waited for a power-off. That is stricter than the platform: with
   Dynamic Memory already enabled, Minimum and Maximum change live — it is the
   whole point of dynamic memory — and only Startup, or turning dynamic memory
   on or off, needs the VM stopped.

   Deferring a live-capable change is not safe conservatism. It leaves the VM
   Progressing behind a "needs the VM off" message that is untrue, and asks an
   operator to schedule an outage to raise a ceiling Hyper-V would have moved
   while the guest ran. */

func dynVM(startup, min, max uint64) types.VM {
	return types.VM{
		Meta: types.ObjectMeta{Name: "Web01"},
		Spec: types.VMSpec{
			MemoryStartupBytes: startup,
			DynamicMemory:      &types.DynamicMemorySpec{MinBytes: min, MaxBytes: max},
		},
	}
}

func TestDynamicMemoryBandAppliesWithoutStoppingTheVM(t *testing.T) {
	s := newTestPS(&fakeRunner{}).ensureVMScript(dynVM(4294967296, 1073741824, 8589934592), 2)

	// The band has its own branch, reached when dynamic memory is already on and
	// Startup agrees — and that branch applies unconditionally, with no $running
	// test in front of it.
	if !strings.Contains(s, "Set-VMMemory -VMName 'Web01' -MinimumBytes 1073741824 -MaximumBytes 8589934592") {
		t.Fatalf("there is no live apply for the dynamic band:\n%s", s)
	}
	// The precise regression: Minimum and Maximum used to sit INSIDE the
	// needs-off test, which is exactly what made a band change wait for a
	// power-off. Their absence from it is the fix.
	needsOff := "(-not $m.DynamicMemoryEnabled) -or ($m.Startup -ne 4294967296)"
	i := strings.Index(s, needsOff)
	if i < 0 {
		t.Fatalf("the needs-off test is not the expected shape:\n%s", s)
	}
	line := s[i : i+strings.Index(s[i:], "\n")]
	if strings.Contains(line, "Minimum") || strings.Contains(line, "Maximum") {
		t.Errorf("the band is still part of the needs-off test, so it defers:\n%s", line)
	}

	band := strings.Index(s, "Set-VMMemory -VMName 'Web01' -MinimumBytes")
	seg := s[strings.LastIndex(s[:band], "} elseif ("):band]
	if strings.Contains(seg, "$running") || strings.Contains(seg, "$pending = $true") {
		t.Errorf("the band change is still gated on the VM being stopped:\n%s", seg)
	}
	// The branch must test the band it applies, not a condition that is always
	// false — otherwise the apply is dead code and this test would still pass.
	if !strings.Contains(seg, "($m.Minimum -ne 1073741824) -or ($m.Maximum -ne 8589934592)") {
		t.Errorf("the live branch does not test the band it applies:\n%s", seg)
	}
	if strings.Contains(seg, "$false") {
		t.Errorf("the live branch is unreachable:\n%s", seg)
	}
}

/*
Startup and enabling dynamic memory DO need the VM off — Hyper-V refuses

	both while it runs, so the deferral must survive for exactly those.
*/
func TestStartupAndEnablingDynamicMemoryStillNeedThePowerOff(t *testing.T) {
	s := newTestPS(&fakeRunner{}).ensureVMScript(dynVM(4294967296, 1073741824, 8589934592), 2)
	if !strings.Contains(s, "(-not $m.DynamicMemoryEnabled) -or ($m.Startup -ne 4294967296)") {
		t.Fatalf("the needs-off test does not cover enabling dynamic memory and moving Startup:\n%s", s)
	}
	if !strings.Contains(s, "if ($running) { $pending = $true; $pendingWhat += 'memory' }") {
		t.Error("the power-off deferral is gone entirely")
	}
}

/*
Static memory is fixed at boot, so every change still needs the VM off. The

	split must not have loosened that by accident.
*/
func TestStaticMemoryStillNeedsThePowerOff(t *testing.T) {
	vm := types.VM{
		Meta: types.ObjectMeta{Name: "Web01"},
		Spec: types.VMSpec{MemoryStartupBytes: 4294967296},
	}
	s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 2)
	if !strings.Contains(s, "Set-VMMemory -VMName 'Web01' -DynamicMemoryEnabled $false -StartupBytes 4294967296") {
		t.Fatalf("static memory is not applied at all:\n%s", s)
	}
	if !strings.Contains(s, "$m.DynamicMemoryEnabled -or ($m.Startup -ne 4294967296)") {
		t.Error("the static-memory difference test changed shape")
	}
	// And there is no live branch for a static VM — there is nothing Hyper-V
	// would accept while it runs.
	if strings.Contains(s, "-MinimumBytes") {
		t.Error("a static-memory VM was given a dynamic band to apply")
	}
}

/*
Processor count is unchanged by this: Hyper-V refuses it on a running VM

	whatever the memory model, and loosening it would leave the VM in a state
	Set-VMProcessor simply throws on.
*/
func TestProcessorCountStillNeedsThePowerOff(t *testing.T) {
	vm := types.VM{
		Meta: types.ObjectMeta{Name: "Web01"},
		Spec: types.VMSpec{ProcessorCount: 4, MemoryStartupBytes: 4294967296},
	}
	s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 2)
	if !strings.Contains(s, "$pendingWhat += 'processor count'") {
		t.Error("a processor change no longer defers to a power-off")
	}
}

/*
Console resolution: the one thing that actually changes what a console shows.

	A basic session renders exactly what the guest's video adapter is driving, so
	a bigger browser window scales 1024x768 up rather than showing more of
	anything. Set-VMVideo needs the VM stopped, so like Secure Boot it settles on
	the next power-off and says so meanwhile.
*/
func TestEnsureVMScriptDrivesTheConsoleResolution(t *testing.T) {
	vm := types.VM{
		Meta: types.ObjectMeta{Name: "Web01"},
		Spec: types.VMSpec{MemoryStartupBytes: 4294967296, VideoResolution: "1920x1080"},
	}
	s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 2)
	if !strings.Contains(s, "Set-VMVideo -VMName 'Web01' -ResolutionType Single -HorizontalResolution 1920 -VerticalResolution 1080") {
		t.Fatalf("script does not drive the video adapter:\n%s", s)
	}
	// Only while off, and named when deferred: applying it to a running VM is
	// refused by Hyper-V, and a change that silently does nothing is worse.
	if !strings.Contains(s, "if ($running) { $pending = $true") {
		t.Fatalf("a running VM is not deferred:\n%s", s)
	}
	if !strings.Contains(s, "console resolution (want 1920x1080") {
		t.Fatalf("the pending reason does not say what is waiting:\n%s", s)
	}
	// Absent on some builds. A console convenience must not fail a reconcile that
	// power, sizing and networking are waiting behind.
	if !strings.Contains(s, "if (Get-Command Set-VMVideo -ErrorAction SilentlyContinue)") {
		t.Fatalf("the cmdlet is not guarded:\n%s", s)
	}
}

// Unmanaged unless declared: an empty value leaves the adapter exactly as
// Hyper-V set it rather than driving it to something nobody asked for.
func TestEnsureVMScriptLeavesTheResolutionAloneWhenNotDeclared(t *testing.T) {
	vm := types.VM{Meta: types.ObjectMeta{Name: "Web01"}, Spec: types.VMSpec{MemoryStartupBytes: 1}}
	if s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 2); strings.Contains(s, "Set-VMVideo") {
		t.Fatalf("an undeclared resolution was driven anyway:\n%s", s)
	}
}

/*
A malformed resolution is refused, not guessed.

	Reaching Set-VMVideo as a zero would leave the console at 0x0 — a VM whose
	screen never comes back, for a typo in a field nobody would think to look at.
*/
func TestAMalformedResolutionIsRefused(t *testing.T) {
	for _, bad := range []string{"", "1920", "1920x", "x1080", "1920*1080", "abcxdef", "320x240", "99999x99999", "-1920x1080"} {
		if _, _, ok := parseResolution(bad); ok {
			t.Errorf("%q was accepted as a resolution", bad)
		}
	}
	for _, good := range []struct {
		in   string
		w, h int
	}{{"1920x1080", 1920, 1080}, {"1280X800", 1280, 800}, {" 3840 x 2160 ", 3840, 2160}} {
		w, h, ok := parseResolution(good.in)
		if !ok || w != good.w || h != good.h {
			t.Errorf("%q parsed as %dx%d (ok=%v)", good.in, w, h, ok)
		}
	}
}
