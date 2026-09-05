package vmware

import (
	"fmt"
	"strings"
	"testing"
)

// destDir is where the fake provisioner puts destination disks.
const destDir = `C:\CSV1`

/* Copying a RUNNING source, over an export lease.

   The datastore path cannot read one at all — the guest holds its own disk file
   open and ESXi refuses it, measured as NFC_FILE_LOCKED against a file the
   datastore listed at exactly the disk's size. So a warm pass goes through a
   different door, and these are the guarantees that door has to keep. */

// warmPass is a running source whose lease serves one disk of built grains.
func warmPass() (*fakePass, *fakeProv) {
	src, prov := onePass()
	src.info.PowerState = poweredOnState
	src.fakeSource.next = "52 aa/7"
	src.fakeSource.extents = []Extent{{Start: 0, Length: 2 * testGrainBytes}}
	src.export = &fakeExport{
		disks: []ExportDisk{{Path: "disk-0.vmdk", Size: 1 << 20}},
		streams: [][]byte{buildStream((1<<20)/vmdkSector, []builtGrain{
			{sector: 0, data: fill(0xA1)},
			{sector: 8 * testGrainSectors, data: fill(0xB2)},
		})},
	}
	return src, prov
}

func TestARunningSourceIsCopiedFromTheLeaseAndNeverTheDatastore(t *testing.T) {
	src, prov := warmPass()
	out, err := runPass(t.Context(), src, prov, PassRequest{Migration: "mig-1", MoRef: "vm-1", DestDir: destDir}, nil)
	if err != nil {
		t.Fatalf("the warm pass failed: %v", err)
	}
	// Nothing may touch the datastore: every one of those reads is the one that
	// comes back NFC_FILE_LOCKED on a running guest.
	if indexOfPrefix(src.calls, "resolve:") >= 0 || len(src.fakeSource.reads) > 0 {
		t.Errorf("a running source was read over the datastore: %v", src.calls)
	}
	if indexOfPrefix(src.calls, "export:") < 0 {
		t.Fatalf("no export lease was taken: %v", src.calls)
	}
	if want := int64(2 * testGrainBytes); out.CopiedBytes != want {
		t.Errorf("copied %d bytes, want %d", out.CopiedBytes, want)
	}

	sink := prov.disks[DestPathFor(destDir, "Web01", src.info.Disks[0])]
	if sink == nil {
		t.Fatalf("nothing was written to the destination; disks written: %v", prov.created)
	}
	if sink.buf[0] != 0xA1 {
		t.Error("the first grain did not land at offset 0")
	}
	if sink.buf[8*testGrainBytes] != 0xB2 {
		t.Error("the second grain did not land at the sector the stream named")
	}
	// The gap between them was never sent and must still be empty.
	if sink.buf[3*testGrainBytes] != 0 {
		t.Error("empty space was written over")
	}
}

// The marker for the cutover's delta has to come back from the warm base, or
// the cutover re-reads the whole disk with the guest stopped — a cold migration
// discovered at the worst possible moment.
func TestTheWarmBaseCarriesTheMarkerTheCutoverNeeds(t *testing.T) {
	src, prov := warmPass()
	out, err := runPass(t.Context(), src, prov, PassRequest{Migration: "mig-1", MoRef: "vm-1", DestDir: destDir}, nil)
	if err != nil {
		t.Fatalf("the warm pass failed: %v", err)
	}
	if len(out.Disks) != 1 || out.Disks[0].NextChangeID != "52 aa/7" {
		t.Fatalf("the warm base returned no change marker: %+v", out.Disks)
	}
}

// A source that cannot answer a change-tracking query is refused BEFORE the
// hours of copying, not at the cutover that would have nothing to read from.
func TestAWarmCopyWithoutChangeTrackingIsRefusedUpFront(t *testing.T) {
	src, prov := warmPass()
	src.fakeSource.err = fmt.Errorf("%w: nothing tracked", ErrCBTUnavailable)

	_, err := runPass(t.Context(), src, prov, PassRequest{Migration: "mig-1", MoRef: "vm-1", DestDir: destDir}, nil)
	if err == nil {
		t.Fatal("a warm copy that could never cut over was started anyway")
	}
	if !strings.Contains(err.Error(), "cold migration with extra steps") {
		t.Errorf("the refusal does not explain what warm would have bought: %v", err)
	}
	if len(src.export.opened) > 0 {
		t.Error("it began copying before finding out")
	}
}

/* A delta cannot be read from a lease — it serves no byte ranges — and asking
   for one must be refused rather than quietly re-copying the whole disk and
   reporting it as an increment. */
