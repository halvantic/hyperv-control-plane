package hyperv

import (
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

/*
A disk that moved is not a disk that is missing.

	BallastJumphost, moved from I: to D: on HVNEW06. The VM was running with its
	disk attached from D: the entire time. The declared path still said I:, the
	presence check is path-exact, so the agent went off to attach a second copy
	from a folder that no longer held one — and handed the operator
	Add-VMHardDiskDrive's own words about a file it could not find, on a machine
	that was perfectly healthy.

	The window is unavoidable and short: the centre rewrites the declared paths
	when the move job reports success, and the agent enforces the old ones until
	it pulls that generation. What is not acceptable is what it did in the window.
*/
func vmWithDisks(disks ...types.VMDiskSpec) types.VM {
	vm := types.VM{Spec: types.VMSpec{Disks: disks}}
	vm.Meta.Name = "BallastJumphost"
	return vm
}

func TestAMovedDiskIsRecognisedRatherThanReattached(t *testing.T) {
	s := newTestPS(&fakeRunner{}).ensureVMScript(
		vmWithDisks(types.VMDiskSpec{Path: `I:\BallastJumphost\vm-2.vhdx`, Dynamic: true}), 2)

	for _, want := range []string{
		// The same file name attached from somewhere else is the evidence.
		`$leaf = [IO.Path]::GetFileName($want)`,
		`[IO.Path]::GetFileName($full) -ieq $leaf`,
		// Named in the message, both ends of it: where it was declared and where
		// it actually is. Neither alone tells anybody what happened.
		`is attached from ' + $movedTo + ' instead`,
		`the declared path is what needs updating`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in the generated script", want)
		}
	}

	// A warning, not a throw: the VM is healthy and the declaration is what is
	// behind. Failing the reconcile here is what marked a running VM degraded.
	moved := strings.Index(s, "if ($movedTo) {")
	if moved < 0 {
		t.Fatal("the script does not branch on a moved disk at all")
	}
	warn := strings.Index(s, "Write-Warning ('the disk declared at")
	if warn < moved {
		t.Fatal("the moved-disk branch does not warn")
	}
	if add := strings.Index(s, "Add-VMHardDiskDrive"); add < warn {
		t.Error("the disk is attached before the moved case is considered, which is the bug")
	}
}

/*
The silent half, which is worse than the error anybody saw.

	A declared disk WITH a size runs New-VHD when the file is not at the declared
	path. After a move that path is empty, so it would have created a blank VHDX
	there and attached it: no error, the VM boots, and it has an empty disk while
	its real one sits where it was moved to. The create must therefore be reached
	only when the disk is neither present nor found somewhere else.
*/
func TestABlankDiskIsNotCreatedOverAMovedOne(t *testing.T) {
	s := newTestPS(&fakeRunner{}).ensureVMScript(
		vmWithDisks(types.VMDiskSpec{Path: `I:\BallastJumphost\vm-2.vhdx`, Dynamic: true, SizeBytes: 42949672960}), 2)

	newVHD := strings.Index(s, "New-VHD -Path")
	if newVHD < 0 {
		t.Fatal("a sized disk no longer generates a create at all")
	}
	warn := strings.Index(s, "Write-Warning ('the disk declared at")
	if warn < 0 {
		t.Fatal("no moved-disk branch")
	}
	// The create sits in the else of the moved test, so it cannot run for a disk
	// that was found elsewhere.
	if newVHD < warn {
		t.Error("New-VHD runs before the moved-disk check, so a storage migration " +
			"would put a blank disk at the old path and attach it")
	}
}

/*
Two disks of the same name are not one disk that moved.

	A VM may legitimately declare D:\data\disk.vhdx and E:\data\disk.vhdx. Judging
	on the file name alone, the second would read as the first having moved, and
	neither would ever be attached. Every declared path is excluded from the
	comparison, which is what keeps the file-name test honest.
*/
func TestTwoDisksSharingANameAreNotMistakenForAMove(t *testing.T) {
	s := newTestPS(&fakeRunner{}).ensureVMScript(
		vmWithDisks(
			types.VMDiskSpec{Path: `D:\data\disk.vhdx`},
			types.VMDiskSpec{Path: `E:\data\disk.vhdx`},
		), 2)

	if !strings.Contains(s, `$declaredDisks -notcontains $full`) {
		t.Error("the moved test does not exclude the VM's own declared paths, so two disks " +
			"sharing a file name would each read as the other having moved")
	}
	// Both paths are in the exclusion list, normalised by Windows rather than by
	// string comparison in Go.
	for _, p := range []string{`'D:\data\disk.vhdx'`, `'E:\data\disk.vhdx'`} {
		if !strings.Contains(s, "$declaredDisks += [IO.Path]::GetFullPath($dp)") || !strings.Contains(s, p) {
			t.Errorf("declared path %s is not in the exclusion list", p)
		}
	}
}

// A VM with no disks declares nothing and must not emit a half-built list.
func TestNoDisksEmitsNoDeclaredList(t *testing.T) {
	s := newTestPS(&fakeRunner{}).ensureVMScript(vmWithDisks(), 2)
	if strings.Contains(s, "$declaredDisks") {
		t.Error("a VM with no declared disks still builds the exclusion list")
	}
}
