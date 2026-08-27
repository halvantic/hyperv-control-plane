package hyperv

import (
	"strings"
	"testing"
)

/* Filling a VHDX by writing to it as a block device.

   These cover the parts that do not need a Hyper-V host: the script that
   creates and mounts the disk, the refusals that stop a bad write, and the
   alignment arithmetic. The mount itself is proven on the rig, not here.

   Worth remembering why this replaced a VHDX parser. qemu-img's "fixed" VHDX is
   not fully allocated — an empty 64MB disk converted to a 72MB file with no
   payload region at all, every block PAYLOAD_BLOCK_ZERO. A real VM disk is
   mostly free space, so most blocks would be like that, and writing a changed
   range at such an offset lands bytes in the file that Hyper-V then ignores.
   Silent corruption, found only by parsing a real converted image instead of
   the synthetic one I had written myself. */

func TestTheDiskIsCreatedFixedAndLeftOffline(t *testing.T) {
	p := newTestPS(&fakeRunner{})
	// The script is what is asserted; the call needs a host, so the builder is
	// exercised through the same path with a runner that answers nothing.
	s := vhdxCreateScriptFor(t, p, `C:\ClusterStorage\DS1\Web01\Web01.vhdx`, 64<<20)

	// FIXED. A dynamic VHDX allocates on demand, so the offsets a delta writes
	// to would move under it.
	if !strings.Contains(s, "-Fixed") {
		t.Error("the disk is not created fixed, so delta offsets would not stay put")
	}
	// OFFLINE. Online, Windows mounts whatever volumes appear inside and writes
	// filesystem metadata into a guest's disk while it is being filled.
	if !strings.Contains(s, "Set-Disk -Number $d.Number -IsOffline $true") {
		t.Error("the disk is not forced offline")
	}
	if !strings.Contains(s, "-NoDriveLetter") {
		t.Error("the disk is mounted with a drive letter")
	}
	// A half-written disk from an earlier attempt is removed, not resumed into:
	// resuming means trusting bytes nobody can account for.
	if !strings.Contains(s, "Remove-Item -LiteralPath $path -Force") {
		t.Error("an existing VHDX is reused rather than replaced")
	}
}

/* A VHDX larger than the disk it holds boots fine and reports the wrong
   capacity to the guest for ever — the sort of thing nobody connects back to a
   migration months later. */
func TestASizeHyperVCannotRepresentIsRefused(t *testing.T) {
	p := newTestPS(&fakeRunner{})
	_, err := p.CreateAndMountVHDX(t.Context(), `C:\x.vhdx`, 1000)
	if err == nil {
		t.Fatal("a size that is not a multiple of 512 was accepted")
	}
	if !strings.Contains(err.Error(), "not the size it had") {
		t.Errorf("the refusal does not say what rounding would cost: %v", err)
	}
	if _, err := p.CreateAndMountVHDX(t.Context(), `C:\x.vhdx`, 0); err == nil {
		t.Error("a zero-byte disk was accepted")
	}
}

/* Widening a changed range is safe; narrowing is not. The extra bytes at each
   edge come from the same source and go to the same place, so the destination
   matches either way — but dropping the edges leaves them stale for ever. */
func TestAChangedRangeIsWidenedToSectors(t *testing.T) {
	tests := []struct{ off, len, wantOff, wantLen int64 }{
		// Already aligned: unchanged.
		{0, 4096, 0, 4096},
		{8192, 8192, 8192, 8192},
		// Ragged start pulls back to the sector below.
		{100, 4096, 0, 8192},
		// Ragged end pushes out to the sector above.
		{0, 100, 0, 4096},
		// Both ends ragged.
		{5000, 100, 4096, 4096},
		// A range spanning several sectors keeps all of them.
		{4095, 4098, 0, 12288},
	}
	for _, tt := range tests {
		off, l := alignedRange(tt.off, tt.len)
		if off != tt.wantOff || l != tt.wantLen {
			t.Errorf("alignedRange(%d,%d) = (%d,%d), want (%d,%d)", tt.off, tt.len, off, l, tt.wantOff, tt.wantLen)
		}
		// The widened range must always cover the original, or the copy is
		// silently dropping changed bytes.
		if off > tt.off || off+l < tt.off+tt.len {
			t.Errorf("alignedRange(%d,%d) = (%d,%d) does not cover the original range", tt.off, tt.len, off, l)
		}
		if off%sectorSize != 0 || l%sectorSize != 0 {
			t.Errorf("alignedRange(%d,%d) = (%d,%d) is not aligned", tt.off, tt.len, off, l)
		}
	}
}

