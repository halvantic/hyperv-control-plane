package vmware

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
)

/* Decoding a stream-optimised VMDK.

   This is the format an ExportSnapshot lease serves, and it is the only way to
   read a RUNNING guest's disk: the datastore file interface refuses the base
   file while the guest holds it open, which is measured, not assumed —
   `Failed to open disk: NFC_FILE_LOCKED` with the file listed at exactly the
   disk's size.

   The lease is SEQUENTIAL. It sends no Accept-Ranges, no Content-Length, and a
   range asked at the end of the disk comes back 200 OK with the file from the
   beginning. So this cannot seek, cannot be resumed at an offset, and cannot
   serve a change-tracked delta. It reads once, start to finish.

   What it CAN do is skip empty space: a stream contains grains only where the
   disk has data, and each grain carries the sector it belongs at. A thin disk
   crosses the wire at roughly what it occupies rather than what it claims.

   The layout, after a 512-byte header and a text descriptor, is a sequence of
   512-byte-aligned markers:

     size != 0   a grain. val is its sector on the VIRTUAL DISK, size is the
                 length of the deflate stream that follows at byte 12.
     size == 0   metadata. type says which, and val sectors of payload follow
                 the marker's own sector: grain table, grain directory, footer,
                 or end of stream.

   The footer matters more than it looks. It is written last, after every grain,
   so a stream that ends without one was truncated — and a truncated disk that
   decoded without complaint is precisely the failure this codebase keeps
   guarding against: plausible, complete-looking, and wrong. */

const (
	// vmdkSector is the format's own unit. Not sectorSize: that one is the
	// destination's alignment and the two are different numbers that would
	// silently swap.
	vmdkSector = 512

	markerEOS    = 0
	markerGT     = 1
	markerGD     = 2
	markerFooter = 3

	// flagCompressed and flagMarkers are what make a sparse VMDK a
	// STREAM-optimised one. A sparse extent without them is a different layout
	// read a different way, so it is refused rather than misread.
	flagCompressed = 1 << 16
	flagMarkers    = 1 << 17

	// compressDeflate is the only compression the format defines.
	compressDeflate = 1
)

// streamHeader is the part of SparseExtentHeader this needs. The rest of the
// 512 bytes is padding and legacy fields nothing here reads.
type streamHeader struct {
	version          uint32
	flags            uint32
	capacitySectors  uint64
	grainSectors     uint64
	descriptorOffset uint64
	descriptorSize   uint64
	// overHead is the number of sectors before the data begins, and it is the
	// ONLY reliable answer to where the first grain marker sits. See dataStart.
	overHead          uint64
	compressAlgorithm uint16
}

/*
dataStart is the sector the first grain marker sits on.

	overHead when it says anything, and this is not a preference. vCenter's own
	export puts descriptorOffset=1, descriptorSize=1 and overHead=128: the
	descriptor ends at sector 2 and the data begins at sector 128, with 126
	sectors of padding between them. Computing the start from the descriptor
	lands in that padding — and a zero-filled sector is a structurally valid
	end-of-stream marker, so the decode ends immediately, reports success in its
	own terms, and produces an empty disk.

	That is not a hypothetical: it is what the first real warm migration did.
	Only the footer check turned it into a failure instead of an 80GB file of
	zeros. Measured against BallastJumphost on 2026-08-31, not read off a
	specification.

	The descriptor arithmetic is kept as the fallback for a producer that leaves
	overHead at zero, where it is the best answer available.
*/
func (h streamHeader) dataStart() uint64 {
	afterDescriptor := uint64(1)
	if h.descriptorSize > 0 && h.descriptorOffset >= 1 {
		afterDescriptor = h.descriptorOffset + h.descriptorSize
	}
	if h.overHead > afterDescriptor {
		return h.overHead
	}
	return afterDescriptor
}

// CapacityBytes is the size of the virtual disk the stream describes.
func (h streamHeader) CapacityBytes() int64 { return int64(h.capacitySectors) * vmdkSector }

