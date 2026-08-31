package hyperv

import (
	"context"
	"fmt"
	"os"
	"strings"
)

/* Filling a VHDX by writing to it as a block device.

   This replaced parsing the VHDX format by hand, and the reason is worth
   keeping: qemu-img's "fixed" VHDX is not fully allocated. Converting an empty
   64MB disk produced a 72MB file with NO payload region at all — every block
   marked PAYLOAD_BLOCK_ZERO — and a real VM disk is mostly free space, so most
   blocks would be like that. Writing a changed range at such an offset puts
   bytes in the file, the block allocation table still says ZERO, Hyper-V
   returns zeros and ignores them. Silent corruption, found only by testing
   against a real converted image rather than the synthetic one I had written
   myself.

   So Windows owns the format and Ballast only ever hands it bytes:

     New-VHD -Fixed        Windows allocates the whole disk. It is creating a
                           disk, not optimising a conversion, so there are no
                           sparse blocks to fall through.
     Mount-VHD             attached with no drive letter and left OFFLINE, so
                           nothing in Windows touches the filesystem inside it
                           — a mounted volume would have the host writing
                           metadata into a guest's disk while it is being
                           filled.
     \\.\PhysicalDriveN    ordinary seeks and writes at any offset.

   The base copy and every delta become the same operation. There is no format
   knowledge here at all, no third-party binary, and no GPL to distribute — the
   whole converter problem was created by framing this as "convert a file"
   rather than "fill a disk". A flat VMDK is already raw; the bytes map one to
   one.

   THE ONE CONSTRAINT: writes to a physical device must be sector-aligned. That
   is handled here rather than left to callers, because an unaligned write does
   not fail cleanly — it fails with a bare Win32 error at some depth, and the
   caller has no way to know why. */

// sectorSize is the alignment a raw device write must satisfy. 4096 covers both
// 512e and native 4K disks: a 4K-aligned write is always 512-aligned too, so
// using the larger is correct on either and avoids asking the device.
const sectorSize int64 = 4096

// RawDisk is a mounted VHDX, open for writing at arbitrary offsets.
type RawDisk struct {
	path      string // the .vhdx
	device    string // \\.\PhysicalDriveN
	f         *os.File
	sizeBytes int64
	ps        *PowerShell
}

/*
CreateAndMountVHDX makes a fixed VHDX of exactly sizeBytes and attaches it.

	sizeBytes is the SOURCE disk's size and is not rounded up for convenience: a
	VHDX larger than the disk it holds boots fine and then reports the wrong
	capacity to the guest for ever, which is the sort of thing nobody connects
	back to a migration months later. Hyper-V requires a multiple of 512, so a
	source that is not one is refused rather than quietly grown.
*/
func (p *PowerShell) CreateAndMountVHDX(ctx context.Context, path string, sizeBytes int64) (*RawDisk, error) {
	if sizeBytes <= 0 {
		return nil, fmt.Errorf("create %s: a disk size of %d makes no sense", path, sizeBytes)
	}
	if sizeBytes%512 != 0 {
		return nil, fmt.Errorf("create %s: the source disk is %d bytes, which is not a multiple of 512. "+
			"Hyper-V cannot represent that exactly, and rounding it would give the guest a disk that is not the size it had", path, sizeBytes)
	}

	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$path = %[1]s
$dir = Split-Path $path -Parent
if ($dir -and -not (Test-Path -LiteralPath $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }

# A half-written VHDX from an earlier attempt is removed rather than reused.
# Resuming into one would mean trusting bytes nobody can account for.
if (Test-Path -LiteralPath $path) {
  try { Dismount-VHD -Path $path -ErrorAction SilentlyContinue } catch {}
  Remove-Item -LiteralPath $path -Force
}

# FIXED, not dynamic. A dynamic VHDX allocates on demand, so the offsets a
# delta writes to would move under it; fixed is laid out once and stays put.
New-VHD -Path $path -SizeBytes %[2]d -Fixed | Out-Null

$d = Mount-VHD -Path $path -NoDriveLetter -Passthru | Get-Disk
# OFFLINE on purpose. Online, Windows would mount whatever volumes appear
# inside and start writing filesystem metadata into a guest's disk while it is
# still being filled.
if (-not $d.IsOffline) { Set-Disk -Number $d.Number -IsOffline $true }

[pscustomobject]@{ number = [int]$d.Number; size = [int64]$d.Size } | ConvertTo-Json -Compress
`, psQuote(path), sizeBytes)

	out, err := p.run(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("create and mount %s: %w", path, err)
	}
	var res struct {
		Number int   `json:"number"`
		Size   int64 `json:"size"`
	}
	if derr := decodeJSON(out, &res); derr != nil {
		return nil, fmt.Errorf("create and mount %s: %w", path, derr)
	}

	dev := fmt.Sprintf(`\\.\PhysicalDrive%d`, res.Number)
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		// Dismount rather than leave a disk attached to a host that cannot use
		// it: a stray mounted VHDX survives reboots and confuses everything
		// that enumerates disks, including Ballast's own inventory.
		_ = p.DismountVHDX(ctx, path)
		return nil, fmt.Errorf("open %s for writing: %w — the disk was created and has been dismounted again", dev, err)
	}
	return &RawDisk{path: path, device: dev, f: f, sizeBytes: sizeBytes, ps: p}, nil
}

/*
MountVHDX attaches a VHDX that already exists, for a later pass.

	Separate from CreateAndMountVHDX because the two must never be confused: that
	one DELETES what it finds, which is right for a base copy starting again and
	catastrophic for a delta pass onto a disk holding hours of copied data. A
	delta that silently started from an empty disk would finish, import, and
	produce a VM with an empty disk that nothing reported.
*/
func (p *PowerShell) MountVHDX(ctx context.Context, path string) (*RawDisk, error) {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$path = %[1]s
if (-not (Test-Path -LiteralPath $path)) { throw "the disk $path is not there any more" }

# Already attached from an interrupted pass is the normal case, not a failure:
# reuse the attachment rather than dismount and reattach, which would race with
# anything still holding it.
$v = Get-VHD -Path $path
if (-not $v.DiskNumber) {
  $v = Mount-VHD -Path $path -NoDriveLetter -Passthru
}
$d = Get-Disk -Number $v.DiskNumber
if (-not $d.IsOffline) { Set-Disk -Number $d.Number -IsOffline $true }

[pscustomobject]@{ number = [int]$d.Number; size = [int64]$v.Size } | ConvertTo-Json -Compress
`, psQuote(path))

	out, err := p.run(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("mount %s: %w", path, err)
	}
	var res struct {
		Number int   `json:"number"`
		Size   int64 `json:"size"`
	}
	if derr := decodeJSON(out, &res); derr != nil {
		return nil, fmt.Errorf("mount %s: %w", path, derr)
	}
	dev := fmt.Sprintf(`\\.\PhysicalDrive%d`, res.Number)
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		_ = p.DismountVHDX(ctx, path)
		return nil, fmt.Errorf("open %s for writing: %w — the disk has been dismounted again", dev, err)
	}
	return &RawDisk{path: path, device: dev, f: f, sizeBytes: res.Size, ps: p}, nil
}

