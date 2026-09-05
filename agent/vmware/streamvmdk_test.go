package vmware

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

/* Reading a stream-optimised VMDK.

   Every test here is about a way of getting a disk that looks finished and is
   wrong, because that is the only interesting failure mode: a stream is decoded
   once, sequentially, straight into the destination, and there is no second
   pass to notice that the first one dropped something. A truncated download, a
   grain written at the wrong offset, or a stream that is not what it claims all
   produce a VHDX that mounts, boots part of the way, and fails days later.

   The fixtures are built rather than captured, so the shapes that matter — a
   missing footer, a grain past the end of the disk — can be produced at all. */

const (
	testGrainSectors = 128 // 64KB, what vCenter actually serves
	testGrainBytes   = testGrainSectors * vmdkSector
)

type builtGrain struct {
	sector uint64
	data   []byte
}

// buildStream assembles a stream-optimised VMDK the way an export lease serves
// one: header, descriptor, grains, then a footer and an end-of-stream marker.
func buildStream(capacitySectors uint64, grains []builtGrain, opts ...func(*streamBuild)) []byte {
	/* overHead 128 because that is what vCenter actually sends.

	   The fixtures used to have none, and that omission is what let a real
	   migration fail: with descriptorOffset=1 and descriptorSize=1 the data
	   still begins at sector 128, and the 126 sectors between are zeros. A
	   decoder that computed the start from the descriptor landed in them, read
	   a zero sector as a valid end-of-stream marker, and finished having copied
	   nothing. Invented fixtures agreed with it. */
	b := streamBuild{capacity: capacitySectors, grainSectors: testGrainSectors, footer: true, eos: true,
		descriptor: 1, overHead: 128}
	for _, o := range opts {
		o(&b)
	}
	var out bytes.Buffer
	out.Write(b.header())
	// The descriptor, then padding out to where the data really starts.
	for i := uint64(1); i < b.dataStart(); i++ {
		out.Write(make([]byte, vmdkSector))
	}
	for _, g := range grains {
		out.Write(grainMarker(g.sector, g.data))
	}
	if b.footer {
		out.Write(metaMarker(1, markerFooter))
		out.Write(b.header())
	}
	if b.eos {
		out.Write(metaMarker(0, markerEOS))
	}
	return out.Bytes()
}

type streamBuild struct {
	capacity     uint64
	grainSectors uint64
	descriptor   uint64
	overHead     uint64
	flags        uint32
	compress     uint16
	magic        string
	footer       bool
	eos          bool
}

// dataStart mirrors what the real producer does: the data begins at overHead
// when it says anything, otherwise straight after the descriptor.
func (b streamBuild) dataStart() uint64 {
	after := 1 + b.descriptor
	if b.overHead > after {
		return b.overHead
	}
	return after
}

func (b streamBuild) header() []byte {
	h := make([]byte, vmdkSector)
	magic := b.magic
	if magic == "" {
		magic = "KDMV"
	}
	copy(h[0:4], magic)
	binary.LittleEndian.PutUint32(h[4:8], 3)
	flags := b.flags
	if flags == 0 {
		flags = 1 | flagCompressed | flagMarkers
	}
	binary.LittleEndian.PutUint32(h[8:12], flags)
	binary.LittleEndian.PutUint64(h[12:20], b.capacity)
	binary.LittleEndian.PutUint64(h[20:28], b.grainSectors)
	binary.LittleEndian.PutUint64(h[28:36], 1)
	binary.LittleEndian.PutUint64(h[36:44], b.descriptor)
	binary.LittleEndian.PutUint64(h[64:72], b.overHead)
	compress := b.compress
	if compress == 0 {
		compress = compressDeflate
	}
	binary.LittleEndian.PutUint16(h[77:79], compress)
	return h
}

func grainMarker(sector uint64, data []byte) []byte {
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write(data)
	w.Close()

	total := align(int64(z.Len())+12, vmdkSector)
	m := make([]byte, total)
	binary.LittleEndian.PutUint64(m[0:8], sector)
	binary.LittleEndian.PutUint32(m[8:12], uint32(z.Len()))
	copy(m[12:], z.Bytes())
	return m
}

