// Package vmware is the AGENT's half of a migration: the part that moves bytes.
//
// The centre reads inventory and decides; this connects to the same vCenter
// from the Hyper-V host that will own the VM and streams the disks straight
// onto its storage. One hop instead of two, no staging space anywhere, and
// several hosts migrating at once rather than queueing behind the centre.
//
// It is a separate package from centre/vmware on purpose. That one is read-only
// by design and says so; this one enables Changed Block Tracking, takes
// snapshots and powers VMs off. Sharing a package would put the destructive
// calls one import away from the code that promises not to make them.
package vmware

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// Endpoint is everything needed to reach a source. Delivered on the job rather
// than cached on the host, for the reason CHAP secrets are: a credential that
// lives on a host outlives the operator's intent for it.
type Endpoint struct {
	Address     string
	Username    string
	Password    string
	InsecureTLS bool
}

// Client is a connection from an agent to one vCenter or ESXi.
type Client struct {
	c *govmomi.Client
	// pin is the ESXi host every datastore read is routed through, or nil to
	// let vCenter choose one. See PinReads.
	pin *object.HostSystem
}

func Connect(ctx context.Context, e Endpoint) (*Client, error) {
	addr := strings.TrimSpace(e.Address)
	if !strings.Contains(addr, "://") {
		addr = "https://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("vmware: bad address %q: %w", e.Address, err)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/sdk"
	}
	u.User = url.UserPassword(e.Username, e.Password)

	c, err := govmomi.NewClient(ctx, u, e.InsecureTLS)
	if err != nil {
		return nil, fmt.Errorf("vmware: connect %s: %s", u.Host, explainConnect(u.Host, err))
	}
	return &Client{c: c}, nil
}

/*
explainConnect names what went wrong from THIS side of the wire.

	The centre explains its own connection failures and this one did not, so
	govmomi's raw text reached the console: `dial tcp: lookup vcsa-02: no such
	host`. That says a lookup failed. It does not say WHO looked, and that is the
	whole of the answer here — the copy runs on the Hyper-V host and asks the
	host's own DNS, so a source the centre resolves perfectly well can be
	unknown on the host that has to fetch from it. An operator reading the raw
	text goes and checks the centre, where everything works.

	Worded for the host rather than shared with the centre's version: the same
	condition has a different remedy depending on which machine hit it.
*/
func explainConnect(host string, err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "no such host") || strings.Contains(s, "server misbehaving"):
		return host + " did not resolve on this Hyper-V host. The copy runs here and asks this host's own DNS, " +
			"so a source the centre resolves may still be unknown here — give the host a DNS server that knows the " +
			"name, add a hosts entry, or register the source by IP address."
	case strings.Contains(s, "incorrect user name or password") || strings.Contains(s, "cannot complete login"):
		return "the credential was rejected by " + host + ". For vCenter this is usually an SSO account such as " +
			"administrator@vsphere.local; a local ESXi account will not authenticate against vCenter."
	case strings.Contains(s, "certificate") || strings.Contains(s, "x509"):
		return "the certificate presented by " + host + " is not trusted by this Hyper-V host. Tick “skip certificate " +
			"verification” on the source if that is what you intend: " + err.Error()
	case strings.Contains(s, "connection refused"):
		return "nothing answered on " + host + " port 443. The name resolves from this host, so this is a firewall or the wrong address."
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline exceeded"):
		return "the connection to " + host + " timed out. This Hyper-V host has no route to it, or a firewall is dropping rather than refusing."
	}
	return err.Error()
}

func (v *Client) Close(ctx context.Context) {
	if v == nil || v.c == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = v.c.Logout(cctx)
}

func (v *Client) vm(moRef string) *object.VirtualMachine {
	return object.NewVirtualMachine(v.c.Client, types.ManagedObjectReference{Type: "VirtualMachine", Value: moRef})
}

// Disk describes one virtual disk to copy.
type Disk struct {
	Key int32
	// Label is what an operator sees in vSphere Client.
	Label string
	// Path is the datastore path of the DESCRIPTOR, "[datastore1] Web01/Web01.vmdk".
	// It is what VMware reports as the backing and it is what an operator
	// recognises, but on VMFS it is a few hundred bytes of text.
	Path string
	// DataPath is the file that actually holds the bytes, resolved by asking the
	// datastore. Usually "[datastore1] Web01/Web01-flat.vmdk"; the same as Path
	// where the datastore keeps one file. Reads use this; messages use Path.
	DataPath string
	// SizeBytes is the guest-visible capacity, which is what the destination
	// VHDX is created at.
	SizeBytes int64
	// Snapshotted is whether this disk is running on a snapshot chain — its
	// backing has a parent, so the live file is a delta holding only what has
	// changed since the snapshot was taken. A pass reads ONE file per disk over
	// the datastore interface and cannot compose a chain, so this decides
	// whether the disk can be copied at all.
	Snapshotted bool
}

