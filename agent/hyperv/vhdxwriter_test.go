package hyperv

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/* Finding where guest data starts in a fixed VHDX.

   This is the one place in the migration path where being wrong does not fail.
   A bad payload offset writes every delta a few megabytes off, the copy reports
   success, and the VM boots into corruption days later. So the offset is READ
   from the block allocation table rather than assumed from qemu-img's habits,
   and these build real structures to prove the reading is right.

   The BAT region GUID is 2DC27766-F623-4200-9D64-115E9BFD4A08, stored with its
   first three fields little-endian as GUIDs are on disk. */

const batGUIDHex = "6677c22d23f600429d64115e9bfd4a08"

// fakeVHDX writes just enough of a VHDX for the layout reader: a region table
// naming the BAT, and BAT entries at that offset.
func fakeVHDX(t *testing.T, entries []uint64, payloadBytes int64) string {
	t.Helper()
	const batAt = 1 << 20 // where we put the BAT itself
	path := filepath.Join(t.TempDir(), "disk.vhdx")

	// Big enough to hold the header, the BAT and some payload.
	size := batAt + 4096 + payloadBytes + (3 << 20)
	buf := make([]byte, size)

	// Region table at 192KB: "regi", then the entry count, then entries of
	// {GUID(16), offset(8), length(4), required(4)}.
	binary.LittleEndian.PutUint32(buf[vhdxRegionTableOffset:], vhdxRegionSignature)
	binary.LittleEndian.PutUint32(buf[vhdxRegionTableOffset+8:], 1)
	guid := unhex(t, batGUIDHex)
	copy(buf[vhdxRegionTableOffset+16:], guid)
	binary.LittleEndian.PutUint64(buf[vhdxRegionTableOffset+32:], uint64(batAt))
	binary.LittleEndian.PutUint32(buf[vhdxRegionTableOffset+40:], 4096)

	for i, e := range entries {
		binary.LittleEndian.PutUint64(buf[batAt+i*8:], e)
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	out := make([]byte, len(s)/2)
	for i := range out {
		var v int
		for j := 0; j < 2; j++ {
			c := s[i*2+j]
			d := 0
			switch {
			case c >= '0' && c <= '9':
				d = int(c - '0')
			case c >= 'a' && c <= 'f':
				d = int(c-'a') + 10
			default:
				t.Fatalf("bad hex %q", s)
			}
			v = v*16 + d
		}
		out[i] = byte(v)
	}
	return out
}

// batEntry packs a payload block: state in the low 3 bits, offset in MB above
// bit 20. State 6 is PAYLOAD_BLOCK_FULLY_PRESENT.
func batEntry(offsetMB uint64, state uint64) uint64 { return (offsetMB << 20) | state }

func TestPayloadOffsetIsReadFromTheTableNotAssumed(t *testing.T) {
	// Payload at 4MB, next block at 6MB — a 2MB block size.
	path := fakeVHDX(t, []uint64{batEntry(4, 6), batEntry(6, 6)}, 8<<20)
	got, err := readVHDXLayout(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.PayloadOffset != 4<<20 {
		t.Errorf("payload offset = %d, want %d", got.PayloadOffset, 4<<20)
	}
	if got.BlockSize != 2<<20 {
		t.Errorf("block size = %d, want %d", got.BlockSize, 2<<20)
	}
}

/*
A DYNAMIC VHDX has blocks that are not all present, and writing into one by

	guest offset would put data in the wrong place with no error at all. Refused
	loudly instead.
*/
func TestADynamicVHDXIsRefusedRatherThanPatched(t *testing.T) {
	// State 1 is PAYLOAD_BLOCK_UNDEFINED — not a fixed image.
	path := fakeVHDX(t, []uint64{batEntry(4, 1)}, 8<<20)
	_, err := readVHDXLayout(path)
	if err == nil {
		t.Fatal("a dynamic image was accepted for offset writing")
	}
	if !strings.Contains(err.Error(), "would corrupt it") {
		t.Errorf("the refusal does not say what the risk is: %v", err)
	}
}

/*
Blocks laid out backwards are not something a constant offset can map, and

	guessing would be silent corruption.
*/
func TestOutOfOrderBlocksAreRefused(t *testing.T) {
	path := fakeVHDX(t, []uint64{batEntry(6, 6), batEntry(4, 6)}, 8<<20)
	if _, err := readVHDXLayout(path); err == nil {
		t.Fatal("blocks in descending order were accepted")
	}
}

func TestSomethingThatIsNotAVHDXIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.vhdx")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readVHDXLayout(path)
	if err == nil || !strings.Contains(err.Error(), "not a VHDX") {
		t.Fatalf("err = %v, want a clear not-a-VHDX refusal", err)
	}
}

/*
Writing at a guest offset must land at payload+offset in the file. Off by a

	header is exactly the bug this whole file exists to prevent.
*/
func TestAWriteLandsAtPayloadPlusOffset(t *testing.T) {
	path := fakeVHDX(t, []uint64{batEntry(4, 6), batEntry(6, 6)}, 8<<20)
	p, err := openVHDXPatcher(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("BALLAST")
	if err := p.WriteAt(want, 512); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := make([]byte, len(want))
	if _, err := f.ReadAt(got, (4<<20)+512); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("data landed elsewhere: read %q at payload+512", got)
	}
}

/*
A range past the end of the disk means the tracking marker no longer matches

	this image. Extending the file would produce a VHDX that looks fine and boots
	into corruption, so the copy stops instead.
*/
func TestAWritePastTheEndIsRefused(t *testing.T) {
	path := fakeVHDX(t, []uint64{batEntry(4, 6), batEntry(6, 6)}, 8<<20)
	p, err := openVHDXPatcher(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	err = p.WriteAt(make([]byte, 4096), 1<<40)
	if err == nil {
		t.Fatal("a write past the end of the disk was accepted")
	}
	if !strings.Contains(err.Error(), "no longer matches this image") {
		t.Errorf("the refusal does not explain what it means: %v", err)
	}
}

func TestANegativeOffsetIsRefused(t *testing.T) {
	path := fakeVHDX(t, []uint64{batEntry(4, 6), batEntry(6, 6)}, 8<<20)
	p, _ := openVHDXPatcher(path)
	defer p.Close()
	if err := p.WriteAt([]byte("x"), -1); err == nil {
		t.Fatal("a negative offset was accepted")
	}
}
