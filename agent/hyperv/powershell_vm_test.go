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