// VMInfo is what a copy needs to know about the source.
type VMInfo struct {
	Name       string
	PowerState string
	CBTEnabled bool
	Disks      []Disk
}

// Inspect reads the VM's current shape. Read every pass rather than carried on
// the job: a disk added or grown in VMware between passes changes what has to
// be copied, and a plan made an hour ago would quietly copy the old shape.
func (v *Client) Inspect(ctx context.Context, moRef string) (VMInfo, error) {
	var m mo.VirtualMachine
	pc := property.DefaultCollector(v.c.Client)
	err := pc.RetrieveOne(ctx, v.vm(moRef).Reference(), []string{
		"name", "runtime.powerState", "config.changeTrackingEnabled", "config.hardware.device",
	}, &m)
	if err != nil {
		return VMInfo{}, fmt.Errorf("vmware: read %s: %w", moRef, err)
	}
	out := VMInfo{Name: m.Name, PowerState: string(m.Runtime.PowerState)}
	if m.Config == nil {
		return out, fmt.Errorf("vmware: %s reports no configuration, so there is nothing to copy", moRef)
	}
	out.CBTEnabled = m.Config.ChangeTrackingEnabled != nil && *m.Config.ChangeTrackingEnabled
	for _, dev := range m.Config.Hardware.Device {
		d, ok := dev.(*types.VirtualDisk)
		if !ok {
			continue
		}
		disk := Disk{Key: d.Key, SizeBytes: d.CapacityInBytes}
		if d.DeviceInfo != nil {
			disk.Label = d.DeviceInfo.GetDescription().Label
		}
		switch b := d.Backing.(type) {
		case *types.VirtualDiskFlatVer2BackingInfo:
			disk.Path = b.FileName
			// A redo log on VMFS is a flat backing WITH a parent, and the file
			// it names ("…-000001.vmdk") is the delta, not the disk.
			disk.Snapshotted = b.Parent != nil
		case *types.VirtualDiskSeSparseBackingInfo:
			// SEsparse exists only as a snapshot delta.
			disk.Path = b.FileName
			disk.Snapshotted = true
		}
		out.Disks = append(out.Disks, disk)
	}
	return out, nil
}

/*
EnableCBT turns Changed Block Tracking on.

	Refused on a running VM, deliberately. The setting applies at power-on, so
	enabling it on a running guest reports success and changes nothing — the
	next QueryChangedDiskAreas fails and the operator is left with a migration
	that said it was ready and was not. Ballast will not restart somebody's VM
	to save itself a step, so the honest answer is to say what has to happen.
*/
func (v *Client) EnableCBT(ctx context.Context, moRef string) error {
	info, err := v.Inspect(ctx, moRef)
	if err != nil {
		return err
	}
	if info.CBTEnabled {
		return nil // already on; nothing to do and nothing to say
	}
	if info.PowerState == string(types.VirtualMachinePowerStatePoweredOn) {
		return fmt.Errorf("%s is running and Changed Block Tracking is off. It takes effect at power-on, so turning it on now "+
			"would report success and change nothing. Restart the VM once at a time of your choosing, then migrate with no further "+
			"downtime until cutover — or migrate it cold, which copies once and needs it stopped anyway", info.Name)
	}

	on := true
	task, err := v.vm(moRef).Reconfigure(ctx, types.VirtualMachineConfigSpec{ChangeTrackingEnabled: &on})
	if err != nil {
		return fmt.Errorf("vmware: enable tracking on %s: %w", info.Name, err)
	}
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("vmware: enable tracking on %s: %w", info.Name, err)
	}
	return nil
}

/*
Snapshot freezes the disks so they can be read while the guest runs.

	Memory is deliberately NOT captured: it would double the snapshot's cost and
	the time the guest is stunned, and nothing here ever restores it. Quiescing
	is also off — it needs VMware Tools, fails on guests that do not have it,
	and turning a copy into a refusal because a guest could not flush its
	filesystem is the wrong trade for a migration that will be cut over cleanly
	later.
*/
func (v *Client) Snapshot(ctx context.Context, moRef, name string) (string, error) {
	task, err := v.vm(moRef).CreateSnapshot(ctx, name,
		"Created by Ballast for a migration. Safe to leave; Ballast removes it when the migration ends.",
		false /* memory */, false /* quiesce */)
	if err != nil {
		return "", fmt.Errorf("vmware: snapshot %s: %w", moRef, err)
	}
	info, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("vmware: snapshot %s: %w", moRef, err)
	}
	ref, ok := info.Result.(types.ManagedObjectReference)
	if !ok {
		return "", fmt.Errorf("vmware: snapshot %s: the task returned no snapshot reference", moRef)
	}
	return ref.Value, nil
}

