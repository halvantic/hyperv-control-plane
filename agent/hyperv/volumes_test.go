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