func metaMarker(sectors uint64, kind uint32) []byte {
	m := make([]byte, vmdkSector)
	binary.LittleEndian.PutUint64(m[0:8], sectors)
	binary.LittleEndian.PutUint32(m[8:12], 0)
	binary.LittleEndian.PutUint32(m[12:16], kind)
	return m
}

// fill makes a recognisable grain: every sector stamped with its own number, so
// a grain written to the wrong offset is visible rather than plausible.
func fill(stamp byte) []byte {
	b := make([]byte, testGrainBytes)
	for i := range b {
		b[i] = stamp
	}
	return b
}

type written struct {
	offset int64
	data   []byte
}

func collect(t *testing.T, stream []byte) ([]written, int64, error) {
	t.Helper()
	var got []written
	n, err := decodeStreamVMDK(bytes.NewReader(stream), func(off int64, d []byte) error {
		got = append(got, written{off, append([]byte(nil), d...)})
		return nil
	})
	return got, n, err
}

// The ordinary case: three grains scattered across a disk, each landing at the
// sector the stream named rather than where it happened to appear in the file.
func TestGrainsLandAtTheOffsetTheStreamNames(t *testing.T) {
	const capacity = 2048 * testGrainSectors // 128MB
	stream := buildStream(capacity, []builtGrain{
		{sector: 0, data: fill(0xA1)},
		{sector: 512 * testGrainSectors, data: fill(0xB2)},
		{sector: 2047 * testGrainSectors, data: fill(0xC3)},
	})

	got, n, err := collect(t, stream)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d grains, want 3", len(got))
	}
	want := []int64{0, 512 * testGrainBytes, 2047 * testGrainBytes}
	for i, w := range want {
		if got[i].offset != w {
			t.Errorf("grain %d landed at %d, want %d", i, got[i].offset, w)
		}
		if len(got[i].data) != testGrainBytes {
			t.Errorf("grain %d is %d bytes, want %d", i, len(got[i].data), testGrainBytes)
		}
	}
	for i, stamp := range []byte{0xA1, 0xB2, 0xC3} {
		if got[i].data[0] != stamp || got[i].data[len(got[i].data)-1] != stamp {
			t.Errorf("grain %d holds the wrong data — grains were paired with the wrong offsets", i)
		}
	}
	// DISK bytes, not wire bytes: the wire is compressed and a meter drawn from
	// it would mean nothing to anybody watching a copy.
	if n != 3*testGrainBytes {
		t.Errorf("reported %d bytes copied, want %d disk bytes", n, 3*testGrainBytes)
	}
}

// A stream carries grains only where the disk has data. Empty space never
// crosses the wire, which is the one advantage this channel has.
func TestEmptySpaceIsNeverSent(t *testing.T) {
	const capacity = 4096 * testGrainSectors
	stream := buildStream(capacity, []builtGrain{{sector: 0, data: fill(0x01)}})
	got, n, err := collect(t, stream)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(got) != 1 || n != testGrainBytes {
		t.Fatalf("a mostly-empty disk moved %d bytes in %d grains", n, len(got))
	}
}

/* THE test. A download cut short must not decode as a finished disk.

   Everything before the cut is real data at real offsets, so the destination
   looks correct and is missing its tail. The footer is written after every
   grain, so its absence is the only reliable evidence that the stream did not
   finish. */
func TestATruncatedStreamIsNotASuccess(t *testing.T) {
	const capacity = 64 * testGrainSectors
	full := buildStream(capacity, []builtGrain{
		{sector: 0, data: fill(0xA1)},
		{sector: testGrainSectors, data: fill(0xB2)},
	})

	for _, cut := range []struct {
		name string
		at   int
	}{
		{"mid-grain", len(full) - vmdkSector*3},
		{"after the last grain but before the footer", len(full) - vmdkSector*3},
		{"one sector from the end", len(full) - vmdkSector},
	} {
		_, _, err := collect(t, full[:cut.at])
		if err == nil {
			t.Errorf("a stream cut %s decoded as a complete disk", cut.name)
		}
	}
}

