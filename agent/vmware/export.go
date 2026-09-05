package vmware

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vmware/govmomi/nfc"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

/* Reading a RUNNING guest's disks, over an export lease.

   The datastore file interface cannot do it. A guest holds its base disk open
   for as long as it runs, and ESXi will not serve an open file — measured, on a
   single-host datastore where nothing else could have held it:

     Failed to open disk: NFC_FILE_LOCKED

   with the flat file listed by the datastore browser at exactly the disk's
   size. Freezing the contents behind a snapshot does not close the guest's
   handle, so no amount of reordering reaches it.

   ExportSnapshot does reach it. It is the sanctioned read of a snapshotted
   disk, it works on a powered-on VM, and it hands back one stream per disk.
   What it will not do is seek: no Accept-Ranges, no Content-Length, and a range
   asked at the end of the disk comes back 200 OK with the file from the
   beginning. So this channel copies a disk ONCE, start to finish, and cannot
   serve a change-tracked delta. That is the whole reason a warm migration here
   is a base copy followed by a cold delta at cutover rather than a convergence.

   The lease is also a lease: vCenter takes it away if nobody says anything for
   a few minutes. Report keeps it, and reports the truth while it is at it, so
   an operator watching the task in vSphere Client sees the same progress the
   Ballast console shows. */

// ExportDisk is one disk an export lease offers.
type ExportDisk struct {
	// Path is the lease's own name for it, "disk-0.vmdk". For messages: it is
	// not a datastore path and nothing can be read from it.
	Path string
	// Size is the disk's capacity as the lease reports it, used to check the
	// lease's disks against the VM's rather than trusting their order.
	Size int64
}

/* Export is a sequential read of a VM's disks from an NFC lease.

   An interface so a pass can be driven without a vCenter. What is worth testing
   about it is the ORDER — that the lease is finished exactly once, that a
   failure aborts rather than completes — and an order can only be tested by
   watching the calls. */
type Export interface {
	// Disks are what the lease offers, in the order VMware lists them.
	Disks() []ExportDisk
	// Open starts reading one of them. The caller closes it.
	Open(ctx context.Context, i int) (io.ReadCloser, error)
	// Report renews the lease and shows progress on the vCenter task.
	Report(ctx context.Context, done, total int64)
	// Done finishes with the lease: completed if failed is nil, aborted
	// otherwise. A lease left open holds a snapshot's worth of state on the
	// source until vCenter times it out.
	Done(ctx context.Context, failed error)
}

// Export takes an export lease on a snapshot of moRef.
func (v *Client) Export(ctx context.Context, moRef, snapRef string) (Export, error) {
	snap := types.ManagedObjectReference{Type: "VirtualMachineSnapshot", Value: snapRef}
	/* Up to but NOT including the snapshot given, which is what its own
	   documentation says and what a base copy wants: the frozen disk as it was
	   when the snapshot was taken, without the delta the guest has been writing
	   since. */
	lease, err := v.vm(moRef).ExportSnapshot(ctx, &snap)
	if err != nil {
		return nil, fmt.Errorf("vmware: export the snapshot of %s: %w", moRef, err)
	}
	info, err := lease.Wait(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("vmware: wait for the export lease on %s: %w", moRef, err)
	}
	if len(info.Items) == 0 {
		lease.Abort(ctx, nil)
		return nil, fmt.Errorf("vmware: the export lease on %s offered no disks", moRef)
	}
	return &leaseExport{c: v, lease: lease, items: info.Items}, nil
}

type leaseExport struct {
	c     *Client
	lease *nfc.Lease
	items []nfc.FileItem
	// lastReport throttles the progress calls. Renewing the lease is the point;
	// a call per grain would be thousands a minute for no more information.
	lastReport time.Time
}

func (e *leaseExport) Disks() []ExportDisk {
	out := make([]ExportDisk, 0, len(e.items))
	for _, it := range e.items {
		out = append(out, ExportDisk{Path: it.Path, Size: it.Size})
	}
	return out
}

func (e *leaseExport) Open(ctx context.Context, i int) (io.ReadCloser, error) {
	if i < 0 || i >= len(e.items) {
		return nil, fmt.Errorf("vmware: the export lease has no disk %d", i)
	}
	it := e.items[i]
	/* Through the soap client, so the session the lease authenticates against
	   rides along. The URL points at the ESXi host directly rather than at
	   vCenter — every host running a warm migration has to be able to reach it,
	   which is a prerequisite worth knowing at plan time rather than eight hours
	   into a copy. */
	p := soap.DefaultDownload
	res, err := e.c.c.Client.DownloadRequest(ctx, it.URL, &p)
	if err != nil {
		return nil, fmt.Errorf("vmware: read %s from the export lease: %w", it.Path, err)
	}
	if res.StatusCode != http.StatusOK {
		reason := serverReason(res.Body)
		res.Body.Close()
		if reason != "" {
			return nil, fmt.Errorf("vmware: read %s from the export lease: the host answered %s: %s", it.Path, res.Status, reason)
		}
		return nil, fmt.Errorf("vmware: read %s from the export lease: the host answered %s", it.Path, res.Status)
	}
	return res.Body, nil
}

func (e *leaseExport) Report(ctx context.Context, done, total int64) {
	if total <= 0 || time.Since(e.lastReport) < 5*time.Second {
		return
	}
	e.lastReport = time.Now()
	pct := int32(float64(done) * 100 / float64(total))
	if pct > 100 {
		pct = 100
	}
	// Best effort. A renewal that fails will surface as the read failing, which
	// is a better message than one raised from here about a lease.
	_ = e.lease.Progress(ctx, pct)
}

func (e *leaseExport) Done(ctx context.Context, failed error) {
	if failed != nil {
		_ = e.lease.Abort(ctx, nil)
		return
	}
	if err := e.lease.Complete(ctx); err != nil {
		// Completing a lease whose transfer already finished is tidiness, not
		// correctness; aborting instead still releases it.
		_ = e.lease.Abort(ctx, nil)
	}
}

/* matchExportDisks pairs the lease's disks with the VM's, by ORDER and then by
   SIZE.

   The lease names its disks "disk-0.vmdk", which says nothing about which
   VirtualDisk key it is. Order is how VMware presents them and is almost
   certainly right — but "almost certainly" is how a two-disk VM ends up with
   its data and its log swapped, each copy internally consistent and the pair
   useless. So the sizes have to agree, and where they do not this refuses
   rather than picking one. */
func matchExportDisks(disks []Disk, offered []ExportDisk) error {
	if len(offered) != len(disks) {
		return fmt.Errorf("the export lease offers %d disk(s) and the VM has %d. Ballast will not guess which is which",
			len(offered), len(disks))
	}
	var wrong []string
	for i, d := range disks {
		if offered[i].Size != d.SizeBytes {
			wrong = append(wrong, fmt.Sprintf("%s is %d bytes and the lease's %s is %d",
				d.Label, d.SizeBytes, offered[i].Path, offered[i].Size))
		}
	}
	if len(wrong) > 0 {
		return fmt.Errorf("the export lease's disks do not line up with the VM's: %s. Copying them in this order could "+
			"put one disk's data into another, so the pass is stopping instead", strings.Join(wrong, "; "))
	}
	return nil
}
