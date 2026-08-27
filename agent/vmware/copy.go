package vmware

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/joshua-fourie/ballast/agent/hyperv"
)

/* Moving the bytes.

   One pass — base or delta, they are the same operation — is:

     1. snapshot the VM, so its disks stop changing under the read
     2. ask what to copy: everything allocated for a base pass, only what has
        changed since the last marker for a delta
     3. read each range over the datastore HTTPS endpoint and write it into the
        mounted VHDX at the same offset
     4. remove the snapshot and WAIT for it to consolidate, so the next pass
        reads a base disk that is complete

   Step 4 is what lets this work without VMware's VDDK. Reading the flat file
   directly gives a view as of the last consolidation, so consolidating each
   pass keeps that view current. The cost is real — consolidation is I/O on
   somebody else's datastore — and it is why passes are not run back to back. */

// Sink is where bytes go. Implemented by hyperv.RawDisk; an interface so the
// copy can be tested without a Hyper-V host, which is most of what is worth
// testing here.
type Sink interface {
	WriteAt(b []byte, offset int64) error
	Size() int64
}

// readChunk bounds one HTTP range request. VMware returns changed areas that
// can be gigabytes long, and asking for one in a single request means a
// connection that must survive the whole transfer and a failure that loses all
// of it. 32MB is large enough that the per-request overhead disappears and
// small enough that a retry is cheap.
const readChunk int64 = 32 << 20

// PassResult is what one pass did.
type PassResult struct {
	// BytesCopied is what actually moved.
	BytesCopied int64
	// NextChangeID is the marker the following pass must use. Stored even when
	// nothing was copied: a pass that found no changes still advances the
	// window, and reusing the old marker would re-copy it for ever.
	NextChangeID string
	// Extents is how many ranges were transferred, which is the number that
	// says whether a guest is writing scattered or sequentially.
	Extents int
}

// Progress reports how far a pass has got, for the job's live message.
type Progress func(copied, total int64)

/* CopyDisk runs one pass for one disk.

   snapRef must be a snapshot that already exists — taken once for the whole VM
   rather than once per disk, so every disk in a multi-disk VM is captured at
   the same instant. A VM whose disks were snapshotted seconds apart can arrive
   with a database and its log out of step, which is the kind of corruption
   that surfaces days later. */
/* Source is the half of Client a copy uses: what to copy and how to read it.

   Named so the copy loop can be tested against something that is not a
   vCenter. The arithmetic here — chunking, alignment, offsets — is exactly the
   part that corrupts a disk quietly when it is wrong, so it must be testable
   without a lab. */
type Source interface {
	ChangedAreas(ctx context.Context, moRef, snapRef string, deviceKey int32, changeID string, diskSize int64) ([]Extent, string, error)
	ReadAt(ctx context.Context, dsPath string, offset, length int64) (io.ReadCloser, error)
}

// CopyDisk runs one pass against this client.
func (v *Client) CopyDisk(ctx context.Context, moRef, snapRef string, disk Disk, changeID string, dst Sink, onProgress Progress) (PassResult, error) {
	return CopyDisk(ctx, v, moRef, snapRef, disk, changeID, dst, onProgress)
}

func CopyDisk(ctx context.Context, src Source, moRef, snapRef string, disk Disk, changeID string, dst Sink, onProgress Progress) (PassResult, error) {
	var res PassResult

	// Checked here, before a single byte moves. The last extent of a disk whose
	// size is not a whole number of sectors cannot be written aligned, and
	// finding that out at the end of a 400GB copy is the worst possible moment
	// — everything looks well for hours and then fails with nothing usable.
	if disk.SizeBytes%sectorSize != 0 {
		return res, fmt.Errorf("%s is %d bytes, which is not a multiple of %d. The destination is written as a raw "+
			"device and its last sector could not be completed, so this disk cannot be copied", disk.Label, disk.SizeBytes, sectorSize)
	}
	if disk.SizeBytes > dst.Size() {
		return res, fmt.Errorf("%s is %d bytes at the source and the destination disk holds %d. "+
			"The source disk has grown since this migration started; it has to be restarted so the destination matches",
			disk.Label, disk.SizeBytes, dst.Size())
	}

	extents, next, err := src.ChangedAreas(ctx, moRef, snapRef, disk.Key, changeID, disk.SizeBytes)
	if err != nil {
		return res, err
	}
	res.NextChangeID = next
	res.Extents = len(extents)

	var total int64
	for _, e := range extents {
		total += e.Length
	}

	for _, e := range extents {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		// Widened to sector boundaries: the destination is a block device and
		// will refuse anything else. Widening reads a few extra bytes from the
		// same source and writes them to the same place, so the result is
		// identical; narrowing would leave the edges of every range stale.
		start, length := hyperv.AlignedRange(e.Start, e.Length)
		for off := start; off < start+length; off += readChunk {
			n := readChunk
			if rem := start + length - off; rem < n {
				n = rem
			}
			if err := copyRange(ctx, src, disk.Path, off, n, dst); err != nil {
				return res, err
			}
			res.BytesCopied += n
			if onProgress != nil {
				onProgress(res.BytesCopied, total)
			}
		}
	}
	return res, nil
}

func copyRange(ctx context.Context, src Source, dsPath string, offset, length int64, dst Sink) error {
	rc, err := src.ReadAt(ctx, dsPath, offset, length)
	if err != nil {
		return err
	}
	defer rc.Close()

	buf := make([]byte, length)
	if _, err := io.ReadFull(rc, buf); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			// Short of what was asked for means the source file is not the size
			// the configuration claims. Stopping is right: writing the partial
			// read would leave the rest of the range holding whatever was there
			// before, silently.
			return fmt.Errorf("read %s at %d: the datastore returned less than the %d bytes requested, "+
				"so the disk file is not the size its configuration reports", dsPath, offset, length)
		}
		return fmt.Errorf("read %s at %d: %w", dsPath, offset, err)
	}
	return dst.WriteAt(buf, offset)
}

// sectorSize mirrors the alignment the destination enforces. Named here so the
// refusal above can explain itself; the arithmetic itself is hyperv's, so there
// is one implementation of it rather than two that can drift apart.
const sectorSize int64 = 4096

/* SnapshotName is what Ballast calls its snapshots.

   Recognisable on sight in vSphere Client, and carrying the migration's own
   identity so two migrations of different VMs — or a retry after a failure —
   never look like each other's leftovers. Somebody investigating an unexpected
   snapshot should be able to tell what made it and whether it is still needed. */
func SnapshotName(migration string) string {
	return "ballast-migration-" + migration
}
