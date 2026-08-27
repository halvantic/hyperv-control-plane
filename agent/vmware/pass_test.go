package vmware

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

/* The order a pass does things in.

   Every one of these is an ordering or a cleanup guarantee, which is exactly
   what a live run against vCenter cannot show you: a snapshot removed slightly
   too early still produces a disk, and the disk is plausible and wrong. */

type fakePass struct {
	fakeSource
	info  VMInfo
	calls []string
	// failCopy makes the copy fail, to prove cleanup still runs.
	failCopy  error
	failPower error
	removed   []string
}

func (f *fakePass) Inspect(context.Context, string) (VMInfo, error) {
	f.calls = append(f.calls, "inspect")
	return f.info, nil
}

func (f *fakePass) Snapshot(_ context.Context, _, name string) (string, error) {
	f.calls = append(f.calls, "snapshot:"+name)
	return "snapshot-9001", nil
}

func (f *fakePass) RemoveSnapshot(_ context.Context, _, snapRef string) error {
	f.calls = append(f.calls, "remove:"+snapRef)
	f.removed = append(f.removed, snapRef)
	return nil
}

func (f *fakePass) PowerOff(context.Context, string, time.Duration) error {
	f.calls = append(f.calls, "poweroff")
	return f.failPower
}

func (f *fakePass) ReadAt(ctx context.Context, p string, offset, length int64) (io.ReadCloser, error) {
	if f.failCopy != nil {
		return nil, f.failCopy
	}
	return f.fakeSource.ReadAt(ctx, p, offset, length)
}

type fakeProv struct {
	created []string
	opened  []string
	disks   map[string]*fakeSink
	closed  int
}

func (p *fakeProv) Create(_ context.Context, path string, size int64) (Disk2, error) {
	p.created = append(p.created, path)
	return p.disk(path, size), nil
}

func (p *fakeProv) Open(_ context.Context, path string) (Disk2, error) {
	p.opened = append(p.opened, path)
	return p.disk(path, 1<<20), nil
}

func (p *fakeProv) disk(path string, size int64) Disk2 {
	if p.disks == nil {
		p.disks = map[string]*fakeSink{}
	}
	if p.disks[path] == nil {
		p.disks[path] = &fakeSink{buf: make([]byte, size)}
	}
	return &countedDisk{fakeSink: p.disks[path], prov: p}
}

type countedDisk struct {
	*fakeSink
	prov *fakeProv
}

func (d *countedDisk) Close(context.Context) error {
	d.prov.closed++
	return nil
}

func onePass() (*fakePass, *fakeProv) {
	src := &fakePass{
		fakeSource: fakeSource{
			image:   pattern(1 << 20),
			extents: []Extent{{Start: 0, Length: 8192}},
			next:    "52 aa/2",
		},
		info: VMInfo{
			Name:  "Web01",
			Disks: []Disk{{Key: 2000, Label: "Hard disk 1", Path: "[ds1] Web01/Web01.vmdk", SizeBytes: 1 << 20}},
		},
	}
	return src, &fakeProv{}
}

/* Power off comes BEFORE the snapshot on a cutover.

   The other way round, whatever the guest writes between the snapshot and the
   shutdown is in neither place: not in the snapshot this pass reads, and not in
   a later pass, because there is no later pass. */
func TestTheCutoverStopsTheGuestBeforeItSnapshotsIt(t *testing.T) {
	src, prov := onePass()
	req := PassRequest{Migration: "mig-1", MoRef: "vm-1", DestDir: `C:\CSV1`, Final: true}

	out, err := runPass(t.Context(), src, prov, req, nil)
	if err != nil {
		t.Fatalf("pass failed: %v", err)
	}
	off, snap := indexOf(src.calls, "poweroff"), indexOfPrefix(src.calls, "snapshot:")
	if off < 0 || snap < 0 {
		t.Fatalf("calls were %v", src.calls)
	}
	if off > snap {
		t.Errorf("the snapshot was taken before the guest was stopped: %v", src.calls)
	}
	if !out.SourcePoweredOff {
		t.Error("the pass did not record that it stopped a production workload")
	}
}

// An ordinary delta must not touch the guest's power. Only the cutover does.
func TestADeltaPassNeverTouchesThePower(t *testing.T) {
	src, prov := onePass()
	if _, err := runPass(t.Context(), src, prov, PassRequest{
		Migration: "mig-1", MoRef: "vm-1", DestDir: `C:\CSV1`,
		Marker: map[int32]string{2000: "52 aa/1"},
	}, nil); err != nil {
		t.Fatalf("pass failed: %v", err)
	}
	if indexOf(src.calls, "poweroff") >= 0 {
		t.Errorf("a delta pass powered the source off: %v", src.calls)
	}
}

/* The snapshot is removed on EVERY exit, failure included. One left on somebody
   else's VM grows until their datastore fills, which is the worst thing this
   feature could leave behind on a system it does not own. */
func TestTheSnapshotIsRemovedEvenWhenTheCopyFails(t *testing.T) {
	src, prov := onePass()
	src.failCopy = errors.New("the datastore stopped answering")

	_, err := runPass(t.Context(), src, prov, PassRequest{Migration: "mig-1", MoRef: "vm-1", DestDir: `C:\CSV1`}, nil)
	if err == nil {
		t.Fatal("a failed copy reported success")
	}
	if len(src.removed) != 1 || src.removed[0] != "snapshot-9001" {
		t.Errorf("the snapshot was not removed after a failure: %v", src.calls)
	}
	// And the destination disk was dismounted, or it stays attached to the host
	// across reboots and the next pass finds it in a state it did not expect.
	if prov.closed != 1 {
		t.Errorf("the destination disk was closed %d times after a failure", prov.closed)
	}
}

