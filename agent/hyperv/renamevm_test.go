package hyperv

import (
	"context"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

/*
Renaming a VM, and the two things that make it safe.

	The name is the identity: desired state is keyed on it, the reconciler matches
	VMs by it, and a clustered VM's cluster group is named after it. Get any of
	those out of step and the reconciler stops recognising the machine — which
	does not fail loudly, it CREATES a second one on the same disks.
*/
func TestRenameCarriesTheClusterGroupWithIt(t *testing.T) {
	s := renameVMScript("Web01", "Web02", "Primary1")

	for _, want := range []string{
		"Rename-VM -VM $vm -NewName $new",
		// The group is named after the VM, and the reconciler's do-not-create
		// guard looks it up BY VM NAME.
		"$grp = Get-ClusterGroup -Name $old",
		"$grp.Name = $new",
		// If the group will not follow, the VM's name goes back.
		"try { Rename-VM -VM $vm -NewName $old } catch {}",
		"The VM name has been put back",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}

	// The rollback must come AFTER the rename it undoes, or it is not a rollback.
	rename := strings.Index(s, "Rename-VM -VM $vm -NewName $new")
	back := strings.Index(s, "Rename-VM -VM $vm -NewName $old")
	if back < rename {
		t.Error("the rollback appears before the rename it exists to undo")
	}
}

// A standalone VM has no group, and the script must not go looking for one —
// Get-ClusterGroup on a host with no cluster is an error, not an empty answer.
func TestAStandaloneRenameDoesNotTouchTheCluster(t *testing.T) {
	s := renameVMScript("Web01", "Web02", "")
	for _, forbidden := range []string{"Get-ClusterGroup", "FailoverClusters"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("a standalone rename references %s", forbidden)
		}
	}
}

/*
Idempotent, because the centre re-keys only after the job succeeds.

	A job that ran and lost its result must be safe to repeat: the VM is already
	carrying the new name and nothing answers to the old one, which is this job's
	own previous run rather than a missing VM.
*/
func TestARenameThatAlreadyHappenedIsANoOp(t *testing.T) {
	s := renameVMScript("Web01", "Web02", "")
	if !strings.Contains(s, "if (-not $vm -and $already) { 'RESULT=NOOP") {
		t.Error("a repeat run is not recognised as already done")
	}
	// And a name already worn by a DIFFERENT machine is refused rather than
	// collided into.
	if !strings.Contains(s, "a different VM is already named") {
		t.Error("a collision with an existing VM is not refused by name")
	}
}

func TestRenamingToTheSameNameIsNotAnError(t *testing.T) {
	p := &PowerShell{}
	if err := p.RenameVM(context.Background(), "Web01", " Web01 ", ""); err != nil {
		t.Errorf("renaming to the name it already has returned %v", err)
	}
	if err := p.RenameVM(context.Background(), "Web01", "  ", ""); err == nil {
		t.Error("an empty new name was accepted")
	}
}

/*
THE GUARD. A renamed VM must never be recreated under its old name.

	Everything in the VM reconcile matches by NAME, so between the host rename and
	the centre re-keying desired state, the declared VM looks absent — and the next
	thing in that script is New-VM, pointed at the SAME VHDX files. The attach then
	fails with "being used by another process", which the script deliberately
	tolerates as transient, so the result is a phantom empty VM and no error.

	The Hyper-V GUID does not change when a VM is renamed, and the centre already
	carries it as VMStatus.VMID.
*/
func TestAVMFoundByGUIDIsNotRecreatedUnderItsOldName(t *testing.T) {
	vm := types.VM{Spec: types.VMSpec{Disks: []types.VMDiskSpec{{Path: `C:\VMs\Web01\os.vhdx`}}}}
	vm.Meta.Name = "Web01"
	vm.Status.VMID = "E0157A30-A923-406E-9162-CE0905EF642A"
	s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 2)

	for _, want := range []string{
		"[string]$_.Id -eq 'E0157A30-A923-406E-9162-CE0905EF642A'",
		"$ownedElsewhere = $true",
		"it has been renamed, and nothing was created",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in the generated script", want)
		}
	}

	// It has to run BEFORE New-VM, or it guards nothing.
	guard := strings.Index(s, "[string]$_.Id -eq")
	create := strings.Index(s, "New-VM -Name")
	if guard < 0 || create < 0 || guard > create {
		t.Fatal("the GUID check does not run before New-VM, so a renamed VM is still recreated")
	}
}

// With no VMID known there is nothing to match on, and the script must not
// emit a half-built check that matches everything or nothing.
func TestNoKnownGUIDEmitsNoGuard(t *testing.T) {
	vm := types.VM{Spec: types.VMSpec{Disks: []types.VMDiskSpec{{Path: `C:\VMs\Web01\os.vhdx`}}}}
	vm.Meta.Name = "Web01"
	s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 2)
	if strings.Contains(s, "[string]$_.Id -eq") {
		t.Error("a VM with no reported VMID still emits a GUID guard")
	}
}