/*
WriteAt writes guest bytes at a guest offset.

	Both the offset and the length must be sector-aligned, and that is checked
	rather than silently corrected. Rounding a caller's range outward would
	write bytes it did not ask to write — over data the previous pass had
	already put there — and rounding inward would drop the edges of every
	changed range. A changed-block list from VMware is sector-aligned already;
	one that is not means something upstream is wrong and should say so.
*/
func (d *RawDisk) WriteAt(b []byte, offset int64) error {
	if offset < 0 {
		return fmt.Errorf("write to %s: negative offset %d", d.path, offset)
	}
	if offset%sectorSize != 0 || int64(len(b))%sectorSize != 0 {
		return fmt.Errorf("write to %s: a %d-byte write at offset %d is not %d-aligned. "+
			"Raw device writes must be, and adjusting the range here would either write bytes nobody asked for or drop the edges of a changed range",
			d.path, len(b), offset, sectorSize)
	}
	if end := offset + int64(len(b)); end > d.sizeBytes {
		return fmt.Errorf("write to %s: a range ending at %d is past the %d bytes this disk holds. "+
			"The change list no longer matches this image, so the copy is stopping rather than writing outside it", d.path, end, d.sizeBytes)
	}
	_, err := d.f.WriteAt(b, offset)
	return err
}

// Size is the disk's capacity in bytes.
func (d *RawDisk) Size() int64 { return d.sizeBytes }

// Path is the VHDX on disk.
func (d *RawDisk) Path() string { return d.path }

/*
Close flushes and dismounts.

	Both, always, even when the flush fails. A VHDX left mounted outlives the
	job and the reboot, and a host with a stray attached disk confuses every
	tool that enumerates storage — including Ballast's own inventory, which
	would report it as a disk somebody could use.
*/
func (d *RawDisk) Close(ctx context.Context) error {
	if d == nil || d.f == nil {
		return nil
	}
	syncErr := d.f.Sync()
	closeErr := d.f.Close()
	d.f = nil
	dismountErr := d.ps.DismountVHDX(ctx, d.path)

	switch {
	case syncErr != nil:
		return fmt.Errorf("flush %s: %w (the disk has been dismounted)", d.path, syncErr)
	case closeErr != nil:
		return fmt.Errorf("close %s: %w (the disk has been dismounted)", d.path, closeErr)
	default:
		return dismountErr
	}
}

// DismountVHDX detaches a VHDX. Detaching one that is not attached is not an
// error — the caller is usually cleaning up after a failure and does not know
// how far the previous attempt got.
func (p *PowerShell) DismountVHDX(ctx context.Context, path string) error {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$path = %[1]s
if (-not (Test-Path -LiteralPath $path)) { 'RESULT=absent'; return }
try {
  Dismount-VHD -Path $path -ErrorAction Stop
  'RESULT=dismounted'
} catch {
  $m = [string]$_.Exception.Message
  # Not attached is the expected case on a cleanup path, not a failure.
  if ($m -match 'not currently attached|could not be found|is not mounted') { 'RESULT=notattached'; return }
  throw
}
`, psQuote(path))
	if _, err := p.run(ctx, script); err != nil {
		return fmt.Errorf("dismount %s: %w", path, err)
	}
	return nil
}

/*
AlignedRange widens a changed range out to sector boundaries.

	VMware reports changed areas in bytes and does not promise alignment.
	Widening is safe in a way that narrowing is not: the extra bytes at each
	edge are read from the same source and written to the same place, so the
	destination ends up with the source's content either way. Narrowing would
	leave the edges stale. Returned as start and length so the caller reads
	exactly what it will write.
*/
func AlignedRange(offset, length int64) (int64, int64) {
	if length <= 0 {
		return offset, 0
	}
	start := offset - (offset % sectorSize)
	end := offset + length
	if r := end % sectorSize; r != 0 {
		end += sectorSize - r
	}
	return start, end - start
}

// psQuoteList renders a []string for PowerShell, used by the migration scripts.
func psQuoteList(xs []string) string {
	if len(xs) == 0 {
		return "@()"
	}
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		parts = append(parts, psQuote(x))
	}
	return "@(" + strings.Join(parts, ",") + ")"
}