/* A pass with no marker CREATES the destination; one with a marker OPENS it.

   Getting this backwards is the quiet catastrophe: Create replaces what it
   finds, so a delta that created its disk would start from empty, finish, be
   imported, and produce a VM with a blank disk that nothing anywhere reported. */
func TestADeltaOpensTheDiskAndABaseCopyCreatesIt(t *testing.T) {
	src, prov := onePass()
	if _, err := runPass(t.Context(), src, prov, PassRequest{Migration: "m", MoRef: "vm-1", DestDir: `C:\CSV1`}, nil); err != nil {
		t.Fatalf("base pass failed: %v", err)
	}
	if len(prov.created) != 1 || len(prov.opened) != 0 {
		t.Errorf("a base copy created %v and opened %v", prov.created, prov.opened)
	}

	src2, prov2 := onePass()
	if _, err := runPass(t.Context(), src2, prov2, PassRequest{
		Migration: "m", MoRef: "vm-1", DestDir: `C:\CSV1`, Marker: map[int32]string{2000: "52 aa/1"},
	}, nil); err != nil {
		t.Fatalf("delta pass failed: %v", err)
	}
	if len(prov2.created) != 0 || len(prov2.opened) != 1 {
		t.Errorf("a delta created %v and opened %v", prov2.created, prov2.opened)
	}
}

/* The snapshot carries the migration's name, so a leftover one on a customer's
   vCenter can be traced back to what made it. */
func TestTheSnapshotIsNamedForTheMigration(t *testing.T) {
	src, prov := onePass()
	if _, err := runPass(t.Context(), src, prov, PassRequest{Migration: "mig-web01", MoRef: "vm-1", DestDir: `C:\CSV1`}, nil); err != nil {
		t.Fatalf("pass failed: %v", err)
	}
	i := indexOfPrefix(src.calls, "snapshot:")
	if i < 0 || !strings.Contains(src.calls[i], "mig-web01") {
		t.Errorf("the snapshot was not named for the migration: %v", src.calls)
	}
}

/* A VM with no disks is a vCLS agent or a template, not a workload. Copying
   nothing and reporting success would produce an empty VM at the far end. */
func TestAVMWithNoDisksIsRefusedWithTheReason(t *testing.T) {
	src, prov := onePass()
	src.info.Disks = nil
	_, err := runPass(t.Context(), src, prov, PassRequest{Migration: "m", MoRef: "vm-1", DestDir: `C:\CSV1`}, nil)
	if err == nil {
		t.Fatal("a VM with no disks was migrated")
	}
	if !strings.Contains(err.Error(), "vCLS") {
		t.Errorf("the refusal does not say what such a VM usually is: %v", err)
	}
	if indexOfPrefix(src.calls, "snapshot:") >= 0 {
		t.Error("a snapshot was taken on a VM there was nothing to copy from")
	}
}

/* A CBT reset is reported as recoverable in the same breath as the failure. The
   remedy is automatic and costs only time, and an operator who reads it as a
   lost migration will start again from nothing. */
func TestACBTResetSaysWhatHappensNext(t *testing.T) {
	src, prov := onePass()
	src.fakeSource.err = explainCBTError(errors.New("A specified parameter was not correct: changeId"), "52 aa/1")

	_, err := runPass(t.Context(), src, prov, PassRequest{
		Migration: "m", MoRef: "vm-1", DestDir: `C:\CSV1`, Marker: map[int32]string{2000: "52 aa/1"},
	}, nil)
	if !errors.Is(err, ErrCBTReset) {
		t.Fatalf("a reset was not reported as one: %v", err)
	}
	if !strings.Contains(err.Error(), "read this disk in full") {
		t.Errorf("the failure does not say the next pass recovers: %v", err)
	}
	if !strings.Contains(err.Error(), "Hard disk 1") {
		t.Errorf("the failure does not say which disk: %v", err)
	}
}

/* Destination names come from the SOURCE disk's file name. A VM with disks
   added and removed over the years has keys 2000, 2002 and 2005; numbering the
   destinations 1, 2, 3 loses the only thing that lets an operator match a VHDX
   back to the VMDK it came from a year later. */
func TestADestinationIsNamedAfterTheDiskItCameFrom(t *testing.T) {
	got := DestPathFor(`C:\ClusterStorage\DS1`, "Web01", Disk{Key: 2001, Path: "[datastore1] Web01/Web01_1.vmdk"})
	if !strings.HasSuffix(got, `Web01\Web01_1.vhdx`) {
		t.Errorf("destination %q does not carry the source disk's name", got)
	}
}

/* VMware names allow characters NTFS does not. A VM called "web/prod" must not
   create a directory nobody asked for, and one ending in a dot produces a path
   Windows will write and never open again. */
func TestASourceNameCannotEscapeIntoThePath(t *testing.T) {
	got := DestPathFor(`C:\CSV1`, `web/prod: "live"`, Disk{Key: 2000, Path: "[ds1] a/b.vmdk"})
	if strings.Contains(strings.TrimPrefix(got, `C:\CSV1\`), "/") {
		t.Errorf("a source name put a path separator in %q", got)
	}
	for _, bad := range []string{":", `"`, "*", "?", "|"} {
		if strings.Contains(strings.TrimPrefix(got, `C:\`), bad) {
			t.Errorf("%q survived into the path %q", bad, got)
		}
	}
	if s := sanitise("trailing. "); strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") {
		t.Errorf("sanitise left a trailing dot or space: %q", s)
	}
	if sanitise("   ") != "vm" {
		t.Errorf("a name that sanitises to nothing became %q", sanitise("   "))
	}
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}

func indexOfPrefix(xs []string, prefix string) int {
	for i, x := range xs {
		if strings.HasPrefix(x, prefix) {
			return i
		}
	}
	return -1
}