/*
parseStreamHeader reads and CHECKS the header.

	Every check here is one that, skipped, produces a disk rather than an error:
	a sparse extent that is not stream-optimised decodes into nonsense at
	plausible offsets, and a grain size that is not a whole number of destination
	sectors cannot be written to a block device at all. Both are cheaper to
	refuse now than to discover at the end of an eighty-gigabyte copy.
*/
func parseStreamHeader(b []byte) (streamHeader, error) {
	var h streamHeader
	if len(b) < vmdkSector {
		return h, fmt.Errorf("vmware: the stream is %d bytes, too short to hold a VMDK header", len(b))
	}
	if string(b[0:4]) != "KDMV" {
		return h, fmt.Errorf("vmware: the stream does not start with a VMDK header (first bytes % x). "+
			"An export lease should serve application/x-vnd.vmware-streamVmdk; something else answered", b[:min(8, len(b))])
	}
	h.version = binary.LittleEndian.Uint32(b[4:8])
	h.flags = binary.LittleEndian.Uint32(b[8:12])
	h.capacitySectors = binary.LittleEndian.Uint64(b[12:20])
	h.grainSectors = binary.LittleEndian.Uint64(b[20:28])
	h.descriptorOffset = binary.LittleEndian.Uint64(b[28:36])
	h.descriptorSize = binary.LittleEndian.Uint64(b[36:44])
	h.overHead = binary.LittleEndian.Uint64(b[64:72])
	h.compressAlgorithm = binary.LittleEndian.Uint16(b[77:79])

	if h.flags&flagMarkers == 0 || h.flags&flagCompressed == 0 {
		return h, fmt.Errorf("vmware: this is a sparse VMDK but not a stream-optimised one (flags %#x). "+
			"It has no grain markers, so it cannot be read as a stream", h.flags)
	}
	if h.compressAlgorithm != compressDeflate {
		return h, fmt.Errorf("vmware: the stream is compressed with algorithm %d, and only deflate (1) is defined",
			h.compressAlgorithm)
	}
	if h.grainSectors == 0 {
		return h, fmt.Errorf("vmware: the stream declares a grain size of zero")
	}
	grain := int64(h.grainSectors) * vmdkSector
	if grain%sectorSize != 0 {
		return h, fmt.Errorf("vmware: the stream's grains are %d bytes, which is not a whole number of %d-byte sectors. "+
			"The destination is written as a block device and cannot take a partial sector", grain, sectorSize)
	}
	if h.CapacityBytes()%sectorSize != 0 {
		return h, fmt.Errorf("vmware: the stream declares a capacity of %d bytes, which is not a whole number of %d-byte "+
			"sectors", h.CapacityBytes(), sectorSize)
	}
	return h, nil
}

// grainSink takes one decoded grain at its offset in the virtual disk.
type grainSink func(offset int64, data []byte) error