/*
RemoveSnapshot deletes one and waits for the data to merge back.

	Waiting matters. Returning as soon as the task is accepted would let the
	next pass start while consolidation is still running, and the next pass
	reads the base disk — which is mid-merge and not yet complete. Consolidation
	is also the expensive part for the source's datastore, so a pass that
	overlaps the last one's merge is how a migration starts hurting the system
	it is leaving. Removing one that has already gone is not an error: this runs
	on cleanup paths that do not know how far a failed attempt got.
*/
func (v *Client) RemoveSnapshot(ctx context.Context, moRef, snapRef string) error {
	if strings.TrimSpace(snapRef) == "" {
		return nil
	}
	// By reference, not by name. Ballast names its snapshots, and a name is
	// something a person can create a second of in vSphere Client — removing
	// "the one called ballast-migration" could then remove somebody else's.
	// Bounded. Waiting for ever on a merge that is not progressing hangs the
	// migration with no message; giving up says which datastore to go and look
	// at, and leaves the snapshot where an operator can see it.
	wctx, cancel := context.WithTimeout(ctx, consolidateWait)
	defer cancel()

	consolidate := true
	task, err := v.vm(moRef).RemoveSnapshot(wctx, snapRef, false /* removeChildren */, &consolidate)
	if err != nil {
		if isGone(err) {
			return nil
		}
		return fmt.Errorf("vmware: remove snapshot %s: %w", snapRef, err)
	}
	if err := task.Wait(wctx); err != nil {
		if isGone(err) {
			return nil
		}
		if wctx.Err() != nil && ctx.Err() == nil {
			return fmt.Errorf("vmware: snapshot %s on %s was still merging after %s. The migration is stopping rather than "+
				"reading a disk that is mid-merge; the snapshot is still there and the source datastore is where to look",
				snapRef, moRef, consolidateWait)
		}
		return fmt.Errorf("vmware: remove snapshot %s: %w", snapRef, err)
	}
	return nil
}

// isGone covers a snapshot that is not there any more, however that is phrased.
// "snapshot %q not found" and "no snapshots for this VM" come from the lookup
// govmomi does before the removal, and both mean the same thing as a successful
// delete for a caller that is tidying up.
func isGone(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no snapshots for this vm") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "has already been deleted") ||
		strings.Contains(s, "could not be found") ||
		strings.Contains(s, "managed object not found")
}

// Extent is one region of a disk that holds data or has changed.
type Extent struct {
	Start  int64
	Length int64
}

/*
ChangedAreas asks what to copy.

	changeID "*" means "everything allocated", which is what a BASE copy wants:
	a thin 500GB disk with 40GB written transfers 40GB, not 500. The same call
	with the previous pass's marker returns only what has changed since, so the
	base copy and every delta are one operation with a different argument. The
	returned ChangeId is the marker for the NEXT pass and must be stored whether
	or not anything came back — a pass that found nothing still moves the marker
	forward, and reusing the old one would re-copy that window for ever.
*/
func (v *Client) ChangedAreas(ctx context.Context, moRef, snapRef string, deviceKey int32, changeID string, diskSize int64) ([]Extent, string, error) {
	if changeID == "" {
		changeID = "*"
	}
	snap := types.ManagedObjectReference{Type: "VirtualMachineSnapshot", Value: snapRef}

	var out []Extent
	// The API returns at most a page of areas per call and expects to be asked
	// again from where it stopped. Looping until the reported end reaches the
	// disk size is what makes a large disk work at all.
	offset := int64(0)
	for offset < diskSize {
		res, err := methods.QueryChangedDiskAreas(ctx, v.c.Client, &types.QueryChangedDiskAreas{
			This:        v.vm(moRef).Reference(),
			Snapshot:    &snap,
			DeviceKey:   deviceKey,
			StartOffset: offset,
			ChangeId:    changeID,
		})
		if err != nil {
			return nil, "", explainCBTError(err, changeID)
		}
		area := res.Returnval
		for _, c := range area.ChangedArea {
			out = append(out, Extent{Start: c.Start, Length: c.Length})
		}
		next := area.StartOffset + area.Length
		if next <= offset {
			// No forward progress: stop rather than loop for ever on a source
			// that is answering but not advancing.
			break
		}
		offset = next
	}

	// The marker for the NEXT pass is read from the snapshot, NOT carried over
	// from the input. Returning the input would leave every delta querying from
	// the same point: each pass would re-copy everything since the base and the
	// window would grow instead of shrinking, so the migration would never
	// converge and would look like a guest writing harder and harder.
	next, err := v.snapshotChangeID(ctx, snapRef, deviceKey)
	if err != nil {
		return nil, "", err
	}
	return out, next, nil
}

