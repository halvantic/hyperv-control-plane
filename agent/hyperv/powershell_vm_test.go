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
	if !strings.Contains(s, "if ($running) { $pending = $true }") {
		t.Fatalf("boot order should defer while the VM runs:\n%s", s)
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
	out, err := newTestPS(f).MigrateVM(context.Background(), "Web01", "hvnew02", `C:\VMs\Web01`,
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
	if !strings.Contains(s, "Move-VM -Name $vm -DestinationHost $dest -IncludeStorage -DestinationStoragePath $path") {
		t.Fatalf("script missing shared-nothing Move-VM:\n%s", s)
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
	_, err := newTestPS(f).MigrateVM(context.Background(), "Web01", "hvnew02", "", nil)
	if err == nil || !strings.Contains(err.Error(), "transport failed") {
		t.Fatalf("expected transport failure, got %v", err)
	}
}
