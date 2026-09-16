/*
nfcprobe answers one question that cannot be answered by reading documentation:
what does an NFC export lease actually serve, and can it be read at an offset?

	Warm migration is blocked because the datastore file interface refuses a
	running guest's disk — the guest holds it open, and a snapshot freezes the
	contents without closing the handle. ExportSnapshot is the candidate
	replacement. Whether it can carry the existing design turns on two facts:

	  FORMAT   raw disk bytes, or a stream-optimised VMDK that has to be decoded
	  RANGES   whether a ranged GET is honoured (206) or ignored (200)

	Both together decide whether warm can converge with change tracking, or
	whether it is one sequential base copy followed by a cold delta at cutover.
	Everything else in the design follows from the answer, so it is measured
	before anything is built on it.

	This program is deliberately read-only about the guest: it takes a snapshot
	named for itself, reads a few kilobytes, and removes the snapshot on every
	exit including a failed one. It never writes to the source and never touches
	the destination.

Usage:

	go run ./cmd/nfcprobe -host vcsa-02.lab.example -user administrator@vsphere.local -vm BallastJumphost -insecure

The password is read from BALLAST_PROBE_PASSWORD, or prompted for, so it does
not end up in a shell history.
*/
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/nfc"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

const snapshotName = "ballast-nfcprobe"

func main() {
	var (
		host     = flag.String("host", "", "vCenter address")
		user     = flag.String("user", "", "vCenter username")
		vmName   = flag.String("vm", "", "VM name to probe")
		insecure = flag.Bool("insecure", false, "skip TLS verification")
		probeLen = flag.Int64("len", 4096, "bytes to request in each probe")
	)
	flag.Parse()
	if *host == "" || *user == "" || *vmName == "" {
		flag.Usage()
		os.Exit(2)
	}
	pass := os.Getenv("BALLAST_PROBE_PASSWORD")
	if pass == "" {
		fmt.Fprint(os.Stderr, "password: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		pass = strings.TrimSpace(line)
	}

	// Interrupt still has to remove the snapshot; that is the whole reason the
	// context is separated from the cleanup below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, *host, *user, pass, *vmName, *insecure, *probeLen); err != nil {
		fmt.Fprintf(os.Stderr, "\nprobe failed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, host, user, pass, vmName string, insecure bool, probeLen int64) error {
	addr := host
	if !strings.Contains(addr, "://") {
		addr = "https://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return fmt.Errorf("bad address %q: %w", host, err)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/sdk"
	}
	u.User = url.UserPassword(user, pass)

	c, err := govmomi.NewClient(ctx, u, insecure)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer c.Logout(context.WithoutCancel(ctx))

	finder := find.NewFinder(c.Client, false)
	dc, err := finder.DefaultDatacenter(ctx)
	if err != nil {
		return fmt.Errorf("find datacenter: %w", err)
	}
	finder.SetDatacenter(dc)
	vm, err := finder.VirtualMachine(ctx, vmName)
	if err != nil {
		return fmt.Errorf("find VM %q: %w", vmName, err)
	}

	var m mo.VirtualMachine
	pc := property.DefaultCollector(c.Client)
	if err := pc.RetrieveOne(ctx, vm.Reference(), []string{"name", "runtime.powerState", "config.hardware.device"}, &m); err != nil {
		return fmt.Errorf("read VM: %w", err)
	}
	fmt.Printf("VM        %s (%s), power %s\n", m.Name, vm.Reference().Value, m.Runtime.PowerState)
	for _, dev := range m.Config.Hardware.Device {
		if d, ok := dev.(*types.VirtualDisk); ok {
			fmt.Printf("disk      key %d, %d bytes\n", d.Key, d.CapacityInBytes)
		}
	}
	// The whole question is about a RUNNING guest. Answering it against a
	// stopped one would measure the case that already works.
	if m.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOn {
		fmt.Printf("\nWARNING: %s is %s. The datastore interface already works on a stopped VM, so this run does not\n"+
			"         answer the question warm migration is blocked on.\n", m.Name, m.Runtime.PowerState)
	}

	fmt.Printf("\ntaking snapshot %q\n", snapshotName)
	task, err := vm.CreateSnapshot(ctx, snapshotName,
		"Taken by Ballast's nfcprobe to measure the export lease. Removed automatically.", false, false)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	info, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	snap := info.Result.(types.ManagedObjectReference)

	// Its own context and its own deferral: an interrupt or a failure is exactly
	// when leaving a snapshot on somebody's VM matters most.
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer cancel()
		fmt.Printf("\nremoving snapshot\n")
		t, rerr := vm.RemoveSnapshot(cctx, snap.Value, false, nil)
		if rerr == nil {
			rerr = t.Wait(cctx)
		}
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "SNAPSHOT NOT REMOVED — remove %q in vSphere Client: %v\n", snapshotName, rerr)
		}
	}()

	/* ExportSnapshot, whose own documentation says it exports the files "up to
	   but not including" the snapshot given — which is the frozen base, and
	   exactly what a base copy wants. */
	lease, err := vm.ExportSnapshot(ctx, &snap)
	if err != nil {
		return fmt.Errorf("export snapshot: %w", err)
	}
	li, err := lease.Wait(ctx, nil)
	if err != nil {
		return fmt.Errorf("lease: %w", err)
	}
	defer lease.Abort(context.WithoutCancel(ctx), nil)

	fmt.Printf("lease     %d item(s), disk capacity %d KB\n", len(li.Items), li.TotalDiskCapacityInKB)
	for i := range li.Items {
		probeItem(ctx, c, li.Items[i], probeLen)
	}
	return nil
}