/*
snapshotChangeID reads the change marker VMware stamped on a disk when the

	snapshot was taken. It lives on the snapshot's copy of the hardware, not the
	VM's, which is the whole point: it names the exact instant this pass read.
*/
func (v *Client) snapshotChangeID(ctx context.Context, snapRef string, deviceKey int32) (string, error) {
	var m mo.VirtualMachineSnapshot
	pc := property.DefaultCollector(v.c.Client)
	ref := types.ManagedObjectReference{Type: "VirtualMachineSnapshot", Value: snapRef}
	if err := pc.RetrieveOne(ctx, ref, []string{"config.hardware.device"}, &m); err != nil {
		return "", fmt.Errorf("read the change marker from snapshot %s: %w", snapRef, err)
	}
	for _, d := range m.Config.Hardware.Device {
		disk, ok := d.(*types.VirtualDisk)
		if !ok || disk.Key != deviceKey {
			continue
		}
		if b, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok && b.ChangeId != "" {
			return b.ChangeId, nil
		}
		if b, ok := disk.Backing.(*types.VirtualDiskSeSparseBackingInfo); ok && b.ChangeId != "" {
			return b.ChangeId, nil
		}
		// A disk with no marker means CBT produced nothing to resume from. Say
		// so here rather than return an empty string, which the caller would
		// store and later read back as "*" — a silent full re-copy.
		return "", fmt.Errorf("disk %d carries no change marker in snapshot %s, so there is nothing for the next pass to resume from. "+
			"Changed block tracking has to be active at the moment the snapshot is taken", deviceKey, snapRef)
	}
	return "", fmt.Errorf("disk %d is not in snapshot %s — it was added or removed in VMware after this migration started", deviceKey, snapRef)
}

/*
explainCBTError names the one failure that matters and cannot be retried.

	A CBT reset — caused by a storage vMotion, a failed consolidation, or some
	power operations — invalidates the marker, and VMware then refuses the query
	rather than silently returning everything. That is the good outcome: the bad
	one would be copying the whole disk while telling the operator it is a
	delta. It is named here so the migration can fall back to a full re-copy
	knowingly.
*/
func explainCBTError(err error, changeID string) error {
	s := strings.ToLower(err.Error())
	// The reset is checked FIRST. Its message mentions change tracking too, and
	// classified as "not enabled" it would send the migration off to enable
	// something already enabled, then fail again the same way — where a reset is
	// recoverable on the spot by reading the disk in full.
	if strings.Contains(s, "reset") || strings.Contains(s, "invalid change") || strings.Contains(s, "changeid") {
		return fmt.Errorf("%w: the marker %q is no longer valid — VMware has reset change tracking, "+
			"usually after a storage vMotion or a snapshot consolidation, and the disk has to be read in full again: %w",
			ErrCBTReset, changeID, err)
	}
	/* Change tracking is not available on this disk.

	   VMware reports this two ways and the second is the one that cost a real
	   migration: a plain FileFault naming the VMDK, with nothing in it about
	   change tracking at all. Passed through it reads as a damaged disk —
	   "ServerFaultCode: Error caused by file /vmfs/volumes/…/BSL.vmdk" — when
	   the disk is perfectly healthy and the VM simply has CBT off.

	   Its own error because the remedy depends on the pass. A BASE copy does not
	   need change tracking: asking "*" is an optimisation that reads only the
	   allocated part of a thin disk, and reading the whole disk is the correct
	   answer when it is unavailable. A DELTA does need it, and there the same
	   condition is genuinely fatal. */
	if strings.Contains(s, "change tracking is not enabled") || strings.Contains(s, "changetracking") ||
		strings.Contains(s, "error caused by file") {
		return fmt.Errorf("%w: changed block tracking is not active on this VM. It takes effect at power-on, so a VM "+
			"that has not been restarted since it was enabled cannot be copied incrementally: %w", ErrCBTUnavailable, err)
	}
	return err
}

/*
ErrDiskLocked reports a disk file the host holds open and will not serve.

	Its own error because the remedy depends on WHO holds it, and only the caller
	knows: on a powered-off source a lock is somebody else's and worth chasing,
	and on a running one it is the guest's own and there is nothing to chase.
	NFC answers both with the same "NFC_FILE_LOCKED", so the distinction cannot
	be made here.
*/
var ErrDiskLocked = fmt.Errorf("the host will not serve the disk file")

