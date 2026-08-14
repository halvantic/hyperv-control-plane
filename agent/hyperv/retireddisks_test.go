package hyperv

import (
	"strings"
	"testing"
)

/* Two guards that were shown the right fact and did not use it.

   FormatDisk refused eight disks on bcluster2 with "could not be resolved to a
   disk on this host, so whether it is the OS or boot disk cannot be
   established". The refusal was correct — but the reason was not a read failure:
   a POOLED disk has no Disk object by design, because Storage Spaces owns it.
   The message sent an operator looking for broken hardware when the answer was
   "it is in a pool, take it out first", and the pool's name was one query away.

   The CSV capacity pre-check exists so New-Volume never gets to say "Not
   Supported" — and then compared free space against the LOGICAL size, which is
   the one calculation a mirror does not do. A 200GB three-way volume consumes
   600GB. Its own comment admitted the gap without acting on it. */

func TestFormatRefusalNamesThePoolThatOwnsTheDisk(t *testing.T) {
	s := formatDiskTemplate
	if !strings.Contains(s, "is a member of storage pool") {
		t.Fatal("a pooled disk must be named as pooled, not reported as unreadable")
	}
	if !strings.Contains(s, "Remove it from the pool first") {
		t.Error("a known cause with a known remedy must offer the step")
	}
	// And the genuinely-unreadable case must survive: that guard stands between
	// this and erasing a boot disk.
	if !strings.Contains(s, "Refusing to erase a disk that cannot be read") {
		t.Error("an unreadable disk must still be refused")
	}
}

func TestTheCSVPreCheckCountsCopiesNotJustSize(t *testing.T) {
	s := csvScriptForTest()
	if !strings.Contains(s, "$need = [int64]($want * $copies)") {
		t.Fatal("a mirror writes several copies; the pre-check must size against the footprint")
	}
	if !strings.Contains(s, "NumberOfDataCopiesDefault") {
		t.Error("the copy count must come from the pool's own resiliency setting, not a guess")
	}
	// The message must state both figures, or it cannot be checked.
	if !strings.Contains(s, "needs ' + [math]::Round($need/1GB,1)") {
		t.Error("the refusal must say what the volume actually needs")
	}
}

// The cause the capacity arithmetic cannot see: a retired disk is healthy and
// counted in the pool's size, so free space looks ample while there is nowhere
// to place a third copy.
func TestTheCSVPreCheckRefusesWhileDisksAreRetired(t *testing.T) {
	s := csvScriptForTest()
	if !strings.Contains(s, "are RETIRED") {
		t.Fatal("retired disks are why a healthy-looking pool refuses a mirror; the CSV step must say so")
	}
	if !strings.Contains(s, "Set-PhysicalDisk -Usage AutoSelect") {
		t.Error("it must name the command that returns them")
	}
}
