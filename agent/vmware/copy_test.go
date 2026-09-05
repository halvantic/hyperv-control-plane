package vmware

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

/* The copy loop, tested without a vCenter.

   This is the arithmetic that corrupts a disk quietly when it is wrong: a
   range read from one offset and written to another leaves a VM that boots and
   is subtly not itself. None of it needs a lab, so none of it is left to one. */

// fakeSource serves a synthetic disk image. Reads come from the image at the
// offset asked for, so a loop that writes to the wrong place shows up as
// mismatched content rather than as a byte count that happens to add up.
type fakeSource struct {
	image   []byte
	extents []Extent
	next    string
	reads   []Extent
	// readPaths is which FILE each read came from, which is the difference
	// between reading a disk and reading its descriptor.
	readPaths []string
	err       error
}

func (f *fakeSource) ChangedAreas(_ context.Context, _, _ string, _ int32, _ string, _ int64) ([]Extent, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	return f.extents, f.next, nil
}

func (f *fakeSource) ReadAt(_ context.Context, path string, offset, length int64) (io.ReadCloser, error) {
	f.reads = append(f.reads, Extent{Start: offset, Length: length})
	f.readPaths = append(f.readPaths, path)
	if offset+length > int64(len(f.image)) {
		return nil, fmt.Errorf("read past the end of the fake image")
	}
	return io.NopCloser(bytes.NewReader(f.image[offset : offset+length])), nil
}

// fakeSink is a destination disk in memory, enforcing the same alignment the
// real one does — a test destination that accepts what Hyper-V refuses proves
// nothing.
type fakeSink struct {
	buf []byte
}

func (s *fakeSink) WriteAt(b []byte, offset int64) error {
	if offset%4096 != 0 || int64(len(b))%4096 != 0 {
		return fmt.Errorf("unaligned write: %d bytes at %d", len(b), offset)
	}
	if offset+int64(len(b)) > int64(len(s.buf)) {
		return fmt.Errorf("write past the end")
	}
	copy(s.buf[offset:], b)
	return nil
}

func (s *fakeSink) Size() int64 { return int64(len(s.buf)) }

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		// Position-dependent, so a block written at the wrong offset does not
		// happen to match the bytes that belong there.
		b[i] = byte(i*7 + i/4096)
	}
	return b
}

/*
What lands at the destination must be what was at the same offset in the

	source, and only the changed extents may move.
*/
func TestOnlyTheChangedRangesMoveAndTheyLandWhereTheyCameFrom(t *testing.T) {
	const size = 1 << 20
	src := &fakeSource{
		image:   pattern(size),
		extents: []Extent{{Start: 8192, Length: 4096}, {Start: 65536, Length: 8192}},
		next:    "52 de c0 d3/7",
	}
	dst := &fakeSink{buf: make([]byte, size)}

	res, err := CopyDisk(t.Context(), src, "vm-1", "snapshot-1",
		Disk{Key: 2000, Label: "Hard disk 1", Path: "[ds1] Web01/Web01.vmdk", SizeBytes: size}, "*", dst, nil)
	if err != nil {
		t.Fatalf("copy failed: %v", err)
	}
	if res.BytesCopied != 12288 {
		t.Errorf("copied %d bytes, want 12288", res.BytesCopied)
	}
	if res.NextChangeID != "52 de c0 d3/7" {
		t.Errorf("the next marker is %q, not the one the source gave", res.NextChangeID)
	}
	for _, e := range src.extents {
		if !bytes.Equal(dst.buf[e.Start:e.Start+e.Length], src.image[e.Start:e.Start+e.Length]) {
			t.Errorf("the range at %d does not match the source", e.Start)
		}
	}
	// Untouched regions stay untouched: copying more than was asked for is not
	// harmless when the destination already holds a base copy.
	if !bytes.Equal(dst.buf[0:8192], make([]byte, 8192)) {
		t.Error("a range that was not reported as changed was written anyway")
	}
}

/*
A range VMware reports off a sector boundary is widened, not narrowed — and

	the widened read must still land at the offset it was read from.
*/
func TestARaggedRangeIsWidenedAndStillLandsCorrectly(t *testing.T) {
	const size = 1 << 20
	src := &fakeSource{image: pattern(size), extents: []Extent{{Start: 5000, Length: 100}}, next: "x/1"}
	dst := &fakeSink{buf: make([]byte, size)}

	if _, err := CopyDisk(t.Context(), src, "vm-1", "snap-1",
		Disk{Label: "Hard disk 1", SizeBytes: size}, "*", dst, nil); err != nil {
		t.Fatalf("copy failed: %v", err)
	}
	if len(src.reads) != 1 || src.reads[0].Start != 4096 || src.reads[0].Length != 4096 {
		t.Fatalf("read %+v, want one 4096-byte read at 4096", src.reads)
	}
	if !bytes.Equal(dst.buf[4096:8192], src.image[4096:8192]) {
		t.Error("the widened range did not land at the offset it was read from")
	}
}

