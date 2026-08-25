package hyperv

import (
	"context"
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

/* Adopting a LUN that already has contents.

   The reconcile refuses one, correctly — formatting a disk with data on it is a
   decision an operator makes once, about one disk, not a field that re-applies
   every pass. But the refusal named "Wipe and adopt" and NO SUCH ACTION EXISTED:
   Wipe was a struct field hard-coded to false with no job able to set it. The
   console advertised a button for months that had never been built, which is
   worse than naming no remedy at all — it sends people looking.

   And wiping was never the only answer. A LUN holding "Cluster Disk 1" with
   7.6GB on it is a CSV moving between clusters, and the right act there is to
   adopt the existing volume untouched. Offering only the destructive option
   would have made the console's own advice destructive by default. */

func adopt(t *testing.T, a ISCSIAdoption, result string) (string, error, string) {
	t.Helper()
	var seen string
	var p PowerShell
	p.run = func(_ context.Context, script string) ([]byte, error) { seen = script; return []byte(result), nil }
	note, err := p.AdoptISCSIDiskWithContents(context.Background(), a)
	return note, err, seen
}

const adoptOK = `{"serial":"8a1d8c9e","changed":true,"note":""}`

func TestAdoptingAsIsDoesNotFormat(t *testing.T) {
	a := ISCSIAdoption{Name: "DS1", Source: types.CSVSourceSpec{SerialNumber: "8a1d8c9e"}, Keep: true}
	note, err, seen := adopt(t, a, adoptOK)
	if err != nil {
		t.Fatal(err)
	}
	// $keep only has to get past the refusal: New-Partition is already skipped
	// when a partition exists, and Format-Volume when a filesystem does.
	if !strings.Contains(seen, "$keep    = $true") {
		t.Fatalf("keep must reach the script:\n%s", seen)
	}
	if !strings.Contains(seen, "$wipe    = $false") {
		t.Error("adopting as is must not wipe")
	}
	// The report must not be ambiguous: the two modes have opposite consequences
	// for the data, so "adopted" alone is not an answer.
	if !strings.Contains(note, "without formatting it") {
		t.Errorf("it must say the contents were kept: %q", note)
	}
}

func TestWipeAndAdoptSaysWhatItDid(t *testing.T) {
	a := ISCSIAdoption{Name: "DS1", Source: types.CSVSourceSpec{SerialNumber: "8a1d8c9e"}, Wipe: true}
	note, err, seen := adopt(t, a, adoptOK)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "$wipe    = $true") || !strings.Contains(seen, "$keep    = $false") {
		t.Fatalf("wipe must reach the script and keep must not:\n%s", seen)
	}
	if !strings.Contains(note, "wiped DS1") {
		t.Errorf("it must say the disk was wiped: %q", note)
	}
}

/* Neither, or both, is refused rather than defaulted. Formatting a LUN because
   a parameter was missing is not a mistake that can be walked back. */
func TestAdoptRefusesAnAmbiguousMode(t *testing.T) {
	for _, a := range []ISCSIAdoption{
		{Name: "DS1"},
		{Name: "DS1", Keep: true, Wipe: true},
	} {
		_, err, seen := adopt(t, a, adoptOK)
		if err == nil {
			t.Fatalf("keep=%v wipe=%v must be refused", a.Keep, a.Wipe)
		}
		if !strings.Contains(err.Error(), "exactly one of keep or wipe") {
			t.Errorf("the refusal must say what to choose: %v", err)
		}
		if seen != "" {
			t.Error("nothing may run on the host when the request is ambiguous")
		}
	}
}

/* A filesystem that could not be READ is not adoptable as-is: keeping what is
   there is a promise nothing can verify. Wiping stays available, because that is
   an explicit decision to lose it. */
func TestTheRefusalOffersBothWaysOutButNotForAnUnreadableDisk(t *testing.T) {
	s := adoptScript
	if !strings.Contains(s, `"Adopt as is" brings the existing volume into the cluster untouched`) {
		t.Fatal("the refusal must name the non-destructive remedy too")
	}
	if !strings.Contains(s, "$hasData -and -not $wipe -and -not $keep -and -not $ours") {
		t.Error("keep must bypass the contents refusal")
	}
	// The unreadable branch keeps its own guard, without $keep.
	if !strings.Contains(s, "if ($hasData -and -not $wipe -and -not $ours -and $unreadable) {") {
		t.Error("an unreadable filesystem must still refuse, keep or not")
	}
}