// diskLocked carries a resolve failure that ended in a lock without changing a
// word of it. The message is already the full account of what was tried; this
// only lets a caller that knows the power state add the remedy.
type diskLocked struct{ err error }

func (e diskLocked) Error() string        { return e.err.Error() }
func (e diskLocked) Unwrap() error        { return e.err }
func (e diskLocked) Is(target error) bool { return target == ErrDiskLocked }

// poweredOnState is VMware's spelling of a running VM, as a plain string so the
// pass can test a power state without importing vim25 types.
const poweredOnState = string(types.VirtualMachinePowerStatePoweredOn)

/*
ErrCBTUnavailable reports a disk that cannot answer a change-tracking query.

	Distinct from ErrCBTReset: a reset had tracking and lost its marker, and this
	never had tracking at all. Both are recovered by reading in full, but only one
	of them is a surprise.
*/
var ErrCBTUnavailable = fmt.Errorf("change tracking is not available")

// ErrCBTReset reports a change marker VMware no longer recognises. Its own error
// because the remedy is specific and automatic: start again from a full read,
// and say so, rather than failing a migration that is still perfectly possible.
var ErrCBTReset = fmt.Errorf("change tracking was reset")

/*
PinReads routes datastore reads through the host that is running the VM.

	govmomi's own words for the alternative, on Datastore.ServiceTicket, are "An
	host is chosen at random". Where several hosts are attached to a datastore,
	vCenter may proxy a read to one that does not own the VM — and that host
	cannot take a lock on a running guest's files, so it answers NFC_FILE_LOCKED.
	Identical to a guest holding its own disk, from an entirely different cause,
	and the operator is told to stop a VM that never needed stopping.

	Only where there is a choice to make. One host attached means vCenter had
	nowhere else to send it, and pinning would swap a read through vCenter for a
	read straight at the ESXi host — so an agent that can reach vCenter but not
	the host would lose a path that works, to close a hazard that cannot occur.
	Pinning exactly when there is a decision is the point; doing it always is a
	different bug waiting on a different network.
*/
func (v *Client) PinReads(ctx context.Context, moRef, dsPath string) error {
	ds, _, err := v.datastoreFor(ctx, dsPath)
	if err != nil {
		return err
	}
	hosts, err := ds.AttachedHosts(ctx)
	if err != nil {
		return fmt.Errorf("vmware: read the hosts attached to %s: %w", dsPath, err)
	}
	if len(hosts) < 2 {
		return nil
	}

	var m mo.VirtualMachine
	pc := property.DefaultCollector(v.c.Client)
	if err := pc.RetrieveOne(ctx, v.vm(moRef).Reference(), []string{"runtime.host"}, &m); err != nil {
		return fmt.Errorf("vmware: find the host running %s: %w", moRef, err)
	}
	if m.Runtime.Host == nil {
		// Said rather than shrugged off: a VM with no host on a datastore shared
		// by several is precisely the case a random choice gets wrong.
		return fmt.Errorf("vmware: %s reports no host, and %s is shared by %d hosts — a read would be sent to one of them "+
			"at random, and any host but the VM's own will refuse a running guest's disk", moRef, dsPath, len(hosts))
	}
	v.pin = object.NewHostSystem(v.c.Client, *m.Runtime.Host)
	return nil
}

/*
ReadAt reads bytes from a disk file on the datastore.

	Over the datastore HTTP endpoint with a Range header, authenticated by the
	same session as everything else. This is what removes the need for VMware's
	VDDK — a C library under a click-through licence with no Go bindings — at
	the cost of reading the FLAT file rather than a consolidated view, which is
	why every pass consolidates its snapshot before the next one starts.
*/
func (v *Client) ReadAt(ctx context.Context, dsPath string, offset, length int64) (io.ReadCloser, error) {
	ds, path, err := v.datastoreFor(ctx, dsPath)
	if err != nil {
		return nil, err
	}
	u := ds.NewURL(path)
	p := soap.DefaultDownload
	p.Headers = map[string]string{"Range": rangeHeader(offset, length)}
	// Pinned, where PinReads found a choice worth making. The ticket is what
	// authenticates a read that no longer goes through vCenter.
	if v.pin != nil {
		tu, ticket, terr := ds.ServiceTicket(ds.HostContext(ctx, v.pin), path, p.Method)
		if terr != nil {
			return nil, fmt.Errorf("vmware: get a read ticket for %s on the host running it: %w", dsPath, terr)
		}
		u, p.Ticket = tu, ticket
	}
	/* DownloadRequest, not Download.

	   govmomi's Download accepts only 200 OK and turns anything else into an
	   error — including 206 Partial Content, which is the CORRECT answer to a
	   range request and the only answer a ranged read of a real disk ever gets.
	   Every read here is ranged, so this path could never have copied a byte.

	   It went unnoticed because the failures before it all happened earlier: a
	   523-byte descriptor fits entirely inside the requested range, so the
	   server answered 200 with the whole file and the read failed later, on
	   being short, rather than here on the status. */
	res, err := v.c.Client.DownloadRequest(ctx, u, &p)
	if err != nil {
		return nil, fmt.Errorf("vmware: read %s at %d+%d: %w", dsPath, offset, length, err)
	}
	if err := acceptRangedRead(res.StatusCode, res.Status, res.Body, offset); err != nil {
		res.Body.Close()
		return nil, fmt.Errorf("vmware: read %s at %d+%d: %w", dsPath, offset, length, err)
	}
	return res.Body, nil
}

