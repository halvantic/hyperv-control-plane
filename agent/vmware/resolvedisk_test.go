package vmware

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

/* Finding the file that actually holds a disk's bytes.

   Two real failures on vcsa-02 came through here, one after the other, and the
   second was caused by the fix for the first.

     1. The copy read "[datastore1] BSL/BSL.vmdk" — VMware's own backing path —
        and got 523 bytes. That is the DESCRIPTOR: a few hundred bytes of text
        that names the real data file beside it, BSL-flat.vmdk.

     2. The fix asked the datastore browser for the file's size and took
        anything big enough. The browser answers about the VIRTUAL DISK: for
        BSL.vmdk it reports the full capacity, while an HTTP GET on that exact
        path still returns the 523-byte descriptor. Both answers are true about
        different things. Choosing on the strength of the one the copy does not
        use picked the descriptor every time, and the second run failed
        identically to the first.

   So the probe reads through the SAME channel the copy does, and reads the END
   of the disk — a descriptor will happily serve its own first bytes, so a probe
   at offset 0 cannot tell the two apart. */

const testCapacity int64 = 40 << 30

/* vmfsDatastore is a VMFS layout, including the trap.

   descriptorSize bytes are served for the .vmdk over HTTP, and the full disk is
   served for -flat.vmdk. statSize is what the datastore BROWSER would report for
   the .vmdk — the disk's capacity — and is deliberately present so a future
   implementation that goes back to asking it fails this test. */
type vmfsDatastore struct {
	files    map[string]int64 // path -> bytes actually servable over HTTP
	statSize map[string]int64 // path -> what the browser claims
	probes   []string
}

func (d *vmfsDatastore) probe(_ context.Context, dsPath string, offset, length int64) (int64, error) {
	d.probes = append(d.probes, dsPath)
	size, ok := d.files[dsPath]
	if !ok {
		return 0, fmt.Errorf("File not found: %s", dsPath)
	}
	if offset >= size {
		return 0, io.EOF
	}
	if n := size - offset; n < length {
		return n, nil
	}
	return length, nil
}

func vmfs() *vmfsDatastore {
	return &vmfsDatastore{
		files: map[string]int64{
			"[datastore1] BSL/BSL.vmdk":      523,
			"[datastore1] BSL/BSL-flat.vmdk": testCapacity,
		},
		statSize: map[string]int64{
			// The browser reports the logical disk here, not 523. This is the
			// number that made the previous implementation wrong.
			"[datastore1] BSL/BSL.vmdk":      testCapacity,
			"[datastore1] BSL/BSL-flat.vmdk": testCapacity,
		},
	}
}

// The bug, exactly as it presented: BSL.vmdk serves 523 bytes and the browser
// says it is 40GB.
func TestTheFlatFileIsChosenEvenWhenTheDescriptorClaimsToBeTheDisk(t *testing.T) {
	ds := vmfs()
	got, err := resolveDiskFile(t.Context(), "[datastore1] BSL/BSL.vmdk", testCapacity, ds.probe)
	if err != nil {
		t.Fatalf("nothing resolved: %v", err)
	}
	if got != "[datastore1] BSL/BSL-flat.vmdk" {
		t.Fatalf("resolved %q — the descriptor was chosen again, which is the bug that failed two real migrations", got)
	}
}

/* The probe must read the END of the disk.

   A descriptor is a real file and serves its own first bytes perfectly well, so
   a probe at offset 0 returns a full sector from BOTH files and cannot tell
   them apart. This asserts the offset rather than the outcome, because a probe
   at the wrong offset gives the right answer here by luck and the wrong one on
   any disk whose descriptor happens to exceed a sector. */
func TestTheProbeAsksForTheEndOfTheDiskNotTheStart(t *testing.T) {
	var gotOffset, gotLength int64
	probe := func(_ context.Context, _ string, offset, length int64) (int64, error) {
		gotOffset, gotLength = offset, length
		return length, nil
	}
	if _, err := resolveDiskFile(t.Context(), "[ds1] a/a.vmdk", testCapacity, probe); err != nil {
		t.Fatal(err)
	}
	if gotOffset != testCapacity-gotLength {
		t.Errorf("probed at offset %d of a %d-byte disk — a descriptor would pass this", gotOffset, testCapacity)
	}
	if gotLength != sectorSize {
		t.Errorf("probed %d bytes, want one sector (%d)", gotLength, sectorSize)
	}
}

/* A datastore that serves the data under the descriptor's own name — NFS, and
   anything monolithic — must keep using that name rather than hunting for a
   flat file that does not exist. */
func TestADatastoreThatServesTheDiskUnderItsOwnNameIsUsedAsIs(t *testing.T) {
	ds := &vmfsDatastore{files: map[string]int64{"[nfs1] BSL/BSL.vmdk": testCapacity}}
	got, err := resolveDiskFile(t.Context(), "[nfs1] BSL/BSL.vmdk", testCapacity, ds.probe)
	if err != nil {
		t.Fatalf("nothing resolved: %v", err)
	}
	if got != "[nfs1] BSL/BSL.vmdk" {
		t.Errorf("resolved %q, want the file itself", got)
	}
	if len(ds.probes) != 1 {
		t.Errorf("kept looking after finding the disk: %v", ds.probes)
	}
}

