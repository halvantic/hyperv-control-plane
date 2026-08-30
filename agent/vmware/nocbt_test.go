package vmware

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

/* A base copy of a VM that has no change tracking.

   This failed a real migration. BSL on vcsa-02 was migrated cold — correctly,
   the console had already worked out it could not go warm — and the copy asked
   VMware a change-tracking question anyway. What came back was a bare FileFault
   naming the VMDK, which reached the operator as

     ServerFaultCode: Error caused by file /vmfs/volumes/…/BSL/BSL.vmdk

   and reads as a damaged disk. The disk was fine. Change tracking was simply
   off, which for a base copy does not matter at all: "*" is an optimisation
   that reads only the allocated part of a thin disk, not a requirement. */

func TestAFileFaultIsRecognisedAsChangeTrackingBeingOff(t *testing.T) {
	// VMware's own wording, with nothing in it about change tracking.
	err := explainCBTError(
		fmt.Errorf("ServerFaultCode: Error caused by file /vmfs/volumes/69c06b26-a7dce8a6/BSL/BSL.vmdk"), "*")

	if !errors.Is(err, ErrCBTUnavailable) {
		t.Fatalf("a FileFault on the VMDK was not recognised as change tracking being off: %v", err)
	}
	// The operator gets the cause and the remedy, not just the fault.
	if !strings.Contains(err.Error(), "takes effect at power-on") {
		t.Errorf("the explanation does not say what would fix it: %v", err)
	}
	// The original is kept: which file faulted is the one identifying detail.
	if !strings.Contains(err.Error(), "BSL.vmdk") {
		t.Errorf("VMware's own words were dropped: %v", err)
	}
}

/* A reset must not be swallowed by the new case. Both mention a disk, both are
   recovered by reading in full, and only one of them means the marker is dead —
   classified the wrong way round, a reset would be reported as a VM that never
   had tracking and the operator would go and enable something already on. */
func TestAResetIsStillClassifiedAsAResetAndNotAsMissingTracking(t *testing.T) {
	err := explainCBTError(fmt.Errorf("ServerFaultCode: InvalidChangeId: the change id is invalid"), "52 de/7")
	if !errors.Is(err, ErrCBTReset) {
		t.Fatalf("a reset was not classified as one: %v", err)
	}
	if errors.Is(err, ErrCBTUnavailable) {
		t.Error("a reset was also reported as tracking being unavailable, which sends the migration to enable something already enabled")
	}
}

// The fix: a base pass reads the whole disk rather than refusing.
func TestABaseCopyWithoutChangeTrackingReadsTheWholeDisk(t *testing.T) {
	const size = 1 << 20
	src := &fakeSource{
		image: pattern(size),
		err:   explainCBTError(fmt.Errorf("ServerFaultCode: Error caused by file [ds1] BSL/BSL.vmdk"), "*"),
	}
	dst := &fakeSink{buf: make([]byte, size)}

	res, err := CopyDisk(t.Context(), src, "vm-2013", "snap-1",
		Disk{Key: 2000, Label: "Hard disk 1", Path: "[ds1] BSL/BSL.vmdk", SizeBytes: size}, "", dst, nil)
	if err != nil {
		t.Fatalf("a base copy still refuses a VM with no change tracking: %v", err)
	}
	if res.BytesCopied != size {
		t.Errorf("copied %d bytes of a %d-byte disk", res.BytesCopied, size)
	}
	if !res.FullRead {
		t.Error("the full read was not reported, so nothing downstream can say why the copy moved the whole volume")
	}
	// There is no marker, because nothing tracked one. Inventing one would send
	// the next pass to a point that does not exist.
	if res.NextChangeID != "" {
		t.Errorf("a change marker was invented: %q", res.NextChangeID)
	}
	if !bytes.Equal(dst.buf, src.image) {
		t.Error("the destination does not match the source after a full read")
	}
}