/*
A single extent larger than one request is split, and the pieces must be

	contiguous and cover it exactly. A gap here is a hole in the guest's disk.
*/
func TestALargeExtentIsSplitWithoutGapsOrOverlap(t *testing.T) {
	const size = 96 << 20
	src := &fakeSource{image: pattern(size), extents: []Extent{{Start: 0, Length: 80 << 20}}, next: "x/1"}
	dst := &fakeSink{buf: make([]byte, size)}

	res, err := CopyDisk(t.Context(), src, "vm-1", "snap-1", Disk{Label: "Hard disk 1", SizeBytes: size}, "*", dst, nil)
	if err != nil {
		t.Fatalf("copy failed: %v", err)
	}
	if res.BytesCopied != 80<<20 {
		t.Errorf("copied %d, want %d", res.BytesCopied, 80<<20)
	}
	var at int64
	for _, r := range src.reads {
		if r.Start != at {
			t.Fatalf("a read starts at %d, leaving a gap or overlap after %d", r.Start, at)
		}
		if r.Length > readChunk {
			t.Fatalf("a read of %d bytes is larger than one request allows", r.Length)
		}
		at += r.Length
	}
	if at != 80<<20 {
		t.Errorf("the reads cover %d bytes, not the whole extent", at)
	}
	if !bytes.Equal(dst.buf[:80<<20], src.image[:80<<20]) {
		t.Error("the reassembled extent does not match the source")
	}
}

/*
Progress is reported against the total for the pass, not the disk. A delta

	of 200MB shown as "200MB of 500GB" reads as barely started.
*/
func TestProgressIsReportedAgainstThePassNotTheDisk(t *testing.T) {
	const size = 96 << 20
	src := &fakeSource{image: pattern(size), extents: []Extent{{Start: 0, Length: 64 << 20}}, next: "x/1"}
	dst := &fakeSink{buf: make([]byte, size)}

	var lastCopied, lastTotal int64
	calls := 0
	_, err := CopyDisk(t.Context(), src, "vm", "snap", Disk{Label: "Hard disk 1", SizeBytes: size}, "*", dst,
		func(copied, total int64) { lastCopied, lastTotal, calls = copied, total, calls+1 })
	if err != nil {
		t.Fatalf("copy failed: %v", err)
	}
	if calls < 2 {
		t.Errorf("progress was reported %d times for a multi-request extent", calls)
	}
	if lastTotal != 64<<20 || lastCopied != lastTotal {
		t.Errorf("progress ended at %d of %d, want %d of %d", lastCopied, lastTotal, 64<<20, 64<<20)
	}
}

/*
A source disk bigger than the destination means it grew in VMware after the

	destination was created. Copying what fits would produce a VM that boots and
	is missing the end of its disk.
*/
func TestASourceLargerThanTheDestinationIsRefusedBeforeAnythingMoves(t *testing.T) {
	src := &fakeSource{image: pattern(1 << 20)}
	dst := &fakeSink{buf: make([]byte, 8192)}
	_, err := CopyDisk(t.Context(), src, "vm", "snap", Disk{Label: "Hard disk 1", SizeBytes: 1 << 20}, "*", dst, nil)
	if err == nil {
		t.Fatal("a source larger than the destination was accepted")
	}
	if !strings.Contains(err.Error(), "grown since this migration started") {
		t.Errorf("the refusal does not say what happened: %v", err)
	}
	if len(src.reads) != 0 {
		t.Error("bytes were read before the size was checked")
	}
}