func TestADeltaIsNeverAttemptedOverTheLease(t *testing.T) {
	src, prov := warmPass()
	req := PassRequest{Migration: "mig-1", MoRef: "vm-1", DestDir: destDir, Marker: map[int32]string{2000: "52 aa/2"}}

	_, err := runPass(t.Context(), src, prov, req, nil)
	if err == nil {
		t.Fatal("a delta was attempted against a running source")
	}
	if !strings.Contains(err.Error(), "start to finish") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if indexOfPrefix(src.calls, "export:") >= 0 {
		t.Errorf("a lease was taken before the refusal: %v", src.calls)
	}
}

/* The lease is released on every exit, and a FAILED transfer aborts it.

   Completing a lease whose disk did not arrive tells vCenter something untrue
   about a copy Ballast now owns; leaving it open holds state on somebody else's
   system until it times out. */
func TestTheLeaseIsAbortedWhenTheCopyFails(t *testing.T) {
	src, prov := warmPass()
	// A stream cut short: everything before the cut is real data at real
	// offsets, which is exactly what makes it dangerous.
	full := src.export.streams[0]
	src.export.streams[0] = full[:len(full)-vmdkSector*2]

	_, err := runPass(t.Context(), src, prov, PassRequest{Migration: "mig-1", MoRef: "vm-1", DestDir: destDir}, nil)
	if err == nil {
		t.Fatal("a truncated stream was accepted as a finished disk")
	}
	if len(src.export.ended) != 1 || src.export.ended[0] != "abort" {
		t.Errorf("the lease ended as %v, want one abort", src.export.ended)
	}
	// And the snapshot still goes.
	if len(src.removed) != 1 {
		t.Errorf("the snapshot was left on the source: %v", src.calls)
	}
}

func TestTheLeaseIsCompletedWhenTheCopySucceeds(t *testing.T) {
	src, prov := warmPass()
	if _, err := runPass(t.Context(), src, prov, PassRequest{Migration: "mig-1", MoRef: "vm-1", DestDir: destDir}, nil); err != nil {
		t.Fatalf("the warm pass failed: %v", err)
	}
	if len(src.export.ended) != 1 || src.export.ended[0] != "complete" {
		t.Errorf("the lease ended as %v, want one complete", src.export.ended)
	}
}

/* The lease names its disks "disk-0.vmdk", which says nothing about which
   VirtualDisk it is. Order is how VMware presents them, and a two-disk VM whose
   order is wrong gets its data and its log swapped — each copy internally
   consistent, the pair useless. The sizes have to agree. */
func TestDisksThatDoNotLineUpAreRefusedRatherThanGuessedAt(t *testing.T) {
	src, prov := warmPass()
	src.export.disks = []ExportDisk{{Path: "disk-0.vmdk", Size: 40 << 30}}

	_, err := runPass(t.Context(), src, prov, PassRequest{Migration: "mig-1", MoRef: "vm-1", DestDir: destDir}, nil)
	if err == nil {
		t.Fatal("disks of different sizes were paired up anyway")
	}
	if !strings.Contains(err.Error(), "do not line up") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
	if len(src.export.opened) > 0 {
		t.Error("it started copying before checking")
	}
}

// A STOPPED source still goes down the datastore path, which is where change
// tracking works and only the changed ranges move.
func TestAStoppedSourceStillUsesTheDatastore(t *testing.T) {
	src, prov := onePass()
	src.info.PowerState = "poweredOff"
	if _, err := runPass(t.Context(), src, prov, PassRequest{Migration: "mig-1", MoRef: "vm-1", DestDir: destDir}, nil); err != nil {
		t.Fatalf("the cold pass failed: %v", err)
	}
	if indexOfPrefix(src.calls, "export:") >= 0 {
		t.Errorf("a stopped source was read over a lease: %v", src.calls)
	}
	if indexOfPrefix(src.calls, "resolve:") < 0 {
		t.Errorf("a stopped source did not use the datastore: %v", src.calls)
	}
}

// The cutover stops the guest first, so it too takes the datastore path — the
// one that can read only what changed.
func TestTheCutoverReadsTheDatastoreEvenThoughTheSourceWasRunning(t *testing.T) {
	src, prov := warmPass()
	req := PassRequest{Migration: "mig-1", MoRef: "vm-1", DestDir: destDir, Final: true}
	if _, err := runPass(t.Context(), src, prov, req, nil); err != nil {
		t.Fatalf("the cutover failed: %v", err)
	}
	if indexOfPrefix(src.calls, "export:") >= 0 {
		t.Errorf("the cutover took a lease instead of reading changed ranges: %v", src.calls)
	}
	if indexOfPrefix(src.calls, "poweroff") < 0 {
		t.Errorf("the cutover did not stop the guest: %v", src.calls)
	}
}
