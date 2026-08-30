package vmware

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/joshua-fourie/ballast/agent/hyperv"
)

/* One pass of a migration, end to end.

   Everything above this is a piece: connect, snapshot, ask what changed, read a
   range, write a range. This is the order they go in, and the order is where
   the correctness lives — a snapshot taken after the read, or removed before
   the write finishes, produces a disk that is plausible and wrong.

   The same function runs the base copy, every delta, and the final pass after
   the source is powered off. They differ by two things: whether a change marker
   was carried in, and whether the source is stopped first. Making them one code
   path is deliberate — a separate "final pass" that had drifted from the delta
   path would be the one nobody had run a hundred times. */

// Provisioner opens destination disks. Implemented by *hyperv.PowerShell; an
// interface so a pass can be driven without a Hyper-V host.
type Provisioner interface {
	// Create makes a new fixed disk of exactly size bytes, replacing anything
	// already at path.
	Create(ctx context.Context, path string, size int64) (Disk2, error)
	// Open attaches a disk that already exists. It must never create or
	// truncate: a delta pass that silently started from an empty disk would
	// finish, import, and produce a VM with an empty disk that nothing reported.
	Open(ctx context.Context, path string) (Disk2, error)
}

// Disk2 is a destination disk open for writing. Named for what it is rather
// than shadowing the source-side Disk.
type Disk2 interface {
	Sink
	Close(ctx context.Context) error
}

// PassRequest is one pass. Credentials ride on it rather than living on the
// host, for the reason CHAP secrets do: a credential cached on a host outlives
// the operator's intent for it.
type PassRequest struct {
	Endpoint
	// Migration names the migration, and so the snapshot.
	Migration string
	// MoRef is VMware's identity for the VM. Never the name: a name is a label
	// somebody can change under us and two VMs may share one.
	MoRef string
	// DestDir is where the VHDXs live, e.g. a CSV mount.
	DestDir string
	// Marker carries the previous pass's change ID per disk key. Empty for a
	// disk means a base copy of that disk.
	Marker map[int32]string
	// Final powers the source off before reading, so the pass reads a guest
	// that can no longer change. The whole point of the cutover.
	Final bool
	// GracefulOff is how long the guest is given to shut down before the power
	// is pulled. Zero uses the default.
	GracefulOff time.Duration
}

// DiskOutcome is what one disk did.
type DiskOutcome struct {
	Key         int32
	Label       string
	SourcePath  string
	DestPath    string
	SizeBytes   int64
	CopiedBytes int64
	// NextChangeID is the marker for the following pass. Stored even when
	// nothing was copied.
	NextChangeID string
	// FullRead records that this disk was read end to end because the source
	// could not answer a change-tracking query.
	FullRead bool
}

// PassOutcome is what the pass did, in the shape the centre stores.
type PassOutcome struct {
	Disks []DiskOutcome
	// SourcePoweredOff records that this pass stopped the guest, which is the
	// moment a production workload stopped and belongs in the record.
	SourcePoweredOff bool
	// CopiedBytes is the total across disks, for the one-line job message.
	CopiedBytes int64
	// FullRead records that at least one disk had to be read end to end for the
	// want of change tracking. Said out loud rather than left to the byte count,
	// because it is the difference between a base copy that will be followed by
	// small deltas and one that cannot be.
	FullRead bool
}

// defaultGracefulOff is how long a guest gets to shut down before the power is
// pulled. Five minutes: long enough for a Windows guest with updates pending,
// short enough that a cutover window is not spent watching a splash screen.
const defaultGracefulOff = 5 * time.Minute

/* RunPass performs one pass and always cleans up after itself.

   The snapshot is removed on EVERY exit, including a failed one. A snapshot
   left behind on somebody else's VM grows until their datastore fills, and
   filling a datastore Ballast does not own is the worst thing this feature
   could do to a system it is only visiting. */
/* PassProgress is what a pass reports while it is still running.

   Note is one line for the job message, which is prose for a person. Disks is
   the same moment expressed as numbers, because a progress meter cannot be
   drawn from a sentence — and parsing one back into bytes is exactly the habit
   this codebase refuses elsewhere. */
type PassProgress struct {
	Note string
	// Disks is every disk in the pass, with SizeBytes always set and CopiedBytes
	// filling in as it goes. Disks not started yet are present at zero, so the
	// total is the whole job from the first report rather than growing as each
	// disk begins — a denominator that moves makes a meter run backwards.
	Disks []DiskOutcome
}