/*
acceptRangedRead decides whether a response to a ranged GET can be trusted.

	206 is the answer that means "here is the range you asked for".

	200 means the server IGNORED the range and is sending the file from the
	beginning. At offset 0 that is harmless — the bytes start where they belong,
	and a body shorter than asked for is caught by the read itself. Anywhere else
	it is the worst outcome this code can produce: the start of the disk written
	over the middle of it, every chunk, with the copy reporting success and the
	VM booting into a subtly wrong disk. So it is refused, loudly, rather than
	read.
*/
func acceptRangedRead(code int, status string, body io.Reader, offset int64) error {
	switch code {
	case http.StatusPartialContent:
		return nil
	case http.StatusOK:
		if offset == 0 {
			return nil
		}
		return fmt.Errorf("the datastore answered %s instead of 206 Partial Content, which means it ignored the range and "+
			"is sending the file from the beginning. Copying that would write the start of the disk over the middle of it, "+
			"so the read is refused", status)
	default:
		/* The BODY, not just the status line.

		   A bare "500 Internal Server Error" is a fact about HTTP and says
		   nothing about the disk. ESXi puts the actual reason in the response —
		   the file is locked, the datastore cannot be read as files, the path
		   does not resolve — and throwing it away left the resolver guessing
		   from status codes and the operator reading the guess. */
		if reason := serverReason(body); reason != "" {
			return fmt.Errorf("the datastore answered %s: %s", status, reason)
		}
		return fmt.Errorf("the datastore answered %s", status)
	}
}

/*
serverReason pulls the human part out of an error response.

	ESXi answers a refused file read with an HTML page whose text is the reason.
	Bounded and flattened to one line, because this ends up in a job message an
	operator reads, and an unbounded body from a host that is already misbehaving
	is not something to paste whole. An empty result is normal and means the
	response carried nothing worth repeating.
*/
func serverReason(body io.Reader) string {
	if body == nil {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(body, 8<<10))
	if err != nil && len(raw) == 0 {
		return ""
	}
	text := tagText.ReplaceAllString(string(raw), " ")
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	return text
}

// tagText strips HTML markup so the sentence inside an ESXi error page survives
// without the page around it.
var tagText = regexp.MustCompile(`(?s)<[^>]*>`)

// rangeHeader builds an HTTP byte range. Inclusive at BOTH ends, which is the
// one thing to get right here: off by one re-reads or skips a byte on every
// chunk of every disk, and the result still looks like a completed copy.
func rangeHeader(offset, length int64) string {
	return fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
}

// datastoreFor turns "[datastore1] Web01/Web01.vmdk" into a datastore and a
// path within it.
func (v *Client) datastoreFor(ctx context.Context, dsPath string) (*object.Datastore, string, error) {
	var p object.DatastorePath
	if !p.FromString(dsPath) {
		return nil, "", fmt.Errorf("vmware: %q is not a datastore path", dsPath)
	}
	finder := find.NewFinder(v.c.Client, false)
	dc, err := finder.DefaultDatacenter(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("vmware: find datacenter: %w", err)
	}
	finder.SetDatacenter(dc)
	ds, err := finder.Datastore(ctx, p.Datastore)
	if err != nil {
		return nil, "", fmt.Errorf("vmware: find datastore %q: %w", p.Datastore, err)
	}
	return ds, p.Path, nil
}

