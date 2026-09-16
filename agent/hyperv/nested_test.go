package hyperv

import (
	"context"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
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
	/* EVERY adapter, not the first: an inner VM on the second NIC would lose
	   its traffic exactly as silently.

	   Set through the adapter OBJECT now, and only when it has a switch. MAC
	   spoofing is a port feature and an unconnected adapter has no port, so
	   Hyper-V refuses -- which held a whole VM Degraded after a copy landed it
	   with its networks disconnected. */
	for _, want := range []string{
		"@(Get-VMNetworkAdapter -VMName 'Web01' -Name 'net0' -ErrorAction SilentlyContinue)[0]",
		"@(Get-VMNetworkAdapter -VMName 'Web01' -Name 'net1' -ErrorAction SilentlyContinue)[0]",
		"Set-VMNetworkAdapter -VMNetworkAdapter $ad -MacAddressSpoofing 'On'",
		"if ($ad -and $ad.SwitchName) {",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	// And it says so rather than failing: a setting that cannot apply yet is a
	// fact about the adapter, not a fault in the VM.
	if !strings.Contains(s, "is not connected to a switch, so MAC address spoofing cannot be set yet") {
		t.Errorf("a disconnected adapter fails the reconcile instead of being explained:\n%s", s)
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

/*
Shared-nothing migration rests on SMB between the two hosts, and the failure

	when it is missing is expensive.

	  Failed to create folder '\HVNEW06\HVNEW04.905057643$\I\Virtual Hard Disks':
	  'The network path was not found.' (0x80070035)

	Move-VM gets most of the way in before that: the guest has been stunned and,
	with the role removed first, the VM was left un-clustered. Tested up front,
	before anything is touched.
*/
func TestTheMoveChecksSMBBeforeTouchingAnything(t *testing.T) {
	f := &fakeRunner{streamLines: []string{"DONE migrated Web01 to hvnew02"}}
	newTestPS(f).MigrateVM(context.Background(), "Web01", "hvnew02", `I:\`, "Secondary", "", nil, nil)
	s := f.streamScript

	if !strings.Contains(s, `$destShare = '\\' + $dest`) {
		t.Fatalf("nothing tests SMB to the destination:\n%s", s)
	}
	// Before the role comes off, or the check is worth nothing — the VM would
	// already be un-clustered by the time it fired.
	pre, uncluster := strings.Index(s, "$destShare"), strings.Index(s, "Remove-ClusterGroup")
	if uncluster >= 0 && pre > uncluster {
		t.Errorf("the SMB check runs after the VM is un-clustered:\n%s", s)
	}
	/* And Ballast opens the firewall ITSELF rather than naming it as homework.

	   Both ends are hosts it runs an agent on, and the job already reaches the
	   destination for the migration settings. "Go and run this on the other
	   host" is the defect, not the remedy. */
	if !strings.Contains(s, "Enable-NetFirewallRule -CimSession $fwSession -DisplayGroup 'File and Printer Sharing'") {
		t.Errorf("the agent does not open the firewall on the destination: %s", s)
	}
	// Only when something is actually off, and said out loud when it acts: a
	// firewall Ballast opened outlives this job.
	if !strings.Contains(s, "if ($fwOff.Count -gt 0)") {
		t.Errorf("it enables rules that are already enabled: %s", s)
	}
	if !strings.Contains(s, "PROGRESS enabled File and Printer Sharing on") {
		t.Errorf("a firewall change is made silently: %s", s)
	}
	// Once the firewall is dealt with, the refusal points at what is left.
	if !strings.Contains(s, "resolve and reach") {
		t.Errorf("the refusal does not say what to check once the firewall is on: %s", s)
	}
	if !strings.Contains(s, "Nothing has been changed here") {
		t.Errorf("the refusal does not say the VM is untouched:\n%s", s)
	}
}

/*
A failed move must put the cluster role back.

	The role comes off before the copy, so a move that fails leaves the VM where
	it was and NOT highly available — running, reachable, and quietly no longer
	protected. Nobody notices until a node goes down.
*/
func TestAFailedMovePutsTheClusterRoleBack(t *testing.T) {
	f := &fakeRunner{streamLines: []string{"DONE migrated Web01 to hvnew02"}}
	newTestPS(f).MigrateVM(context.Background(), "Web01", "hvnew02", `I:\`, "Secondary", "", nil, nil)
	s := f.streamScript

	if !strings.Contains(s, "} catch {") || !strings.Contains(s, "putting ") {
		t.Fatalf("a failed move does not restore the role:\n%s", s)
	}
	if !strings.Contains(s, "Add-ClusterVirtualMachineRole -Cluster 'Secondary' -VirtualMachine 'Web01'") {
		t.Errorf("the restore does not re-add the role to the source cluster:\n%s", s)
	}
	// And the original failure still surfaces: a restore problem is reported
	// beside it, never instead of it.
	if !strings.Contains(s, "  throw\n}") {
		t.Errorf("the original error is swallowed by the restore:\n%s", s)
	}
	if !strings.Contains(s, "no longer highly available") {
		t.Errorf("a failed restore does not say what state the VM is in:\n%s", s)
	}
}

// A standalone source has no role to remove, so nothing to put back either.
func TestAStandaloneMoveHasNoRoleToRestore(t *testing.T) {
	f := &fakeRunner{streamLines: []string{"DONE migrated Web01 to hvnew02"}}
	newTestPS(f).MigrateVM(context.Background(), "Web01", "hvnew02", `I:\`, "", "", nil, nil)
	if s := f.streamScript; strings.Contains(s, "Add-ClusterVirtualMachineRole") {
		t.Fatalf("a standalone VM was sent cluster cmdlets:\n%s", s)
	}
}

/* Powering a VM on a host that has no Failover Clustering.

     Get-ClusterGroup : The term 'Get-ClusterGroup' is not recognized as the
     name of a cmdlet...

   -ErrorAction SilentlyContinue does not suppress a CommandNotFoundException:
   the failure happens at command resolution, before a parameter is bound, and
   under $ErrorActionPreference='Stop' it terminates. A standalone host has no
   such cmdlet at all.

   Found the moment a VM was evacuated onto HVNEW06, which is not a coincidence:
   the destination of an evacuation is the host least likely to be a cluster
   member. */
func TestPoweringAVMOnAStandaloneHostDoesNotNeedClusterCmdlets(t *testing.T) {
	s := newTestPS(&fakeRunner{}).vmPowerScript("HVNew03", types.VMPowerRunning)

	if !strings.Contains(s, "if (Get-Command Get-ClusterGroup -ErrorAction SilentlyContinue)") {
		t.Fatalf("the power script calls a cluster cmdlet that may not exist: %s", s)
	}
	// And it still uses the cluster path where clustering IS present, because a
	// clustered VM is powered through its group from any member.
	if !strings.Contains(s, "Get-ClusterGroup -Name 'HVNew03'") {
		t.Errorf("the cluster path was removed rather than guarded: %s", s)
	}
}

func TestRestartingAVMOnAStandaloneHostDoesNotNeedClusterCmdlets(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte(``)}}
	newTestPS(f).RestartVM(context.Background(), "HVNew03")
	s := strings.Join(f.calls, "\n")

	if !strings.Contains(s, "if (Get-Command Get-ClusterGroup -ErrorAction SilentlyContinue)") {
		t.Fatalf("restart calls a cluster cmdlet that may not exist: %s", s)
	}
}