func TestAnEmptyRangeStaysEmpty(t *testing.T) {
	if off, l := alignedRange(4096, 0); off != 4096 || l != 0 {
		t.Errorf("an empty range became (%d,%d)", off, l)
	}
}

/* Unaligned writes are refused rather than corrected. Rounding outward writes
   bytes the caller did not ask to write, over data an earlier pass put there;
   rounding inward drops the edges of every changed range. */
func TestAnUnalignedWriteIsRefusedWithTheReason(t *testing.T) {
	d := &RawDisk{path: `C:\x.vhdx`, sizeBytes: 1 << 30}
	err := d.WriteAt(make([]byte, 100), 0)
	if err == nil {
		t.Fatal("an unaligned length was accepted")
	}
	if !strings.Contains(err.Error(), "write bytes nobody asked for") {
		t.Errorf("the refusal does not explain why it will not adjust: %v", err)
	}
	if err := d.WriteAt(make([]byte, 4096), 100); err == nil {
		t.Error("an unaligned offset was accepted")
	}
}

/* A range past the end means the change list no longer describes this image.
   Growing the disk to fit would produce a VHDX that looks right and is not. */
func TestAWritePastTheEndStopsTheCopy(t *testing.T) {
	d := &RawDisk{path: `C:\x.vhdx`, sizeBytes: 8192}
	err := d.WriteAt(make([]byte, 4096), 8192)
	if err == nil {
		t.Fatal("a write past the end was accepted")
	}
	if !strings.Contains(err.Error(), "no longer matches this image") {
		t.Errorf("the refusal does not say what it implies: %v", err)
	}
	if err := d.WriteAt(make([]byte, 4096), -4096); err == nil {
		t.Error("a negative offset was accepted")
	}
}

/* Dismounting something that is not attached is the normal case on a cleanup
   path — the caller is tidying after a failure and does not know how far the
   previous attempt got. It must not read as an error. */
func TestDismountToleratesADiskThatIsNotAttached(t *testing.T) {
	p := newTestPS(&fakeRunner{})
	s := dismountScriptFor(t, p, `C:\x.vhdx`)
	if !strings.Contains(s, "not currently attached|could not be found|is not mounted") {
		t.Error("dismount treats an unattached disk as a failure")
	}
	if !strings.Contains(s, "RESULT=notattached") {
		t.Error("the unattached case is not reported distinctly")
	}
}

/* fakeRunner already records every script it is handed, so the builders are
   captured by driving the method against one rather than by adding a second
   recorder that does the same thing. */
func vhdxCreateScriptFor(t *testing.T, _ *PowerShell, path string, size int64) string {
	t.Helper()
	r := &fakeRunner{}
	_, _ = newTestPS(r).CreateAndMountVHDX(t.Context(), path, size)
	if len(r.calls) == 0 {
		t.Fatal("no script was run")
	}
	return r.calls[0]
}

func dismountScriptFor(t *testing.T, _ *PowerShell, path string) string {
	t.Helper()
	r := &fakeRunner{}
	_ = newTestPS(r).DismountVHDX(t.Context(), path)
	if len(r.calls) == 0 {
		t.Fatal("no script was run")
	}
	return r.calls[0]
}