// A stream whose grains all arrived but whose footer never did is the same
// failure wearing a tidier hat: the end-of-stream marker alone is not proof the
// sender finished.
func TestAnEndOfStreamWithoutAFooterIsRefused(t *testing.T) {
	const capacity = 64 * testGrainSectors
	stream := buildStream(capacity, []builtGrain{{sector: 0, data: fill(0xA1)}},
		func(b *streamBuild) { b.footer = false })

	_, _, err := collect(t, stream)
	if err == nil {
		t.Fatal("a stream that never sent a footer was accepted as complete")
	}
	if !strings.Contains(err.Error(), "without a footer") {
		t.Errorf("the refusal does not say what was missing: %v", err)
	}
}

/* A sparse VMDK that is not stream-optimised has no markers, so reading it this
   way would decode its grain directory as data and write it somewhere plausible.
   Refused on the header rather than discovered as corruption. */
func TestAPlainSparseVMDKIsRefusedRatherThanMisread(t *testing.T) {
	stream := buildStream(64*testGrainSectors, nil, func(b *streamBuild) { b.flags = 1 })
	_, _, err := collect(t, stream)
	if err == nil {
		t.Fatal("a VMDK with no grain markers was read as a stream")
	}
	if !strings.Contains(err.Error(), "not a stream-optimised one") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
}

// Anything that is not a VMDK at all — an error page, an HTML redirect, a
// truncated TLS handshake — must not be read as one.
func TestSomethingThatIsNotAVMDKIsNamedAsSuch(t *testing.T) {
	_, _, err := collect(t, []byte(strings.Repeat("<html>not a disk</html>", 64)))
	if err == nil {
		t.Fatal("a web page decoded as a disk")
	}
	if !strings.Contains(err.Error(), "does not start with a VMDK header") {
		t.Errorf("the refusal does not say what arrived: %v", err)
	}
}

// A grain the stream places beyond the disk it declares is a contradiction, and
// writing it would run off the end of the destination.
func TestAGrainPastTheEndOfTheDiskIsRefused(t *testing.T) {
	const capacity = 4 * testGrainSectors
	stream := buildStream(capacity, []builtGrain{{sector: 99 * testGrainSectors, data: fill(0xFF)}})
	_, _, err := collect(t, stream)
	if err == nil {
		t.Fatal("a grain outside the disk was written")
	}
	if !strings.Contains(err.Error(), "outside the") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
}

/* The last grain of a disk whose capacity is not a whole number of grains is
   trimmed to the disk, not written past it. Capacity stays a whole number of
   destination sectors, so the trim still lands on a boundary a block device
   will take. */
func TestTheFinalGrainIsTrimmedToTheDisk(t *testing.T) {
	// Half a grain short of three full ones.
	capacity := uint64(2*testGrainSectors + testGrainSectors/2)
	stream := buildStream(capacity, []builtGrain{
		{sector: 0, data: fill(0x11)},
		{sector: 2 * testGrainSectors, data: fill(0x33)},
	})
	got, n, err := collect(t, stream)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	last := got[len(got)-1]
	if want := int64(testGrainBytes / 2); int64(len(last.data)) != want {
		t.Errorf("the final grain was written as %d bytes, want %d — the rest is past the end of the disk",
			len(last.data), want)
	}
	// One full grain plus the trimmed half. The grain in between was never sent,
	// which is the point of a sparse stream.
	if want := int64(testGrainBytes + testGrainBytes/2); n != want {
		t.Errorf("reported %d bytes, want %d", n, want)
	}
}