func RunPass(ctx context.Context, prov Provisioner, req PassRequest, onProgress func(PassProgress)) (PassOutcome, error) {
	c, err := Connect(ctx, req.Endpoint)
	if err != nil {
		return PassOutcome{}, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	return runPass(ctx, c, prov, req, onProgress)
}

/* passSource is the half of Client a pass drives.

   The ORDER of these calls is where the correctness of a pass lives — off
   before snapshot, snapshot removed on every exit including a failed one — and
   an order can only be tested by watching the calls. Against a real vCenter
   those are the two things hardest to observe and most expensive to get wrong. */
type passSource interface {
	Source
	Inspect(ctx context.Context, moRef string) (VMInfo, error)
	ResolveDiskFile(ctx context.Context, dsPath string, capacity int64) (string, error)
	Snapshot(ctx context.Context, moRef, name string) (string, error)
	RemoveSnapshot(ctx context.Context, moRef, snapRef string) error
	PowerOff(ctx context.Context, moRef string, graceful time.Duration) error
}

func runPass(ctx context.Context, c passSource, prov Provisioner, req PassRequest, onProgress func(PassProgress)) (PassOutcome, error) {
	var out PassOutcome

	// live is every disk in this pass, carried across the whole run so a report
	// always describes the complete job rather than the disk in hand.
	var live []DiskOutcome
	report := func(p PassProgress) {
		if onProgress != nil {
			p.Disks = append([]DiskOutcome(nil), live...)
			onProgress(p)
		}
	}
	note := func(msg string) { report(PassProgress{Note: msg}) }

	/* The power off comes BEFORE the snapshot, not after.

	   A snapshot taken of a running guest and then powered off leaves whatever
	   the guest wrote between the two in neither place: not in the snapshot the
	   pass reads, and not in a later pass, because there is no later pass. Off
	   first means the disks are already still when they are captured. */
	if req.Final {
		note("shutting the source VM down")
		if err := c.PowerOff(ctx, req.MoRef, gracefulOff(req.GracefulOff)); err != nil {
			return out, err
		}
		out.SourcePoweredOff = true
	}

	info, err := c.Inspect(ctx, req.MoRef)
	if err != nil {
		return out, err
	}
	if len(info.Disks) == 0 {
		return out, fmt.Errorf("%s has no disks to copy. A VM with none is usually a vCLS agent or a template, "+
			"neither of which is a workload that can be migrated", info.Name)
	}

	/* A disk already on a snapshot chain is refused HERE, before change tracking,
	   before a snapshot of our own, and before anything is created at the
	   destination.

	   A pass reads one file per disk over the datastore interface. A snapshot
	   splits a disk into a chain — a frozen base plus a delta holding only what
	   has changed — and nothing here can compose one. What that produced instead
	   was a probe of four candidate filenames and a paragraph of HTTP status
	   codes: true, and no use to anybody. The remedy is one step in vCenter, and
	   it belongs on screen instead. */
	var chained []string
	for _, d := range info.Disks {
		if d.Snapshotted {
			chained = append(chained, d.Label)
		}
	}
	if len(chained) > 0 {
		is := "is"
		if len(chained) > 1 {
			is = "are"
		}
		return out, fmt.Errorf("%s is running on a snapshot: %s %s a delta file rather than the disk itself, and a copy that "+
			"reads one file per disk cannot put a snapshot chain back together. Delete the snapshots in vSphere Client "+
			"(Snapshots → Delete All) so the disks consolidate back to one file each, then retry this migration. Ballast will not "+
			"delete a snapshot it did not take — the data in it is not ours to discard",
			info.Name, strings.Join(chained, ", "), is)
	}

	/* Resolve where each disk's bytes actually live, BEFORE the snapshot and
	   before a single byte is provisioned at the destination.

	   VMware's backing names the descriptor, which on VMFS is a few hundred
	   bytes of text beside the real data. Finding that out at the first read —
	   after a snapshot has been taken on somebody else's VM and a VHDX created
	   here — is a failure that has already cost something. Resolving it here
	   costs one call per disk and fails before anything has been changed. */
	for i := range info.Disks {
		d := &info.Disks[i]
		data, rerr := c.ResolveDiskFile(ctx, d.Path, d.SizeBytes)
		if rerr != nil {
			return out, rerr
		}
		d.DataPath = data
	}

	note("taking a snapshot on the source")
	snapRef, err := c.Snapshot(ctx, req.MoRef, SnapshotName(req.Migration))
	if err != nil {
		return out, err
	}
	defer func() {
		// Its own context: the job's may already be cancelled or timed out, and
		// that is exactly when leaving a snapshot behind matters most.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), consolidateWait+time.Minute)
		defer cancel()
		if rerr := c.RemoveSnapshot(cctx, req.MoRef, snapRef); rerr != nil {
			note("the snapshot could not be removed: " + rerr.Error())
		}
	}()

	// Seeded before the first copy so the total is the whole pass from the first
	// report. A denominator that grows as each disk starts makes a meter run
	// backwards, which reads as a copy losing ground.
	live = make([]DiskOutcome, len(info.Disks))
	for i, d := range info.Disks {
		live[i] = DiskOutcome{Key: d.Key, Label: d.Label, SourcePath: d.Path, SizeBytes: d.SizeBytes}
	}

	for i, d := range info.Disks {
		marker := req.Marker[d.Key]
		dest := DestPathFor(req.DestDir, info.Name, d)
		live[i].DestPath = dest

		var dst Disk2
		var oerr error
		if marker == "" {
			note(fmt.Sprintf("creating %s for %s", filepath.Base(dest), d.Label))
			dst, oerr = prov.Create(ctx, dest, d.SizeBytes)
		} else {
			dst, oerr = prov.Open(ctx, dest)
		}
		if oerr != nil {
			return out, oerr
		}

		res, cerr := CopyDisk(ctx, c, req.MoRef, snapRef, d, marker, dst, func(copied, total int64) {
			// The bytes as numbers AND as a sentence. The sentence is the job
			// message an operator reads; the numbers are what the meter needs.
			live[i].CopiedBytes = copied
			report(PassProgress{Note: fmt.Sprintf("%s: %s of %s", d.Label, fmtBytes(copied), fmtBytes(total))})
		})
		// Closed before the error is returned, and its failure reported if the
		// copy itself did not fail: a dismount that did not happen leaves a disk
		// attached to the host across reboots, and the next pass then finds it
		// in a state it did not expect.
		if xerr := dst.Close(context.WithoutCancel(ctx)); xerr != nil && cerr == nil {
			cerr = xerr
		}
		if cerr != nil {
			if errors.Is(cerr, ErrCBTReset) {
				// Recoverable, and worth saying so in the same breath: the next
				// pass drops the marker and reads in full, which costs time and
				// nothing else.
				return out, fmt.Errorf("%s: %w. The next pass will read this disk in full", d.Label, cerr)
			}
			return out, cerr
		}

		out.Disks = append(out.Disks, DiskOutcome{
			Key:          d.Key,
			Label:        d.Label,
			SourcePath:   d.Path,
			DestPath:     dest,
			SizeBytes:    d.SizeBytes,
			CopiedBytes:  res.BytesCopied,
			NextChangeID: res.NextChangeID,
			FullRead:     res.FullRead,
		})
		if res.FullRead {
			out.FullRead = true
		}
		out.CopiedBytes += res.BytesCopied
		live[i].CopiedBytes = res.BytesCopied
		live[i].NextChangeID = res.NextChangeID
	}
	return out, nil
}

func gracefulOff(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultGracefulOff
	}
	return d
}