/*
The last sector of a disk whose size is not a whole number of sectors cannot

	be written aligned. Finding that out after hours of copying is the worst
	possible moment, so it is refused before the first read.
*/
func TestASizeThatCannotBeWrittenAlignedIsRefusedUpFront(t *testing.T) {
	src := &fakeSource{image: pattern(1 << 20), extents: []Extent{{Start: 0, Length: 512}}}
	dst := &fakeSink{buf: make([]byte, 1<<20)}
	_, err := CopyDisk(t.Context(), src, "vm", "snap", Disk{Label: "Hard disk 2", SizeBytes: 4096*10 + 512}, "*", dst, nil)
	if err == nil {
		t.Fatal("a size that is not a multiple of the sector size was accepted")
	}
	if !strings.Contains(err.Error(), "Hard disk 2") {
		t.Errorf("the refusal does not name the disk: %v", err)
	}
	if len(src.reads) != 0 {
		t.Error("bytes were read before the size was checked")
	}
}

/*
A short read stops the copy: writing the partial read would leave the rest of

	that range holding whatever was there before, with nothing reporting it.

	What it must NOT do is explain itself. This message used to conclude "the disk
	file is not the size its configuration reports", which was one cause of a
	short read stated as the finding — and on the first real migration it was the
	wrong one: the file being read was the descriptor, not the disk. The byte
	count is what tells those apart, so the byte count is what it reports.
*/
func TestAShortReadStopsTheCopyRatherThanWritingPartOfIt(t *testing.T) {
	src := &shortSource{fakeSource{image: pattern(1 << 20), extents: []Extent{{Start: 0, Length: 8192}}, next: "x/1"}}
	dst := &fakeSink{buf: make([]byte, 1<<20)}
	_, err := CopyDisk(t.Context(), src, "vm", "snap", Disk{Label: "Hard disk 1", Path: "[ds1] a.vmdk", SizeBytes: 1 << 20}, "*", dst, nil)
	if err == nil {
		t.Fatal("a short read was accepted")
	}
	for _, want := range []string{"8192", "4096", "[ds1] a.vmdk"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not report %q — what was asked for, what came back, and from where: %v", want, err)
		}
	}
	// The old conclusion, which was a guess wearing a fact's clothes.
	if strings.Contains(err.Error(), "not the size its configuration reports") {
		t.Errorf("the failure asserts a cause it did not establish: %v", err)
	}
	if !bytes.Equal(dst.buf[:8192], make([]byte, 8192)) {
		t.Error("part of a short read was written to the destination")
	}
}

type shortSource struct{ fakeSource }

func (s *shortSource) ReadAt(_ context.Context, _ string, offset, length int64) (io.ReadCloser, error) {
	s.reads = append(s.reads, Extent{Start: offset, Length: length})
	return io.NopCloser(bytes.NewReader(s.image[offset : offset+length/2])), nil
}

// A cancelled job stops rather than finishing the pass.
func TestCancellationStopsTheCopy(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	src := &fakeSource{image: pattern(1 << 20), extents: []Extent{{Start: 0, Length: 4096}}, next: "x/1"}
	dst := &fakeSink{buf: make([]byte, 1<<20)}
	if _, err := CopyDisk(ctx, src, "vm", "snap", Disk{Label: "Hard disk 1", SizeBytes: 1 << 20}, "*", dst, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled copy returned %v", err)
	}
	if len(src.reads) != 0 {
		t.Error("a cancelled copy still read from the source")
	}
}

/*
A pass that copied nothing still returns the new marker. Storing the old one

	would leave the next pass re-copying the same window for ever.
*/
func TestAPassWithNoChangesStillAdvancesTheMarker(t *testing.T) {
	src := &fakeSource{image: pattern(1 << 20), next: "52 de/9"}
	dst := &fakeSink{buf: make([]byte, 1<<20)}
	res, err := CopyDisk(t.Context(), src, "vm", "snap", Disk{Label: "Hard disk 1", SizeBytes: 1 << 20}, "52 de/8", dst, nil)
	if err != nil {
		t.Fatalf("copy failed: %v", err)
	}
	if res.BytesCopied != 0 || res.Extents != 0 {
		t.Errorf("an empty pass copied %d bytes in %d extents", res.BytesCopied, res.Extents)
	}
	if res.NextChangeID != "52 de/9" {
		t.Errorf("the marker did not advance: %q", res.NextChangeID)
	}
}

/*
Snapshot names carry the migration's identity, so a leftover one can be

	traced back to what made it instead of looking like anybody's.
*/
func TestASnapshotNameNamesItsMigration(t *testing.T) {
	n := SnapshotName("mig-7f2c")
	if !strings.HasPrefix(n, "ballast-") || !strings.Contains(n, "mig-7f2c") {
		t.Errorf("snapshot name %q does not identify Ballast and the migration", n)
	}
	if n == SnapshotName("mig-9a11") {
		t.Error("two migrations produce the same snapshot name")
	}
}
