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

/*
The extensions need the VM stopped, so a running one is DEFERRED and says so.

	Ballast does not restart somebody's VM to satisfy a checkbox, and a setting
	that silently did nothing would be worse than either.
*/
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

/*
Unticking the box has to take both settings away again.

	Driving only the "on" direction is the classic half of this: the console
	reports the change settled, the VM keeps its extensions and its spoofing, and
	the next person to look cannot tell why.
*/
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

/*
A host too old to support it must not fail the whole reconcile.

	ExposeVirtualizationExtensions is absent before Hyper-V 2016, and power,
	sizing and networking are all queued behind this script. The same guard the
	video adapter gets, for the same reason.
*/
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

/*
Un-clustering a VM before a cross-boundary move.

	The cluster refuses a group that still holds resources — "Group 'HVNew01' is
	not empty. Please use -RemoveResources" — which is every VM group there has
	ever been. The resources are the VM's CLUSTERING, not the VM: removing them
	leaves it registered and running on its node, which is what Failover Cluster
	Manager's "Remove role" does and what a shared-nothing move needs.
*/
func TestUnclusteringRemovesTheRoleAndChecksTheVMSurvived(t *testing.T) {
	s := unclusterScript("HVNew01", "Secondary")

	if !strings.Contains(s, "-RemoveResources -Force") {
		t.Fatalf("the group is removed without its resources, which the cluster refuses:\n%s", s)
	}
	if strings.Contains(s, "-RemoveResources:$false") {
		t.Errorf("the refused form is still there:\n%s", s)
	}
	/* The guard that matters. If removing the role ever took the VM with it,
	   the next line would move nothing and report success on a VM that no
	   longer exists. */
	if !strings.Contains(s, "if (-not (Get-VM -Name 'HVNew01'") {
		t.Errorf("nothing checks the VM survived the role removal:\n%s", s)
	}
	if !strings.Contains(s, "still on the datastore") {
		t.Errorf("the refusal does not say the files are recoverable:\n%s", s)
	}
}

// A standalone source has no cluster to leave, and must not be sent cluster
// cmdlets that would fail on a host with no cluster service.
func TestAStandaloneSourceIsNotUnclustered(t *testing.T) {
	if s := unclusterScript("Web01", ""); s != "" {
		t.Fatalf("a standalone VM was sent cluster cmdlets:\n%s", s)
	}
	if s := reclusterScript("Web01", ""); s != "" {
		t.Fatalf("a standalone destination was sent cluster cmdlets:\n%s", s)
	}
}

/*
Failing to make the VM highly available at the destination must NOT fail the

	move. The copy is done and the VM has arrived; re-running it would move a VM
	that is already there.
*/
func TestAFailedReclusterWarnsRatherThanFailingTheMove(t *testing.T) {
	s := reclusterScript("HVNew01", "Primary")
	if !strings.Contains(s, "Write-Warning") {
		t.Fatalf("a failed role addition would fail the whole move:\n%s", s)
	}
	if !strings.Contains(s, "add the role in Failover Cluster Manager or re-run this move") {
		t.Errorf("the warning does not say what to do about it:\n%s", s)
	}
}

/*
Which volumes a host offers as VM storage.

	Measured on HVNEW06, where Ballast reported "no volumes on this host" while
	the OS had a 3.8TB NTFS volume at I: that the operator had formatted for
	Hyper-V. Windows had put the 16MB system partition on disk 1 and the boot
	volume on disk 0, so disk 1 was IsSystem — and the exclusion, which asked the
	DISK, took every lettered partition on it.
*/
func TestTheOSExclusionAsksThePartitionNotTheDisk(t *testing.T) {
	s := resourcesScript

	// Partition-level flags. A disk carrying a system partition is not itself
	// off limits: its other partitions are ordinary storage.
	if !strings.Contains(s, "Get-Partition -ErrorAction SilentlyContinue |") ||
		!strings.Contains(s, "($_.IsBoot -or $_.IsSystem) -and $_.DriveLetter") {
		t.Fatalf("the exclusion does not ask the partition:\n%s", s)
	}
	if strings.Contains(s, "Get-Disk -ErrorAction SilentlyContinue | Where-Object { $_.IsBoot -or $_.IsSystem }") {
		t.Errorf("the disk-level exclusion is still there, which hides every volume sharing a disk with the ESP:\n%s", s)
	}
	// Windows is not always on C.
	if !strings.Contains(s, "$env:SystemDrive") {
		t.Errorf("a Windows installed somewhere other than C would not be excluded:\n%s", s)
	}
	// And C stays as the fallback for a host where detection fails entirely.
	if !strings.Contains(s, `$osDriveLetters = @('C')`) {
		t.Errorf("the C fallback was dropped:\n%s", s)
	}
}

/*
A cluster member reports its CSVs AND its own volumes.

	This was an either/or, so a member's local drives were invisible everywhere
	Ballast offers storage — no placement, no evacuation destination, nothing in
	the tree. Which kind a volume is decides what is safe to put on it, and that
	is expressed by tagging them, not by leaving half of them out.
*/
func TestAMemberReportsBothKindsOfVolume(t *testing.T) {
	s := resourcesScript

	if !strings.Contains(s, "$vols += @(Get-Volume") {
		t.Fatalf("local volumes are not added alongside the CSVs:\n%s", s)
	}
	if strings.Contains(s, "} else {\n  # Exclude the OS/system volume") {
		t.Errorf("the either/or is still there:\n%s", s)
	}
	for _, want := range []string{"shared = $true", "shared = $false"} {
		if !strings.Contains(s, want) {
			t.Errorf("volumes are not tagged with %q, so nothing can tell a CSV from a local disk:\n%s", want, s)
		}
	}
}