/* DestPathFor names one disk's VHDX.

   Derived from the SOURCE disk's file name, not from a counter. A VM with disks
   added and removed over the years has keys 2000, 2002 and 2005, and numbering
   the destinations 1, 2, 3 loses the only thing that lets an operator match a
   VHDX back to the VMDK it came from a year later. */
func DestPathFor(dir, vmName string, d Disk) string {
	base := d.Path
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if base == "" {
		base = fmt.Sprintf("disk-%d", d.Key)
	}
	return filepath.Join(dir, sanitise(vmName), sanitise(base)+".vhdx")
}

// sanitise strips what Windows will not accept in a path. VMware names allow
// characters NTFS does not, and a VM called "web/prod" would otherwise create a
// directory nobody asked for.
func sanitise(s string) string {
	s = strings.TrimSpace(s)
	repl := func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return '-'
		}
		if r < 32 {
			return '-'
		}
		return r
	}
	s = strings.Map(repl, s)
	// Trailing dots and spaces are legal to write and impossible to open again.
	s = strings.TrimRight(s, ". ")
	if s == "" {
		return "vm"
	}
	return s
}

// fmtBytes renders a size the way the console does, so a progress note and the
// table agree.
func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

/* HostDisks is the real destination: VHDXs on this Hyper-V host.

   Thin on purpose. Create and Open differ by ONE thing — Create replaces what it
   finds and Open must not — and keeping them two named methods rather than one
   with a flag means a caller cannot get that wrong by passing false. */
type HostDisks struct {
	PS *hyperv.PowerShell
}

func (h HostDisks) Create(ctx context.Context, path string, size int64) (Disk2, error) {
	return h.PS.CreateAndMountVHDX(ctx, path, size)
}

func (h HostDisks) Open(ctx context.Context, path string) (Disk2, error) {
	return h.PS.MountVHDX(ctx, path)
}