// A sink that fails stops the decode there and gives its error back unchanged.
// A write that failed and a copy that carried on would leave a hole.
func TestAFailedWriteStopsTheDecode(t *testing.T) {
	stream := buildStream(64*testGrainSectors, []builtGrain{
		{sector: 0, data: fill(0x11)},
		{sector: testGrainSectors, data: fill(0x22)},
	})
	boom := errors.New("the destination disk went away")
	var seen int
	_, err := decodeStreamVMDK(bytes.NewReader(stream), func(int64, []byte) error {
		seen++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("the write error was swallowed or rewritten: %v", err)
	}
	if seen != 1 {
		t.Errorf("the decode carried on after a failed write, writing %d grains", seen)
	}
}

// Grain size and capacity have to be whole numbers of destination sectors, and
// the check belongs on the header rather than eighty gigabytes later.
func TestAGrainSizeTheDestinationCannotTakeIsRefusedUpFront(t *testing.T) {
	stream := buildStream(64*testGrainSectors, nil, func(b *streamBuild) { b.grainSectors = 1 })
	_, _, err := collect(t, stream)
	if err == nil {
		t.Fatal("a 512-byte grain size was accepted for a 4096-byte-sector destination")
	}
	if !strings.Contains(err.Error(), "whole number of") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
}

/* The padding between the descriptor and the data, which is where the first
   real warm migration ended.

   vCenter sends descriptorOffset=1, descriptorSize=1 and overHead=128: the
   descriptor ends at sector 2 and the data starts at sector 128. Sectors 2 to
   127 are zeros, and a zero sector is a structurally valid end-of-stream
   marker — so a decoder that trusted the descriptor arithmetic stopped at
   sector 2, having copied nothing, and said so in the language of a stream that
   had finished. Measured on BallastJumphost, not read off a specification. */
func TestTheDataStartsAtOverHeadNotAfterTheDescriptor(t *testing.T) {
	const capacity = 64 * testGrainSectors
	stream := buildStream(capacity, []builtGrain{
		{sector: 0, data: fill(0xA1)},
		{sector: testGrainSectors, data: fill(0xB2)},
	})
	// The fixture must actually contain the padding, or this proves nothing.
	if len(stream) < 128*vmdkSector {
		t.Fatalf("the fixture has no padding to skip: %d bytes", len(stream))
	}
	if !isAllZero(stream[2*vmdkSector : 128*vmdkSector]) {
		t.Fatal("the fixture's padding is not zeros, so it does not reproduce the failure")
	}

	got, n, err := collect(t, stream)
	if err != nil {
		t.Fatalf("the padding was read as the end of the stream: %v", err)
	}
	if len(got) != 2 || n != 2*testGrainBytes {
		t.Fatalf("decoded %d grains and %d bytes, want 2 and %d", len(got), n, 2*testGrainBytes)
	}
	if got[0].data[0] != 0xA1 || got[1].data[0] != 0xB2 {
		t.Error("the grains after the padding were misread")
	}
}

// A producer that leaves overHead at zero still works: the descriptor is then
// the best answer available and there is no padding to skip.
func TestAStreamWithNoOverHeadStillReadsFromAfterTheDescriptor(t *testing.T) {
	stream := buildStream(64*testGrainSectors, []builtGrain{{sector: 0, data: fill(0x77)}},
		func(b *streamBuild) { b.overHead = 0 })
	got, _, err := collect(t, stream)
	if err != nil {
		t.Fatalf("a stream without overHead failed: %v", err)
	}
	if len(got) != 1 || got[0].data[0] != 0x77 {
		t.Fatalf("decoded %d grains", len(got))
	}
}

// And overHead is only trusted when it is FURTHER than the descriptor. One that
// points backwards into the descriptor would have the reader decode text.
func TestAnOverHeadInsideTheDescriptorIsIgnored(t *testing.T) {
	stream := buildStream(64*testGrainSectors, []builtGrain{{sector: 0, data: fill(0x55)}},
		func(b *streamBuild) { b.descriptor = 4; b.overHead = 2 })
	got, _, err := collect(t, stream)
	if err != nil {
		t.Fatalf("an overHead pointing inside the descriptor was followed: %v", err)
	}
	if len(got) != 1 || got[0].data[0] != 0x55 {
		t.Fatalf("decoded %d grains", len(got))
	}
}

func isAllZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