/*
ResolveDiskFile finds the file that actually holds a disk's data.

	A VirtualDisk's backing names its DESCRIPTOR — "[datastore1] BSL/BSL.vmdk" —
	and on VMFS that is a few hundred bytes of text. The data is beside it in
	BSL-flat.vmdk. Reading the descriptor and expecting the disk is what produced

	  read [datastore1] BSL/BSL.vmdk at 0: the datastore returned less than the
	  33554432 bytes requested

	on the first real migration: 32MB asked for, a text file returned.

	ASKED, NOT DERIVED. Appending "-flat" is right for VMFS and wrong elsewhere —
	NFS keeps a single file, a snapshot delta is "-000001-delta.vmdk", SEsparse is
	"-sesparse.vmdk", and vSAN has no such file at all. So the datastore is asked
	how big each candidate is and the one that can actually hold the disk wins.
	Where nothing does, the failure lists what was found and how big it was,
	because that is a diagnosis an operator can act on rather than a mystery.
*/
func (v *Client) ResolveDiskFile(ctx context.Context, dsPath string, capacity int64) (string, error) {
	return resolveDiskFile(ctx, dsPath, capacity, v.probeRead, datastoreFacts{
		size: v.fileSize,
		kind: v.datastoreKind,
	})
}

/*
fileSize asks the datastore browser how big a file is.

	Only ever on a failure path, and only to say what was found. The browser
	cannot be trusted to CHOOSE the file — for a descriptor it answers with the
	virtual disk's capacity rather than the 523 bytes of text actually there,
	which is why the choice is made by reading. But once nothing could be read,
	"the browser lists this file at exactly the size of the disk" is the fact
	that separates a file that is missing from one that is being withheld.

	A question the browser cannot answer is reported as unanswered. An absent
	size is not a size of zero.
*/
func (v *Client) fileSize(ctx context.Context, dsPath string) (int64, bool) {
	ds, path, err := v.datastoreFor(ctx, dsPath)
	if err != nil {
		return 0, false
	}
	info, err := ds.Stat(ctx, path)
	if err != nil || info == nil {
		return 0, false
	}
	return info.GetFileInfo().FileSize, true
}

// datastoreKind reports the datastore's filesystem type — "VMFS", "vsan", "NFS".
// It is the difference between a disk that is held and one that was never
// readable as a file, and it is a fact rather than the inference the failure
// used to offer in its place.
func (v *Client) datastoreKind(ctx context.Context, dsPath string) string {
	ds, _, err := v.datastoreFor(ctx, dsPath)
	if err != nil {
		return ""
	}
	t, err := ds.Type(ctx)
	if err != nil {
		return ""
	}
	return string(t)
}

/*
probeRead reports how many bytes the datastore will actually serve.

	Deliberately the SAME channel the copy reads through. The first attempt at
	this asked the datastore browser for the file size instead, and the browser
	answers about the VIRTUAL DISK: for "BSL.vmdk" it reports the disk's full
	capacity, while an HTTP GET on that identical path returns 523 bytes of
	descriptor text. Both answers are true about different things, and picking
	the file on the strength of the one the copy does not use chose the
	descriptor every time.
*/
func (v *Client) probeRead(ctx context.Context, dsPath string, offset, length int64) (int64, error) {
	rc, err := v.ReadAt(ctx, dsPath, offset, length)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(rc, length))
	if err != nil {
		return n, err
	}
	return n, nil
}

// diskProbe reads a range and reports how many bytes came back.
type diskProbe func(ctx context.Context, dsPath string, offset, length int64) (int64, error)

/*
datastoreFacts are the two questions a FAILED resolve needs answered, and

	neither is used to pick the file.

	They exist because the failure this replaced ended in two guesses — that a
	500 meant somebody else held a lock, and that the datastore might be vSAN —
	in a product whose standing rule is that a diagnosis it could make and does
	not is a defect. Both are one call away. Either may be unanswerable, and an
	unanswered question is left out of the message rather than guessed at.
*/
type datastoreFacts struct {
	// size is what the datastore browser lists for a file, and whether it could
	// be asked at all.
	size func(ctx context.Context, dsPath string) (int64, bool)
	// kind is the filesystem type of the datastore holding dsPath, or "".
	kind func(ctx context.Context, dsPath string) string
}

