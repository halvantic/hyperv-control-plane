package vmware

import (
	"context"
	"errors"
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
	got, err := resolveDiskFile(t.Context(), "[datastore1] BSL/BSL.vmdk", testCapacity, ds.probe, datastoreFacts{})
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
	if _, err := resolveDiskFile(t.Context(), "[ds1] a/a.vmdk", testCapacity, probe, datastoreFacts{}); err != nil {
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
	got, err := resolveDiskFile(t.Context(), "[nfs1] BSL/BSL.vmdk", testCapacity, ds.probe, datastoreFacts{})
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
	got, err := resolveDiskFile(t.Context(), "[ds1] BSL/BSL-000001.vmdk", testCapacity, ds.probe, datastoreFacts{})
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
	_, err := resolveDiskFile(t.Context(), "[vsan1] BSL/BSL.vmdk", testCapacity, ds.probe, datastoreFacts{})
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
	got, err := resolveDiskFile(t.Context(), "[ds1] BSL/BSL.vmdk", testCapacity, ds.probe, datastoreFacts{})
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
	if err := acceptRangedRead(206, "206 Partial Content", nil, 103079211008); err != nil {
		t.Fatalf("the answer that means \"here is the range you asked for\" was rejected: %v", err)
	}
}

/* 200 to a MID-FILE range means the server ignored the range and is sending the
   file from the beginning. Reading it would write the start of the disk over the
   middle of it, on every chunk, and the copy would report success — the worst
   thing this code can produce, and invisible until the VM misbehaves. */
func TestARangeTheServerIgnoredIsRefusedRatherThanCopied(t *testing.T) {
	err := acceptRangedRead(200, "200 OK", nil, 65536)
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
	if err := acceptRangedRead(200, "200 OK", nil, 0); err != nil {
		t.Errorf("a read from offset 0 was refused: %v", err)
	}
}

// Everything else carries the status through, because "416" and "404" are the
// facts that tell a too-small file from a missing one.
func TestOtherStatusesAreReportedWithTheStatus(t *testing.T) {
	for _, tc := range []struct {
		code   int
		status string
	}{
		{416, "416 Range Not Satisfiable"},
		{404, "404 Not Found"},
		{403, "403 Forbidden"},
	} {
		err := acceptRangedRead(tc.code, tc.status, nil, 4096)
		if err == nil {
			t.Fatalf("%s was accepted", tc.status)
		}
		if !strings.Contains(err.Error(), tc.status) {
			t.Errorf("the failure drops the status: %v", err)
		}
	}
}

/* The 500 that stalled a warm migration said only "500 Internal Server Error".

   ESXi had answered with a page saying why, and it was dropped on the floor —
   leaving the resolver to infer a cause from a status code and the operator to
   read the inference. The reason the host gave has to reach the job message. */
func TestTheReasonTheHostGaveSurvives(t *testing.T) {
	body := strings.NewReader("<html><head><title>500</title></head><body>" +
		"<h1>Error</h1><p>Failed to open file /vmfs/volumes/datastore1/vm-2/vm-2-flat.vmdk: Device or resource busy</p>" +
		"</body></html>")
	err := acceptRangedRead(500, "500 Internal Server Error", body, 85899341824)
	if err == nil {
		t.Fatal("a 500 was accepted")
	}
	for _, want := range []string{"500 Internal Server Error", "Device or resource busy", "vm-2-flat.vmdk"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure drops %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "<") {
		t.Errorf("the markup came through with the sentence: %v", err)
	}
}

// A body with nothing in it leaves the message as it was, rather than trailing
// a colon and empty space.
func TestAnEmptyBodyAddsNothing(t *testing.T) {
	err := acceptRangedRead(404, "404 Not Found", strings.NewReader("\n\t "), 0)
	if got := err.Error(); got != "the datastore answered 404 Not Found" {
		t.Errorf("an empty body was appended anyway: %q", got)
	}
}

// Bounded: a host that is already misbehaving does not get to put a megabyte
// into a job message.
func TestAHugeBodyIsCutDown(t *testing.T) {
	err := acceptRangedRead(500, "500 Internal Server Error", strings.NewReader(strings.Repeat("x", 1<<20)), 0)
	if len(err.Error()) > 500 {
		t.Errorf("the message ran to %d characters", len(err.Error()))
	}
}

/* A failure that could not read anything has to say what the datastore knows,
   because the read alone cannot tell a missing file from a withheld one.

   The failure this replaced ended in two guesses: that a 500 meant another
   backup held a lock, and that the datastore might be vSAN. Both were one call
   away from being facts. */
func TestAFailureReportsWhatTheDatastoreKnows(t *testing.T) {
	probe := func(_ context.Context, cand string, _, _ int64) (int64, error) {
		if strings.HasSuffix(cand, "-flat.vmdk") {
			return 0, errors.New("the datastore answered 500 Internal Server Error: Device or resource busy")
		}
		return 0, errors.New("the datastore answered 404 Not Found")
	}
	facts := datastoreFacts{
		size: func(_ context.Context, cand string) (int64, bool) {
			switch {
			case strings.HasSuffix(cand, "-flat.vmdk"):
				return 85899345920, true
			case strings.HasSuffix(cand, "vm-2.vmdk"):
				return 523, true
			}
			return 0, false // the browser cannot see it; absent is not zero
		},
		kind: func(context.Context, string) string { return "VMFS" },
	}
	_, err := resolveDiskFile(t.Context(), "[datastore1] vm-2/vm-2.vmdk", 85899345920, probe, facts)
	if err == nil {
		t.Fatal("a disk nothing could serve was accepted")
	}
	got := err.Error()
	// The one fact that settles it: the flat file is exactly the size of the
	// disk and still would not serve a sector.
	if !strings.Contains(got, "lists it at 85899345920 bytes") {
		t.Errorf("the size the datastore reports for the flat file is missing:\n %v", err)
	}
	if !strings.Contains(got, "The datastore is VMFS.") {
		t.Errorf("the datastore's type is missing:\n %v", err)
	}
	if strings.Contains(got, "vSAN") {
		t.Errorf("a known VMFS datastore was still offered vSAN as a possibility:\n %v", err)
	}
	// A candidate the browser cannot see is reported without a size rather than
	// with a zero, which would read as an empty file.
	if strings.Contains(got, "lists it at 0 bytes") {
		t.Errorf("an unanswered size was printed as zero:\n %v", err)
	}
}

// On vSAN the answer is not "the file is missing" but "there is no file", and
// the message has to say the copy is impossible rather than sending the
// operator to look for a lock.
func TestVSANIsNamedAsTheReasonRatherThanGuessedAt(t *testing.T) {
	probe := func(context.Context, string, int64, int64) (int64, error) {
		return 0, errors.New("the datastore answered 404 Not Found")
	}
	facts := datastoreFacts{kind: func(context.Context, string) string { return "vsan" }}
	_, err := resolveDiskFile(t.Context(), "[vsanDatastore] vm-2/vm-2.vmdk", 85899345920, probe, facts)
	if err == nil {
		t.Fatal("a vSAN disk was accepted")
	}
	if !strings.Contains(err.Error(), "does not expose disks as files") {
		t.Errorf("vSAN was not named as the reason:\n %v", err)
	}
}

// Where nothing can be asked, the message keeps its original wording rather
// than asserting a datastore type it does not know.
func TestAnUnknownDatastoreKeepsThePossibility(t *testing.T) {
	probe := func(context.Context, string, int64, int64) (int64, error) {
		return 0, errors.New("the datastore answered 404 Not Found")
	}
	_, err := resolveDiskFile(t.Context(), "[ds1] a/a.vmdk", testCapacity, probe, datastoreFacts{})
	if err == nil {
		t.Fatal("a disk nothing could serve was accepted")
	}
	if !strings.Contains(err.Error(), "A disk on vSAN, or on a datastore this host cannot read as files") {
		t.Errorf("the unknown case lost its explanation:\n %v", err)
	}
}

/*
NFC's own code is what marks a lock, not the prose around it.

	The pass reads the power state and turns a lock into either "your guest holds
	it" or "something else does". It can only do that if the resolver marks the
	lock, and the marker has to be the code: the sentence ESXi wraps it in is
	ESXi's to reword.
*/
func TestALockIsMarkedSoTheCallerCanExplainIt(t *testing.T) {
	locked := func(_ context.Context, cand string, _, _ int64) (int64, error) {
		if strings.HasSuffix(cand, "-flat.vmdk") {
			return 0, errors.New("the datastore answered 500 Internal Server Error: Failed to open disk: NFC_FILE_LOCKED.")
		}
		return 0, errors.New("the datastore answered 404 Not Found")
	}
	_, err := resolveDiskFile(t.Context(), "[datastore1] vm-2/vm-2.vmdk", 85899345920, locked, datastoreFacts{})
	if !errors.Is(err, ErrDiskLocked) {
		t.Fatalf("a locked disk was not marked as one, so the pass cannot explain it:\n %v", err)
	}
	// Marking it must not reword the account of what was tried.
	if !strings.Contains(err.Error(), "nothing beside [datastore1] vm-2/vm-2.vmdk") {
		t.Errorf("marking the lock rewrote the failure:\n %v", err)
	}
}

// A 500 that is not a lock is not marked as one. ESXi answers 500 for more than
// locks, and a wrong mark sends the pass to blame the guest for a disk it is
// not holding.
func TestAPlain500IsNotMistakenForALock(t *testing.T) {
	probe := func(context.Context, string, int64, int64) (int64, error) {
		return 0, errors.New("the datastore answered 500 Internal Server Error: Failed to open disk: I/O error")
	}
	_, err := resolveDiskFile(t.Context(), "[ds1] a/a.vmdk", testCapacity, probe, datastoreFacts{})
	if errors.Is(err, ErrDiskLocked) {
		t.Errorf("an unrelated 500 was marked as a lock:\n %v", err)
	}
}
