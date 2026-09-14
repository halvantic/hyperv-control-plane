package hyperv

import (
	"strings"
	"testing"
)

/* What a CSV with no mount path reports, and in what order.

   From the rig, one line, 1004 characters:

     DS1: reported no mount path, and the resources this cluster does have are:
     "Cluster IP Address" [IP Address] (Online), … thirteen of them … . The CSV
     object reports state Offline and 1 volume entries.

   Everything needed to answer it was in there. The answer — the volume is
   Offline, so of course it has no mount path — was the last clause, behind a
   full inventory of resources that had nothing to do with it. The inventory was
   added for a real reason (see the script's comment: "no resource of that name"
   could not be told apart from a lookup that did not match), and it stays; what
   changes is that it is summarised and the finding leads. */

func TestTheVolumesOwnStateLeadsTheDiagnosis(t *testing.T) {
	// The finding first, so an operator who reads one clause reads the answer.
	i := strings.Index(csvMountScript, "the volume is ' + $csvState")
	j := strings.Index(csvMountScript, "No cluster resource is named")
	if i < 0 {
		t.Fatal("the CSV's own state should be reported")
	}
	if j >= 0 && i > j {
		t.Fatal("the volume's own state must be said before the resource inventory, not after it")
	}
	if !strings.Contains(csvMountScript, "this is the state to fix rather than the name") {
		t.Error("an offline volume should say the state is what to fix")
	}
}

// The inventory is summarised, not enumerated. Thirteen resources in a condition
// message is not a diagnosis, it is a haystack.
func TestTheResourceInventoryIsSummarised(t *testing.T) {
	if !strings.Contains(csvMountScript, "resources is a storage one") {
		t.Error("resources that cannot be a volume should be counted rather than listed")
	}
	if !strings.Contains(csvMountScript, "$others += 1") {
		t.Error("the summary needs a count of what it did not list")
	}
}

/* The filter EXCLUDES what cannot be a volume; it must never INCLUDE only what
   should be one.

   Filtering on 'Physical Disk' is what once reported "this cluster has no
   Physical Disk resources at all" while two CSVs were plainly present — the
   filter was the thing that was wrong, and a narrower question cannot reveal
   that. So an unrecognised type has to fall through and be NAMED. */
func TestAnUnrecognisedTypeIsNamedRatherThanCounted(t *testing.T) {
	if strings.Contains(csvMountScript, "$isVolume = @(") || strings.Contains(csvMountScript, "-eq 'Physical Disk'") {
		t.Fatal("the inventory must not filter to the types it expects; it excludes the ones it can rule out")
	}
	if !strings.Contains(csvMountScript, "$notVolume = @(") {
		t.Fatal("the exclusion list should be named for what it rules out")
	}
	// Whatever falls through is named in full, type included, so a storage
	// resource under an unexpected type is still visible.
	if !strings.Contains(csvMountScript, "$cand += ('\"' + $rn + '\" [' + $rt + '] ' + $rs)") {
		t.Error("a resource that is not ruled out should be named with its type and state")
	}
}

/* A stopped VM is not a fault.

   The "also not online" line exists because a failed resource may be why the
   volume cannot come up. A VM role is Offline whenever the VM is simply turned
   off, so listing it there puts an ordinary state in a line that reads as a
   fault list. Replaying the rig's own data named a stopped LinuxVM next to a
   genuinely failed File Share Witness, as though the two were alike. */
func TestAStoppedVMIsNotReportedAsAFault(t *testing.T) {
	if !strings.Contains(csvMountScript, "$rt -notlike 'Virtual Machine*'") {
		t.Fatal("VM roles must be excluded from the not-online list; being off is not a fault")
	}
}

// The phrase the Go side keys on must survive any rewording of the diagnosis.
// EnsureCSVMountPoints reports the observation only when a volume is not yet at
// its declared name, and it finds those lines by this substring.
func TestTheMarkerTheGoSideMatchesOnIsIntact(t *testing.T) {
	if !strings.Contains(csvMountScript, "reported no mount path") {
		t.Fatal("EnsureCSVMountPoints matches on 'reported no mount path'; rewording it silences the report")
	}
}