/*
decodeStreamVMDK reads a stream-optimised VMDK and hands over each grain.

	Returns the number of DISK bytes handed over — not the bytes read off the
	wire, which are compressed and mean nothing to an operator watching a copy.

	It reads to the end of the stream and insists on getting there. An io.EOF
	anywhere is an error, and so is an end-of-stream marker that arrives without
	a footer before it.
*/
func decodeStreamVMDK(r io.Reader, sink grainSink) (int64, error) {
	head := make([]byte, vmdkSector)
	if _, err := io.ReadFull(r, head); err != nil {
		return 0, fmt.Errorf("vmware: read the stream header: %w", err)
	}
	h, err := parseStreamHeader(head)
	if err != nil {
		return 0, err
	}

	/* Forward to the first marker. The header occupied sector 0, so the skip is
	   everything between it and where the data begins — descriptor AND the
	   padding after it, which is most of the distance and which the descriptor
	   fields alone do not describe. */
	if skip := (int64(h.dataStart()) - 1) * vmdkSector; skip > 0 {
		if _, err := io.CopyN(io.Discard, r, skip); err != nil {
			return 0, fmt.Errorf("vmware: skip to the first grain at sector %d: %w", h.dataStart(), err)
		}
	}

	capacity := h.CapacityBytes()
	grainBytes := int64(h.grainSectors) * vmdkSector
	var written int64
	var sawFooter bool

	marker := make([]byte, vmdkSector)
	for {
		if _, err := io.ReadFull(r, marker); err != nil {
			/* An EOF here is the truncation case, and it is the one that must
			   never pass for success: everything decoded so far is real data at
			   real offsets, so the disk looks fine and is missing its tail. */
			return written, fmt.Errorf("vmware: the stream ended after %d bytes of disk, before its end-of-stream marker. "+
				"The copy is incomplete and the destination must not be used: %w", written, err)
		}
		val := binary.LittleEndian.Uint64(marker[0:8])
		size := binary.LittleEndian.Uint32(marker[8:12])

		if size == 0 {
			kind := binary.LittleEndian.Uint32(marker[12:16])
			if kind == markerEOS {
				if !sawFooter {
					return written, fmt.Errorf("vmware: the stream ended without a footer after %d bytes of disk. "+
						"A footer is written last, so a stream without one was cut short and the destination is incomplete",
						written)
				}
				return written, nil
			}
			if kind == markerFooter {
				sawFooter = true
			}
			// Grain tables and directories are an index into a file that can be
			// seeked. Nothing here can seek, so they are skipped: the grains
			// themselves carry the offsets.
			if val > 0 {
				if _, err := io.CopyN(io.Discard, r, int64(val)*vmdkSector); err != nil {
					return written, fmt.Errorf("vmware: skip a metadata block of %d sectors: %w", val, err)
				}
			}
			continue
		}

		// A grain. Its data starts at byte 12 of this marker and the whole thing
		// is padded out to a sector boundary.
		total := align(int64(size)+12, vmdkSector)
		buf := make([]byte, total)
		copy(buf, marker)
		if total > vmdkSector {
			if _, err := io.ReadFull(r, buf[vmdkSector:]); err != nil {
				return written, fmt.Errorf("vmware: read a %d-byte grain for sector %d: %w", size, val, err)
			}
		}

		data, err := inflate(buf[12 : 12+int64(size)])
		if err != nil {
			return written, fmt.Errorf("vmware: decompress the grain at sector %d: %w", val, err)
		}
		if int64(len(data)) > grainBytes {
			return written, fmt.Errorf("vmware: the grain at sector %d decompressed to %d bytes, more than the %d-byte "+
				"grain size the stream declares", val, len(data), grainBytes)
		}

		offset := int64(val) * vmdkSector
		if offset < 0 || offset >= capacity {
			return written, fmt.Errorf("vmware: the stream places a grain at offset %d, outside the %d-byte disk it "+
				"declares", offset, capacity)
		}
		// The tail of a disk whose capacity is not a whole number of grains.
		// Capacity is a whole number of sectors, checked in the header, so this
		// still lands on a boundary the destination will take.
		if rem := capacity - offset; int64(len(data)) > rem {
			data = data[:rem]
		}
		if int64(len(data))%sectorSize != 0 {
			return written, fmt.Errorf("vmware: the grain at sector %d decompressed to %d bytes, which is not a whole "+
				"number of %d-byte sectors", val, len(data), sectorSize)
		}
		if err := sink(offset, data); err != nil {
			return written, err
		}
		written += int64(len(data))
	}
}

// inflate decompresses one grain. zlib rather than raw flate: the format wraps
// its deflate streams, and a raw reader on a zlib stream fails on the header
// bytes rather than on anything meaningful.
func inflate(b []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// align rounds n up to the next multiple of to.
func align(n, to int64) int64 {
	if r := n % to; r != 0 {
		return n + to - r
	}
	return n
}
