package hyperv

import (
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