// A snapshot delta is the backing while a snapshot is open, and it is named
// differently again.
func TestASnapshotDeltaIsFound(t *testing.T) {
	ds := &vmfsDatastore{files: map[string]int64{
		"[ds1] BSL/BSL-000001.vmdk":       523,
		"[ds1] BSL/BSL-000001-delta.vmdk": testCapacity,
	}}
	got, err := resolveDiskFile(t.Context(), "[ds1] BSL/BSL-000001.vmdk", testCapacity, ds.probe)
	if err != nil {
		t.Fatalf("nothing resolved: %v", err)
	}
	if got != "[ds1] BSL/BSL-000001-delta.vmdk" {
		t.Errorf("resolved %q", got)
	}
}

/* Where nothing can serve the disk, the failure lists what was tried and what
   each one did. That is a diagnosis an operator can act on; "Error caused by
   file" and "returned less than requested" were both mysteries. */
func TestWhenNothingHoldsTheDiskTheFailureNamesEverythingItTried(t *testing.T) {
	ds := &vmfsDatastore{files: map[string]int64{"[vsan1] BSL/BSL.vmdk": 523}}
	_, err := resolveDiskFile(t.Context(), "[vsan1] BSL/BSL.vmdk", testCapacity, ds.probe)
	if err == nil {
		t.Fatal("a disk with no readable data file resolved to something")
	}
	for _, want := range []string{"BSL.vmdk", "BSL-flat.vmdk", "-sesparse.vmdk", "vSAN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q: %v", want, err)
		}
	}
	// What the named file actually did, so the reader can see it is too small
	// rather than missing.
	if !strings.Contains(err.Error(), "served 0 of") && !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("the failure does not say what happened to each candidate: %v", err)
	}
}

// A probe failure on one candidate must not stop the others being tried: a
// missing -flat.vmdk is the normal case on NFS, not a reason to give up.
func TestAMissingCandidateDoesNotStopTheSearch(t *testing.T) {
	ds := &vmfsDatastore{files: map[string]int64{
		"[ds1] BSL/BSL.vmdk":          523,
		"[ds1] BSL/BSL-sesparse.vmdk": testCapacity,
	}}
	got, err := resolveDiskFile(t.Context(), "[ds1] BSL/BSL.vmdk", testCapacity, ds.probe)
	if err != nil {
		t.Fatalf("the search stopped at the first missing candidate: %v", err)
	}
	if got != "[ds1] BSL/BSL-sesparse.vmdk" {
		t.Errorf("resolved %q", got)
	}
}

/* What the datastore's HTTP status means for a ranged read.

   The third real failure, and the one that showed this path had never copied a
   byte. The probe reported:

     BSL-flat.vmdk (could not be read: … 206 Partial Content)

   206 is the CORRECT answer to a range request. govmomi's Download accepts only
   200 OK and turns everything else into an error, so every ranged read of real
   disk data failed on its status. It stayed hidden because the earlier failures
   never got this far: a 523-byte descriptor fits inside the requested range, so
   the server answers 200 with the whole file. */
func TestARangedReadAcceptsPartialContent(t *testing.T) {
	if err := acceptRangedRead(206, "206 Partial Content", 103079211008); err != nil {
		t.Fatalf("the answer that means \"here is the range you asked for\" was rejected: %v", err)
	}
}

/* 200 to a MID-FILE range means the server ignored the range and is sending the
   file from the beginning. Reading it would write the start of the disk over the
   middle of it, on every chunk, and the copy would report success — the worst
   thing this code can produce, and invisible until the VM misbehaves. */
func TestARangeTheServerIgnoredIsRefusedRatherThanCopied(t *testing.T) {
	err := acceptRangedRead(200, "200 OK", 65536)
	if err == nil {
		t.Fatal("a whole-file body was accepted for a mid-file range, which silently corrupts the destination")
	}
	for _, want := range []string{"ignored the range", "over the middle"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not explain the risk (%q): %v", want, err)
		}
	}
}

// At offset 0 a whole-file body starts where it belongs, so it is fine — and a
// body shorter than asked for is caught by the read itself.
func TestAWholeFileBodyIsFineAtTheStartOfTheDisk(t *testing.T) {
	if err := acceptRangedRead(200, "200 OK", 0); err != nil {
		t.Errorf("a read from offset 0 was refused: %v", err)
	}
}

// Everything else carries the status through, because "416" and "404" are the
// facts that tell a too-small file from a missing one.
func TestOtherStatusesAreReportedWithTheStatus(t *testing.T) {
	for _, tc := range []struct{ code int; status string }{
		{416, "416 Range Not Satisfiable"},
		{404, "404 Not Found"},
		{403, "403 Forbidden"},
	} {
		err := acceptRangedRead(tc.code, tc.status, 4096)
		if err == nil {
			t.Fatalf("%s was accepted", tc.status)
		}
		if !strings.Contains(err.Error(), tc.status) {
			t.Errorf("the failure drops the status: %v", err)
		}
	}
}
