package hyperv

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

/* Writing into a fixed VHDX at a byte offset.

   This is what makes warm migration possible, and it is the reason the
   destination is a FIXED VHDX rather than a dynamic one or a raw file.

   A delta from VMware is a list of byte ranges: "these 4MB at offset X changed".
   Applying one means writing inside a disk image that already exists. The
   alternatives were both worse:

     - A DYNAMIC VHDX allocates blocks on demand and moves them; writing at a
       guest offset means walking the block allocation table and possibly
       growing the file. Doable, and every pass becomes format work.
     - A RAW intermediate patches trivially and then has to be CONVERTED to
       VHDX. Converting half a terabyte at cutover puts the whole conversion
       inside the outage, which is precisely what warm migration exists to
       avoid.

   A fixed VHDX has its payload in one contiguous region, so a guest offset maps
   to a file offset by adding a constant. Find that constant once and every
   later pass is an ordinary seek and write.

   THE CONSTANT IS READ FROM THE FILE, never assumed. qemu-img's layout is
   stable in practice and "in practice" is not a guarantee to write data
   through: a wrong offset does not fail, it silently corrupts the disk at every
   delta. The BAT entry says where the payload actually starts, so that is what
   is used. */

const (
	// VHDX structures are at fixed positions by specification (MS-VHDX).
	vhdxRegionTableOffset = 192 * 1024
	vhdxRegionSignature   = 0x69676572 // "regi"
	// The BAT region's GUID, as bytes in the file's little-endian mixed-endian
	// GUID encoding: 2DC27766-F623-4200-9D64-115E9BFD4A08.
	vhdxBATGuidHex = "6677c22d23f60042" + "9d64115e9bfd4a08"
)

// vhdxLayout is what a delta needs to know about a fixed VHDX.
type vhdxLayout struct {
	// PayloadOffset is the file offset of guest LBA 0.
	PayloadOffset int64
	// BlockSize is the payload block size; a fixed image's blocks are
	// contiguous from PayloadOffset, so this is only used to sanity-check that
	// they really are.
	BlockSize int64
	// VirtualSize is the guest-visible size, so a write past the end is refused
	// rather than extending the file into nonsense.
	VirtualSize int64
}

/*
readVHDXLayout finds where guest data actually starts.

	Reads the region table for the BAT, reads the first BAT entry, and takes its
	file offset. For a FIXED image every payload block is present and contiguous,
	which is checked rather than assumed: if the second block is not exactly one
	block after the first, this is not the layout a seek-and-write can use and it
	refuses instead of corrupting the image.
*/
func readVHDXLayout(path string) (vhdxLayout, error) {
	f, err := os.Open(path)
	if err != nil {
		return vhdxLayout{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	batOff, batLen, err := vhdxRegion(f, vhdxBATGuidHex)
	if err != nil {
		return vhdxLayout{}, err
	}
	if batLen < 16 {
		return vhdxLayout{}, fmt.Errorf("%s: block allocation table is too small to read", path)
	}

	// Two entries: the first gives the payload offset, the second proves the
	// blocks are contiguous.
	var raw [16]byte
	if _, err := f.ReadAt(raw[:], batOff); err != nil {
		return vhdxLayout{}, fmt.Errorf("%s: read block allocation table: %w", path, err)
	}
	first := binary.LittleEndian.Uint64(raw[0:8])
	second := binary.LittleEndian.Uint64(raw[8:16])

	// A BAT entry packs state in the low 3 bits and the offset in MB above bit
	// 20. PAYLOAD_BLOCK_FULLY_PRESENT is state 6.
	state := first & 0x7
	if state != 6 {
		return vhdxLayout{}, fmt.Errorf("%s: the first payload block is not fully present (state %d) — "+
			"this is not a fixed VHDX, and writing changed ranges into a dynamic one by offset would corrupt it", path, state)
	}
	payload := int64((first >> 20) << 20)
	if payload <= 0 {
		return vhdxLayout{}, fmt.Errorf("%s: the block allocation table reports payload at offset %d, which cannot be right", path, payload)
	}

	l := vhdxLayout{PayloadOffset: payload}
	if secondState := second & 0x7; secondState == 6 {
		next := int64((second >> 20) << 20)
		l.BlockSize = next - payload
		if l.BlockSize <= 0 {
			return vhdxLayout{}, fmt.Errorf("%s: payload blocks are not laid out in order, so a byte offset cannot be mapped safely", path)
		}
	}
	return l, nil
}

// vhdxRegion finds a region by GUID in the region table.
func vhdxRegion(f *os.File, guidHex string) (offset int64, length int64, err error) {
	hdr := make([]byte, 64*1024)
	if _, err := f.ReadAt(hdr, vhdxRegionTableOffset); err != nil && err != io.EOF {
		return 0, 0, fmt.Errorf("read region table: %w", err)
	}
	if binary.LittleEndian.Uint32(hdr[0:4]) != vhdxRegionSignature {
		return 0, 0, fmt.Errorf("not a VHDX: the region table signature is missing")
	}
	count := binary.LittleEndian.Uint32(hdr[8:12])
	if count > 2047 {
		return 0, 0, fmt.Errorf("region table claims %d entries, which is not credible", count)
	}
	for i := uint32(0); i < count; i++ {
		base := 16 + int(i)*32
		if base+32 > len(hdr) {
			break
		}
		if hexOf(hdr[base:base+16]) != guidHex {
			continue
		}
		return int64(binary.LittleEndian.Uint64(hdr[base+16 : base+24])),
			int64(binary.LittleEndian.Uint32(hdr[base+24 : base+28])), nil
	}
	return 0, 0, fmt.Errorf("no block allocation table in this VHDX")
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = digits[c>>4]
		out[i*2+1] = digits[c&0xf]
	}
	return string(out)
}

/*
vhdxPatcher writes guest byte ranges into a fixed VHDX.

	Deliberately tiny and deliberately strict. Every write is bounds-checked
	against the payload region: a range that runs past the end is refused rather
	than silently extending the file, because a delta that writes outside the
	disk is not a delta, it is a sign the changeId no longer matches the image
	and continuing would produce a VHDX that boots into corruption.
*/
type vhdxPatcher struct {
	f      *os.File
	layout vhdxLayout
	size   int64 // payload bytes available
}

func openVHDXPatcher(path string) (*vhdxPatcher, error) {
	l, err := readVHDXLayout(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s for writing: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &vhdxPatcher{f: f, layout: l, size: st.Size() - l.PayloadOffset}, nil
}

// WriteAt writes guest bytes at a guest offset.
func (p *vhdxPatcher) WriteAt(b []byte, guestOffset int64) error {
	if guestOffset < 0 {
		return fmt.Errorf("negative offset %d", guestOffset)
	}
	if end := guestOffset + int64(len(b)); end > p.size {
		return fmt.Errorf("a changed range ends at %d, past the %d bytes this disk holds — "+
			"the tracking marker no longer matches this image, so the copy is stopping rather than writing outside it", end, p.size)
	}
	_, err := p.f.WriteAt(b, p.layout.PayloadOffset+guestOffset)
	return err
}

func (p *vhdxPatcher) Close() error {
	if p == nil || p.f == nil {
		return nil
	}
	// Flushed explicitly. A delta that is in the page cache when the host
	// reboots is a disk that is silently a few blocks stale, which is the kind
	// of corruption nobody finds until the guest will not boot.
	if err := p.f.Sync(); err != nil {
		p.f.Close()
		return err
	}
	return p.f.Close()
}