/* A DELTA is a different question. Without a marker there is no "since", and
   reading in full there would be a silent full re-copy reported as a delta —
   hours of transfer while the console says the outstanding change is small. */
func TestADeltaWithoutChangeTrackingStillFails(t *testing.T) {
	const size = 1 << 20
	src := &fakeSource{
		image: pattern(size),
		err:   explainCBTError(fmt.Errorf("ServerFaultCode: Error caused by file [ds1] BSL/BSL.vmdk"), "52 de/7"),
	}
	dst := &fakeSink{buf: make([]byte, size)}

	_, err := CopyDisk(t.Context(), src, "vm-2013", "snap-1",
		Disk{Key: 2000, Label: "Hard disk 1", SizeBytes: size}, "52 de/7", dst, nil)
	if err == nil {
		t.Fatal("a delta pass with no change tracking silently read the disk in full")
	}
	if !errors.Is(err, ErrCBTUnavailable) {
		t.Errorf("the delta failed for a reason that does not name the cause: %v", err)
	}
}

/* The fallback is only for change tracking. A genuine failure — no route, a
   rejected session, a snapshot that has gone — must still fail rather than
   turning into a full read of a disk nothing can reach. */
func TestAnUnrelatedFailureIsNotTreatedAsMissingTracking(t *testing.T) {
	src := &fakeSource{image: pattern(1 << 20), err: fmt.Errorf("Post \"https://vcsa-02/sdk\": dial tcp: i/o timeout")}
	dst := &fakeSink{buf: make([]byte, 1<<20)}

	if _, err := CopyDisk(t.Context(), src, "vm", "snap",
		Disk{Label: "Hard disk 1", SizeBytes: 1 << 20}, "", dst, nil); err == nil {
		t.Fatal("a base copy read the whole disk after a connection failure")
	}
}

/* Reading the descriptor instead of the disk.

   The second failure on the rig, after change tracking was sorted out:

     read [datastore1] BSL/BSL.vmdk at 0: the datastore returned less than the
     33554432 bytes requested, so the disk file is not the size its
     configuration reports

   VMware's backing names BSL.vmdk, which on VMFS is a few hundred bytes of text
   beside BSL-flat.vmdk. Nothing was wrong with the disk. */

func TestThePassReadsTheDataFileAndNotTheDescriptor(t *testing.T) {
	src, prov := onePass()
	if _, err := runPass(t.Context(), src, prov, PassRequest{
		MoRef: "vm-2013", Migration: "mig-bsl", DestDir: `C:\CSV1`,
	}, nil); err != nil {
		t.Fatalf("pass failed: %v", err)
	}
	// Every read must come from the resolved data file, not the descriptor.
	for _, r := range src.readPaths {
		if !strings.HasSuffix(r, "-flat.vmdk") {
			t.Errorf("a read came from %q, which is the descriptor rather than the disk", r)
		}
	}
	if len(src.readPaths) == 0 {
		t.Fatal("nothing was read at all, so this proves nothing")
	}
}

/* Resolved BEFORE the snapshot and before anything is provisioned.

   Finding out at the first read means a snapshot has already been taken on
   somebody else's VM and a VHDX created here — a failure that has already cost
   something and left something behind. */
func TestTheDataFileIsResolvedBeforeAnythingIsChanged(t *testing.T) {
	src, prov := onePass()
	src.failResolve = fmt.Errorf("no file beside it is large enough")

	if _, err := runPass(t.Context(), src, prov, PassRequest{
		MoRef: "vm-2013", Migration: "mig-bsl", DestDir: `C:\CSV1`,
	}, nil); err == nil {
		t.Fatal("a disk whose data file could not be found was copied anyway")
	}
	for _, c := range src.calls {
		if strings.HasPrefix(c, "snapshot:") {
			t.Error("a snapshot was taken on the source before the disks were known to be readable")
		}
	}
	if len(prov.created) > 0 {
		t.Errorf("a destination disk was created before the source was known to be readable: %v", prov.created)
	}
}