/*
resolveDiskFile picks the candidate that can serve the END of the disk.

	The end, not the start: a descriptor is a valid file and will happily serve
	its first few hundred bytes, so a probe at offset 0 cannot tell it from the
	data. Only the file that actually holds the disk can return a full sector at
	capacity-1 sector, which makes this a question with one right answer.
*/
func resolveDiskFile(ctx context.Context, dsPath string, capacity int64, probe diskProbe, facts datastoreFacts) (string, error) {
	length := sectorSize
	if capacity < length {
		length = capacity
	}
	offset := capacity - length
	if offset < 0 || length <= 0 {
		return "", fmt.Errorf("vmware: %s reports a capacity of %d bytes, which is not a disk that can be copied", dsPath, capacity)
	}

	base := strings.TrimSuffix(dsPath, ".vmdk")
	candidates := []string{dsPath}
	for _, suffix := range []string{"-flat.vmdk", "-delta.vmdk", "-sesparse.vmdk"} {
		candidates = append(candidates, base+suffix)
	}

	var tried []string
	for _, cand := range candidates {
		n, err := probe(ctx, cand, offset, length)
		if err == nil && n == length {
			return cand, nil
		}
		var why string
		switch {
		case err != nil:
			why = fmt.Sprintf("could not be read: %v", err)
		default:
			why = fmt.Sprintf("served %d of %d bytes at offset %d", n, length, offset)
		}
		// The browser's answer beside the read's, because the pair is the
		// diagnosis: a file the browser cannot see is missing, and a file it
		// lists at the disk's full size that will not serve a sector is present
		// and withheld.
		if facts.size != nil {
			if sz, ok := facts.size(ctx, cand); ok {
				why = fmt.Sprintf("the datastore lists it at %d bytes; %s", sz, why)
			}
		}
		tried = append(tried, fmt.Sprintf("%s (%s)", cand, why))
	}
	/* 404 and 500 are different findings and the difference is the whole
	   diagnosis: 404 says the candidate is not there, which is ordinary — only
	   one of the four ever exists. 500 says the file IS there and the host would
	   not serve it, which on a flat disk means it is in use. Said out loud,
	   because the list of four candidates otherwise reads as "your disk is
	   missing" when the disk is present and simply held. */
	hint := ""
	if strings.Contains(strings.Join(tried, " "), "500 Internal Server Error") {
		hint = " One candidate answered 500, which means the file is there and the host would not serve it. The reason the " +
			"host gave is quoted above."
	}
	/* The datastore's type, asked rather than offered as a possibility.

	   This sentence used to read "a disk on vSAN … cannot be copied over the
	   datastore interface" on every failure, whatever the datastore actually
	   was — a maybe, printed where the operator needed a finding, and one call
	   away from being known. */
	where := " A disk on vSAN, or on a datastore this host cannot read as files, cannot be copied over the datastore interface"
	if facts.kind != nil {
		if k := facts.kind(ctx, dsPath); k != "" {
			where = fmt.Sprintf(" The datastore is %s.", k)
			if strings.EqualFold(k, "vsan") || strings.EqualFold(k, "vvol") {
				where = fmt.Sprintf(" The datastore is %s, which does not expose disks as files at all, so nothing here can "+
					"be copied over the datastore interface.", k)
			}
		}
	}
	err := fmt.Errorf("vmware: nothing beside %s can serve the end of a %d-byte disk, so there is no file here holding "+
		"its data. Tried: %s.%s%s", dsPath, capacity, strings.Join(tried, "; "), hint, where)
	/* A lock is marked, not explained, because the explanation needs the power
	   state and this does not have it. NFC's own word for it is the only
	   reliable marker: the surrounding text is ESXi's and may be reworded, but
	   NFC_FILE_LOCKED is the code. */
	if strings.Contains(strings.Join(tried, " "), "NFC_FILE_LOCKED") {
		return "", diskLocked{err}
	}
	return "", err
}

/*
PowerOff shuts the source down for a cutover.

	Graceful first, through VMware Tools, then a hard stop only after the guest
	has been given time. A migration cutover ends with the guest's filesystem
	being read one last time, and pulling the power on a Windows guest with
	writes in flight puts a dirty filesystem into the disk that is about to
	become production.
*/
func (v *Client) PowerOff(ctx context.Context, moRef string, graceful time.Duration) error {
	vm := v.vm(moRef)
	info, err := v.Inspect(ctx, moRef)
	if err != nil {
		return err
	}
	if info.PowerState != string(types.VirtualMachinePowerStatePoweredOn) {
		return nil
	}

	// Ask the guest. A VM with no Tools refuses this immediately, which is not
	// a failure — it just means the polite route is unavailable.
	if err := vm.ShutdownGuest(ctx); err == nil {
		deadline := time.Now().Add(graceful)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
			st, ierr := v.Inspect(ctx, moRef)
			if ierr == nil && st.PowerState != string(types.VirtualMachinePowerStatePoweredOn) {
				return nil
			}
		}
	}

	task, err := vm.PowerOff(ctx)
	if err != nil {
		return fmt.Errorf("vmware: power off %s: %w", info.Name, err)
	}
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("vmware: power off %s: %w", info.Name, err)
	}
	return nil
}

/*
consolidateWait is how long a pass waits for a snapshot to merge.

	Generous, because the alternative is worse in both directions: giving up
	early leaves a snapshot growing on the source, and the next pass then reads
	a base disk that is mid-merge. Ten minutes covers a normal consolidation on
	busy storage; beyond that something is genuinely wrong and saying so beats
	waiting silently.
*/
const consolidateWait = 10 * time.Minute
