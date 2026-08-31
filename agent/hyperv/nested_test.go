package hyperv

import (
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

/* Nested virtualisation: one tick, two settings.

   Exposing the host's virtualisation extensions lets a guest run Hyper-V. On
   its own that produces a guest which boots, runs inner VMs, and cannot reach
   anything with them: their MAC addresses are ones the outer switch has never
   seen, and it drops the traffic. So spoofing travels with it, and these tests
   exist mostly to stop the two drifting apart — the half-applied state looks
   like a networking fault rather than a missing setting, and nobody diagnoses
   it quickly. */

func nestedVM(on bool) types.VM {
	return types.VM{
		Meta: types.ObjectMeta{Name: "Web01"},
		Spec: types.VMSpec{
			MemoryStartupBytes:   4294967296,
			NestedVirtualisation: on,
			NetworkAdapters: []types.VMNetworkAdapterSpec{
				{Name: "net0", SwitchName: "ConvergedSwitch"},
				{Name: "net1", SwitchName: "ConvergedSwitch"},
			},
		},
	}
}

func TestNestedExposesTheExtensionsAndSpoofsEveryAdapter(t *testing.T) {
	s := newTestPS(&fakeRunner{}).ensureVMScript(nestedVM(true), 2)

	if !strings.Contains(s, "Set-VMProcessor -VMName 'Web01' -ExposeVirtualizationExtensions $true") {
		t.Fatalf("the extensions are not exposed:\n%s", s)
	}
	// EVERY adapter, not the first: an inner VM on the second NIC would lose its
	// traffic exactly as silently.
	for _, want := range []string{
		"Set-VMNetworkAdapter -VMName 'Web01' -Name 'net0' -MacAddressSpoofing 'On'",
		"Set-VMNetworkAdapter -VMName 'Web01' -Name 'net1' -MacAddressSpoofing 'On'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
}

/* The extensions need the VM stopped, so a running one is DEFERRED and says so.

   Ballast does not restart somebody's VM to satisfy a checkbox, and a setting
   that silently did nothing would be worse than either. */
func TestNestedIsDeferredOnARunningVMAndNamed(t *testing.T) {
	s := newTestPS(&fakeRunner{}).ensureVMScript(nestedVM(true), 2)
	if !strings.Contains(s, "$pendingWhat += 'nested virtualisation'") {
		t.Fatalf("a running VM is not deferred, or the pending reason is unnamed:\n%s", s)
	}
	// Spoofing is NOT deferred: it applies to a running VM, and holding it back
	// would leave the pair half-applied for as long as the VM stays up.
	if strings.Contains(s, "$pendingWhat += 'MAC") {
		t.Errorf("spoofing was deferred, though it applies live:\n%s", s)
	}
}

/* Unticking the box has to take both settings away again.

   Driving only the "on" direction is the classic half of this: the console
   reports the change settled, the VM keeps its extensions and its spoofing, and
   the next person to look cannot tell why. */
func TestClearingNestedTakesBothSettingsBack(t *testing.T) {
	s := newTestPS(&fakeRunner{}).ensureVMScript(nestedVM(false), 2)

	if !strings.Contains(s, "-ExposeVirtualizationExtensions $false") {
		t.Fatalf("clearing the box does not remove the extensions:\n%s", s)
	}
	if !strings.Contains(s, "-MacAddressSpoofing 'Off'") {
		t.Fatalf("clearing the box does not turn spoofing off:\n%s", s)
	}
	if strings.Contains(s, "-MacAddressSpoofing 'On'") {
		t.Errorf("spoofing was left on for a VM that is not nested:\n%s", s)
	}
}

/* A host too old to support it must not fail the whole reconcile.

   ExposeVirtualizationExtensions is absent before Hyper-V 2016, and power,
   sizing and networking are all queued behind this script. The same guard the
   video adapter gets, for the same reason. */
func TestAnUnsupportedHostWarnsRatherThanFailing(t *testing.T) {
	s := newTestPS(&fakeRunner{}).ensureVMScript(nestedVM(true), 2)
	if !strings.Contains(s, "if ($null -ne $vp.ExposeVirtualizationExtensions)") {
		t.Fatalf("the property is not guarded:\n%s", s)
	}
	if !strings.Contains(s, "does not support nested virtualisation") {
		t.Errorf("an unsupported host is not told why the setting did not apply:\n%s", s)
	}
}

// A VM with no adapters declared has its networking unmanaged, and nested must
// not invent one to spoof.
func TestNestedDoesNotInventAnAdapter(t *testing.T) {
	vm := types.VM{
		Meta: types.ObjectMeta{Name: "Web01"},
		Spec: types.VMSpec{MemoryStartupBytes: 1, NestedVirtualisation: true},
	}
	if s := newTestPS(&fakeRunner{}).ensureVMScript(vm, 2); strings.Contains(s, "MacAddressSpoofing") {
		t.Fatalf("spoofing was driven on a VM with no declared adapters:\n%s", s)
	}
}