/*
probeItem asks the two questions of one disk in the lease.

	Both reads go through the soap client rather than a bare http.Client, so the
	session cookie the lease authenticates against rides along — the same channel
	a real copy would use.
*/
func probeItem(ctx context.Context, c *govmomi.Client, item nfc.FileItem, probeLen int64) {
	fmt.Printf("\n--- %s\n", item.Path)
	fmt.Printf("url       %s\n", item.URL)
	fmt.Printf("size      %d bytes (as the lease reports it)\n", item.Size)

	// 1. FORMAT. The first bytes say what this is without reading the disk.
	head, hres, err := get(ctx, c, item.URL, "")
	if err != nil {
		fmt.Printf("format    could not read: %v\n", err)
	} else {
		fmt.Printf("status    %s\n", hres.Status)
		fmt.Printf("headers   Content-Length=%s Content-Type=%s Accept-Ranges=%q\n",
			hres.Header.Get("Content-Length"), hres.Header.Get("Content-Type"), hres.Header.Get("Accept-Ranges"))
		fmt.Printf("format    %s\n", identify(head))
		fmt.Printf("first 32  % x\n", head[:min(32, len(head))])
		describeHeader(head)
	}

	/* 2. RANGES, at the END of the disk.

	   The end, not the start: a server that ignores the range answers 200 with
	   the file from the beginning, and at offset 0 that is indistinguishable
	   from a range honoured. Only a read somewhere else can tell them apart —
	   the same reason the datastore resolver probes the last sector. */
	if item.Size <= probeLen {
		fmt.Printf("ranges    not probed: the lease reports only %d bytes\n", item.Size)
		return
	}
	off := item.Size - probeLen
	_, rres, err := get(ctx, c, item.URL, fmt.Sprintf("bytes=%d-%d", off, item.Size-1))
	if err != nil {
		fmt.Printf("ranges    could not read at %d: %v\n", off, err)
		return
	}
	fmt.Printf("ranges    asked bytes=%d-%d, answered %s\n", off, item.Size-1, rres.Status)
	switch rres.StatusCode {
	case http.StatusPartialContent:
		fmt.Printf("VERDICT   ranges honoured — random access works over the lease\n")
	case http.StatusOK:
		fmt.Printf("VERDICT   range IGNORED (200, whole file from the start) — sequential only, so a change-tracked\n" +
			"          delta cannot be read over this channel\n")
	default:
		fmt.Printf("VERDICT   refused: %s\n", rres.Status)
	}
}

// get reads up to 512 bytes and returns the response, without consuming a whole
// disk to answer a question about its first sector.
func get(ctx context.Context, c *govmomi.Client, u *url.URL, rng string) ([]byte, *http.Response, error) {
	p := soap.DefaultDownload
	if rng != "" {
		p.Headers = map[string]string{"Range": rng}
	}
	res, err := c.Client.DownloadRequest(ctx, u, &p)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64*512))
	return b, res, nil
}

/*
identify names the format from its first bytes.

	"KDMV" is the VMDK sparse header, which is what a stream-optimised export
	looks like and what would have to be decoded. A raw disk image starts with
	the guest's own boot sector instead, and 0x55AA at offset 510 is the marker
	for one.
*/
func identify(b []byte) string {
	switch {
	case len(b) >= 4 && string(b[:4]) == "KDMV":
		return "stream-optimised / sparse VMDK (KDMV header) — needs a grain decoder, not addressable by disk offset"
	case len(b) >= 4 && string(b[:4]) == "COWD":
		return "COWD sparse VMDK — needs a decoder"
	case len(b) >= 512 && b[510] == 0x55 && b[511] == 0xAA:
		return "raw disk image (MBR boot signature) — readable by disk offset"
	case bytes.HasPrefix(b, []byte("EFI PART")) || (len(b) >= 512 && bytes.HasPrefix(b[512:], []byte("EFI PART"))):
		return "raw disk image (GPT) — readable by disk offset"
	case bytes.HasPrefix(b, []byte("# Disk Descriptor")):
		return "a VMDK text descriptor, not disk data"
	default:
		return "unrecognised — see the first bytes below"
	}
}
