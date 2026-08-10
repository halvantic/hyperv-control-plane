package hyperv

import (
	"context"
	"strings"
	"testing"
)

/* PhysicalDisk DeviceId is unique per BUS, not per host. On HVNEW04 a 100GB local
   SSD and the 10GB iSCSI LUN both reported DeviceId 2, so the inventory's
   DeviceId-keyed maps attributed one disk's facts to the other: the LUN was shown
   holding drive F, which belongs to the SSD.

   The same assumption sat under FormatDisk, where it is not a display problem.
   Matching on DeviceId could return TWO disks, and then $disk.IsBoot on a
   two-element array is null — falsy — so the refusal to format the OS disk
   silently stopped applying, while Clear-Disk took whichever Number came first. */

func TestInventoryKeysDiskFactsOnTheUniqueId(t *testing.T) {
	s := inventoryScript()

	if !strings.Contains(s, "$diskToLetter[[string]$pd.UniqueId] = $letters[0]") {
		t.Error("the drive-letter map must be keyed on UniqueId; DeviceId collides across buses")
	}
	if !strings.Contains(s, "ForEach-Object { [string]$_.UniqueId }) } catch {}") {
		t.Error("the OS-disk set must be keyed on UniqueId, or the wrong disk is marked as the OS disk")
	}
	if !strings.Contains(s, "isOSDisk = ($uid -in $osIds)") {
		t.Error("OS-disk membership must be tested by UniqueId")
	}
	if !strings.Contains(s, "$letter = if ($diskToLetter.ContainsKey($uid))") {
		t.Error("the letter lookup must use UniqueId")
	}
	// Both are reported: DeviceId stays because it is short and familiar, UniqueId
	// because it is the one that identifies, and BusType is what makes two disks
	// sharing a DeviceId tellable apart in the console.
	for _, want := range []string{"uniqueId = $uid", "busType = [string]$_.BusType"} {
		if !strings.Contains(s, want) {
			t.Errorf("inventory should report %q", want)
		}
	}
}

// The guard. A destructive operation must not choose its target by an identifier
// that does not identify — refusing costs one message, guessing costs the wrong
// disk.
func TestFormatDiskRefusesAnAmbiguousId(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("RESULT=WIPED")}}
	if err := newTestPS(f).FormatDisk(context.Background(), "2"); err != nil {
		t.Fatal(err)
	}
	s := f.calls[0]

	if !strings.Contains(s, "if ($pd.Count -gt 1) {") {
		t.Fatal("an id matching more than one disk must be refused, not resolved by taking the first")
	}
	if !strings.Contains(s, "Refusing to erase one of them by guessing") {
		t.Error("the refusal must say why, and what to give instead")
	}
	// UniqueId is tried first, so an unambiguous identifier always wins.
	uid := strings.Index(s, "[string]$_.UniqueId -eq $want")
	dev := strings.Index(s, "[string]$_.DeviceId -eq $want")
	if uid == -1 || dev == -1 || uid > dev {
		t.Error("a unique id must be matched before falling back to the ambiguous one")
	}
	// The OS guard must run against a single disk, never an array whose IsBoot is
	// null and therefore falsy.
	if !strings.Contains(s, "$d = $disk[0]") || !strings.Contains(s, "if ($d.IsBoot -or $d.IsSystem)") {
		t.Error("the OS/boot refusal must be evaluated on one disk, not on a collection")
	}
	if strings.Contains(s, "if ($disk.IsBoot -or $disk.IsSystem)") {
		t.Error("the array-valued OS check is back; on two matches it is silently false")
	}
}

// Wiping must confirm on the disk it actually acted on, not re-query by the
// ambiguous id and read another disk's poolability.
func TestFormatDiskConfirmsOnTheDiskItTouched(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte("RESULT=WIPED")}}
	if err := newTestPS(f).FormatDisk(context.Background(), "2"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.calls[0], "Where-Object { [string]$_.UniqueId -eq [string]$pd.UniqueId }") {
		t.Fatal("the result must be read back on the same disk by its unique id")
	}
}
