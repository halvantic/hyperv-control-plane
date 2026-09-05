package hyperv

import (
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

func vtpmVM(on bool, gen int) types.VM {
	v := types.VM{Meta: types.ObjectMeta{Name: "Win11"}}
	v.Spec.HyperVGeneration = gen
	v.Spec.TPM = on
	v.Spec.MemoryStartupBytes = 4 << 30
	v.Spec.ProcessorCount = 2
	return v
}

/*
Enable-VMTPM alone does not work, and that is the whole reason this belongs in

	a console rather than a runbook.

	A vTPM is sealed to key protectors, and Hyper-V refuses to enable one on a VM
	that has none: "A key protector cannot be found for the virtual machine."
*/
func TestAVTPMGetsItsKeyProtectorFirst(t *testing.T) {
	s := newTestPS(&fakeRunner{}).ensureVMScript(vtpmVM(true, 2), 2)

	if !strings.Contains(s, "Enable-VMTPM -VMName 'Win11'") {
		t.Fatalf("the TPM is never enabled:\n%s", s)
	}
	if !strings.Contains(s, "Set-VMKeyProtector -VMName 'Win11' -NewLocalKeyProtector") {
		t.Fatalf("no key protector is created, so Enable-VMTPM fails:\n%s", s)
	}
	// Order matters: the protector has to exist before the TPM is enabled.
	// The cmdlet CALLS, not the words: the comment above this block names
	// Enable-VMTPM to explain why the protector comes first, and it comes first
	// in the file too.
	if strings.Index(s, "Set-VMKeyProtector -VMName") > strings.Index(s, "Enable-VMTPM -VMName") {
		t.Errorf("the TPM is enabled before its key protector exists:\n%s", s)
	}
	// And only when there is not one already — creating a second would reseal a
	// VM whose BitLocker is bound to the first.
	if !strings.Contains(s, "if (-not $hasKp) {") {
		t.Errorf("a key protector is created unconditionally, resealing a VM that had one:\n%s", s)
	}
}

/*
The consequence of sealing is stated, because it is not reversible by wishing:

	the protector belongs to THIS host's guardian, so the VM cannot simply be
	exported and imported elsewhere. Better said when it is created than
	discovered during a migration.
*/
func TestTheSealingConsequenceIsSaidOutLoud(t *testing.T) {
	s := newTestPS(&fakeRunner{}).ensureVMScript(vtpmVM(true, 2), 2)
	for _, want := range []string{"cannot simply be exported and imported", "BitLocker recovery key"} {
		if !strings.Contains(s, want) {
			t.Errorf("the note does not mention %q:\n%s", want, s)
		}
	}
}

/*
Generation 1 has no UEFI firmware and can never present a TPM. Accepting the

	setting there would leave a checkbox ticked doing nothing, which is how
	somebody spends an afternoon on a Windows 11 installer that will not proceed
	and blames the installer.
*/
func TestGenerationOneIsRefusedRatherThanIgnored(t *testing.T) {
	s := newTestPS(&fakeRunner{}).ensureVMScript(vtpmVM(true, 1), 1)
	if !strings.Contains(s, "Generation 1") || !strings.Contains(s, "cannot have a TPM") {
		t.Fatalf("a Gen 1 VM accepts a TPM it can never have:\n%s", s)
	}
	// It must not try anyway.
	if strings.Contains(s, "Enable-VMTPM") {
		t.Errorf("it still tries to enable a TPM on Gen 1:\n%s", s)
	}
}

/*
Adding or removing a TPM needs the VM stopped, and Ballast does not restart

	somebody's VM to satisfy a checkbox — the same rule as Secure Boot and the
	virtualisation extensions.
*/
func TestATPMChangeOnARunningVMIsDeferredAndNamed(t *testing.T) {
	s := newTestPS(&fakeRunner{}).ensureVMScript(vtpmVM(true, 2), 2)
	if !strings.Contains(s, "$pendingWhat += ('a TPM (want ") {
		t.Fatalf("a running VM is not deferred, or the reason is unnamed:\n%s", s)
	}
}

// Driven in both directions, so clearing the box takes the TPM off again rather
// than leaving a setting that can only ever be turned on.
func TestTheTPMIsDrivenBothWays(t *testing.T) {
	off := newTestPS(&fakeRunner{}).ensureVMScript(vtpmVM(false, 2), 2)
	if !strings.Contains(off, "Disable-VMTPM -VMName 'Win11'") {
		t.Fatalf("unticking the box cannot remove the TPM:\n%s", off)
	}
	if !strings.Contains(off, "$tpmWant = $false") {
		t.Errorf("the wanted state is not carried:\n%s", off)
	}
}

/*
Compared against what the VM HAS, not against whether the command has been run

	before. Get-VMSecurity reports it, so there is no reason to track it.
*/
func TestTheTPMStateIsReadBackRatherThanAssumed(t *testing.T) {
	s := newTestPS(&fakeRunner{}).ensureVMScript(vtpmVM(true, 2), 2)
	if !strings.Contains(s, "Get-VMSecurity -VMName 'Win11'") || !strings.Contains(s, "$sec.TpmEnabled") {
		t.Fatalf("the current TPM state is never read:\n%s", s)
	}
	// Idempotent: nothing happens when it already matches.
	if !strings.Contains(s, "if ($tpmIs -ne $tpmWant) {") {
		t.Errorf("the TPM is set on every pass rather than only when it differs:\n%s", s)
	}
}
