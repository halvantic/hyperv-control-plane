package hyperv

import (
	"strings"
	"testing"
)

/* A volume with no drive letter is still a volume.

   The inventory only collected volumes that HAD a letter, which was true of
   every volume Ballast could make until it learned to format without one. The
   console now offers that deliberately — for a disk to be mounted into a folder
   or handed to a cluster — and the volume it made was invisible the moment it
   existed, showing only as "1 disk with no drive letter". Offering a way to
   create something the inventory then drops is worse than not offering it. */
func TestLetterlessVolumesAreCollected(t *testing.T) {
	s := resourcesScript

	if !strings.Contains(s, "-not $_.DriveLetter") {
		t.Fatalf("volumes without a drive letter are still filtered out:\n%s", s)
	}
	// Identified by the GUID path, which is what it has in place of a letter and
	// what mounts it.
	if !strings.Contains(s, "path = [string]$_.Path") {
		t.Errorf("a letter-less volume carries no path, so nothing can address it:\n%s", s)
	}
	if !strings.Contains(s, "unlettered = $true") {
		t.Errorf("the console cannot tell it apart from a lettered volume:\n%s", s)
	}
}

/* Windows makes its own letter-less fixed volumes — recovery, system reserved,
   EFI. Listing those would bury the one volume this exists to show under three
   nobody asked about, which is its own kind of hiding. */
func TestWindowsOwnPartitionsAreNotListedAsVolumes(t *testing.T) {
	s := resourcesScript
	for _, want := range []string{"Recovery", "System Reserved", "EFI system partition"} {
		if !strings.Contains(s, want) {
			t.Errorf("%s is not excluded, so it appears as host storage:\n%s", want, s)
		}
	}
	// And a size floor, because a small unnamed system partition slips past a
	// name match whenever Windows chooses a different label.
	if !strings.Contains(s, "-gt 1073741824") {
		t.Errorf("there is no size floor, so a small system partition is listed as storage:\n%s", s)
	}
	// A raw, unformatted disk is not a volume and must not be reported as one.
	if !strings.Contains(s, "$_.FileSystemType -ne 'Unknown'") {
		t.Errorf("an unformatted volume is reported as storage:\n%s", s)
	}
}

// The label leads when there is one: an operator named it for a reason, and a
// bare GUID is not a name anybody recognises.
func TestALetterlessVolumeIsNamedByItsLabel(t *testing.T) {
	s := resourcesScript
	if !strings.Contains(s, "if ($label) { $label } else { 'unlettered volume' }") {
		t.Fatalf("the label is not preferred over a fallback name:\n%s", s)
	}
	// PowerShell 5.1: an if/else expression is fine, a ternary is not — this
	// package has shipped a ternary that failed to parse on every host once.
	if strings.Contains(s, "$label ? ") {
		t.Errorf("a PowerShell 7 ternary will not parse on 5.1:\n%s", s)
	}
}

/* A CSV must not arrive twice.

   The letter-less pass was added after the lettered one and inherited none of
   its protection. The old comment there said a CSV "cannot be reported twice"
   because its mount lives under C: and C is excluded — true of a query that
   requires a drive letter, and a CSV has none of its own. So every CSV came back
   a second time under its raw volume name: DS1 at C:\ClusterStorage\DS1, and
   "Cluster Disk 1" at the same GUID path with identical size and usage.
   Observed on Primary1 and Secondary, 2026-09-02, as a nameless extra row under
   each cluster's Storage. */
func TestAVolumeAlreadyReportedIsNotCollectedAgain(t *testing.T) {
	s := resourcesScript

	if !strings.Contains(s, "$claimedPaths = @{}") {
		t.Fatalf("nothing tracks which volumes have already been reported:\n%s", s)
	}
	// A CSV claims its partition's GUID path as it is collected.
	if !strings.Contains(s, "$claimedPaths[([string]$p.Name)") {
		t.Errorf("a CSV does not claim its own volume path, so it is collected twice:\n%s", s)
	}
	// So does a lettered volume, or the same disk arrives as both I: and a GUID.
	if !strings.Contains(s, "if ($_.Path) { $claimedPaths[([string]$_.Path)") {
		t.Errorf("a lettered volume does not claim its path:\n%s", s)
	}
	// And the letter-less pass skips anything already claimed.
	if !strings.Contains(s, "(-not $claimedPaths.ContainsKey(") {
		t.Fatalf("the letter-less pass reports volumes already collected above it:\n%s", s)
	}
}

/* Matched case-insensitively and without a trailing separator, because the two
   sources spell the same volume differently: a CSV partition reports
   \\?\Volume{guid} and Get-Volume reports \\?\Volume{guid}\. Comparing them
   literally would claim nothing and the duplicate would survive the fix. */
func TestTheClaimIsNormalisedBeforeComparing(t *testing.T) {
	s := resourcesScript
	if strings.Count(s, ".TrimEnd('\\').ToLower()") < 3 {
		t.Errorf("paths are compared without normalising, so the claim never matches:\n%s", s)
	}
}

/* A lettered volume reports which physical disk it lives on, and whether that
   disk also carries the boot/system partition — two different questions.

   Found live on HVNEW06: I: is a 3.8TB data volume carved from spare space on
   the same physical disk that carries the tiny hidden system partition. It is
   not the OS volume (excluded from $osDriveLetters already, same as always),
   but a console asking "which disk backs I:, and is wiping that disk safe"
   had nothing but PhysicalDisk.DriveLetter to go on — which only ever holds
   ONE letter per disk, so it could resolve at most one of a disk's several
   lettered partitions. $letterToDiskUniqueId is built from every letter on
   every disk, not just the first, precisely so this stops being a guess. */
func TestLetteredVolumesReportTheirDisk(t *testing.T) {
	s := resourcesScript
	// Self-contained within THIS script, not reused from the separate
	// physical-disk inventory pass — that runs as its own PowerShell
	// invocation and shares no variables with this one. A cross-script
	// variable reference here would throw "cannot call a method on a
	// null-valued expression" and take down the whole resources collection.
	if !strings.Contains(s, "if ($p.DriveLetter) { $letterToDiskUniqueId[[string]$p.DriveLetter] = [string]$pd.UniqueId }") {
		t.Fatalf("every letter on a disk must resolve to it, not just the first:\n%s", s)
	}
	if !strings.Contains(s, "diskUniqueId = $diskId") {
		t.Errorf("the volume does not report which disk backs it:\n%s", s)
	}
	// Disk-level $osDiskIds, not the volume's own letter — SharesDiskWithOS
	// answers "would a whole-disk wipe endanger the boot chain", a fact about
	// the DISK, unrelated to whether this volume is excluded from
	// $osDriveLetters (it always is, by construction, before this line runs).
	if !strings.Contains(s, "$sharesOS = $diskId -and $osDiskIds.ContainsKey($diskId)") {
		t.Errorf("sharesDiskWithOS must be checked against the disk-level $osDiskIds, not the volume's own letter:\n%s", s)
	}
	if !strings.Contains(s, "sharesDiskWithOS = [bool]$sharesOS") {
		t.Errorf("the volume does not report whether its disk shares the OS:\n%s", s)
	}
}
