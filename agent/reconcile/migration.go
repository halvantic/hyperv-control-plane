package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/halvantic/hyperv-control-plane/agent/hyperv"
	"github.com/halvantic/hyperv-control-plane/agent/vmware"
	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* The agent's side of a VMware migration.

   The centre decides; this moves bytes. One job is one pass, and the result
   goes back two ways: a sentence in the job message for the operator, and a
   MigrationPassResult in host status for the centre — because the centre has to
   resume the next pass from the per-disk change markers, and parsing numbers
   back out of a sentence written for a person is how a pass ends up resuming
   from a marker nobody checked.

   Credentials arrive ON THE JOB and are not kept. A vCenter credential cached
   on a Hyper-V host outlives the operator's intent for it, and the host has no
   business being able to reach vCenter when nothing is migrating. */

// migrationEndpoint reads the source connection off a job's parameters.
func migrationEndpoint(p map[string]string) (vmware.Endpoint, error) {
	e := vmware.Endpoint{
		Address:     strings.TrimSpace(p["address"]),
		Username:    p["username"],
		Password:    p["password"],
		InsecureTLS: p["insecure"] == "true",
	}
	switch {
	case e.Address == "":
		return e, errors.New("no vCenter or ESXi address was sent with this job")
	case e.Username == "" || e.Password == "":
		// Named separately from the address: an operator whose migration source
		// lost its credential should be told that, not told to check a hostname
		// that is perfectly correct.
		return e, fmt.Errorf("no credential was sent for %s. The migration source's credential is resolved by the centre "+
			"when the job is created, so this usually means the secret it names has been deleted", e.Address)
	}
	return e, nil
}

/*
migrationPass runs one pass: base copy, delta, or the final one after the

	source is stopped.

	The result is recorded in status BEFORE any error is returned. A pass that
	copied three disks of four and then failed still moved those three, and their
	markers are the difference between the retry copying 40GB and copying 1.5TB.
*/
func (r *Reconciler) migrationPass(ctx context.Context, job types.Job, onProgress hyperv.ProgressFunc) (string, error) {
	p := job.Params
	name := strings.TrimSpace(p["migration"])
	if name == "" {
		return "", errors.New("migration pass: the job does not say which migration it belongs to")
	}
	ep, err := migrationEndpoint(p)
	if err != nil {
		return "", fmt.Errorf("migration %s: %w", name, err)
	}
	moRef := strings.TrimSpace(p["moRef"])
	if moRef == "" {
		return "", fmt.Errorf("migration %s: no source VM reference was sent. The reference is VMware's own identity for "+
			"the VM and is the only thing safe to find it by — a name can be changed under us and two VMs can share one", name)
	}
	dest := strings.TrimSpace(p["destDir"])
	if dest == "" {
		return "", fmt.Errorf("migration %s: no destination path was sent, so there is nowhere to write the disks", name)
	}

	markers, err := parseMarkers(p["markers"])
	if err != nil {
		return "", fmt.Errorf("migration %s: %w", name, err)
	}

	prov, err := r.diskProvisioner()
	if err != nil {
		return "", err
	}

	req := vmware.PassRequest{
		Endpoint:  ep,
		Migration: name,
		MoRef:     moRef,
		DestDir:   dest,
		Marker:    markers,
		Final:     p["final"] == "true",
	}
	if s := strings.TrimSpace(p["gracefulOffSeconds"]); s != "" {
		if n, cerr := strconv.Atoi(s); cerr == nil && n > 0 {
			req.GracefulOff = time.Duration(n) * time.Second
		}
	}

	started := time.Now()
	/* Live progress, reported two ways.

	   The sentence goes to the job message, as it always did. The per-disk bytes
	   go into the status journal as an IN-PROGRESS pass result, because a base
	   copy of half a terabyte runs for hours and until this existed the console
	   said "nothing copied yet" for the whole of it — the numbers only existed
	   once the pass had finished.

	   Throttled, because the copy calls back on every chunk. Host status is
	   reported on a heartbeat, so anything faster than the heartbeat is work
	   nobody sees. */
	var lastReport time.Time
	out, perr := vmware.RunPass(ctx, prov, req, func(pp vmware.PassProgress) {
		if onProgress != nil && pp.Note != "" {
			onProgress(pp.Note)
		}
		if len(pp.Disks) == 0 || time.Since(lastReport) < migrationProgressEvery {
			return
		}
		lastReport = time.Now()
		live := types.MigrationPassResult{
			Migration: name, JobID: job.ID, Final: req.Final,
			InProgress: true, StartedAt: started,
		}
		for _, d := range pp.Disks {
			// NextChangeID is deliberately NOT carried while the pass is running.
			// The marker for the next pass is not known until this one has read
			// to the end, and a partial one would resume from a point never
			// reached — copying nothing and reporting a delta.
			live.Disks = append(live.Disks, types.MigrationPassDisk{
				Key: d.Key, Label: d.Label, SourcePath: d.SourcePath, DestPath: d.DestPath,
				SizeBytes: d.SizeBytes, CopiedBytes: d.CopiedBytes,
			})
			live.CopiedBytes += d.CopiedBytes
		}
		r.recordMigrationPass(live)
	})

	res := types.MigrationPassResult{
		Migration:        name,
		JobID:            job.ID,
		Final:            req.Final,
		SourcePoweredOff: out.SourcePoweredOff,
		// Carried to the centre, not just written into the job message: the
		// centre is what decides whether another pass is worth running, and a
		// disk read in full has no marker for a delta to resume from.
		FullRead:    out.FullRead,
		CopiedBytes: out.CopiedBytes,
		StartedAt:   started,
		FinishedAt:  time.Now(),
	}
	for _, d := range out.Disks {
		res.Disks = append(res.Disks, types.MigrationPassDisk{
			Key:          d.Key,
			Label:        d.Label,
			SourcePath:   d.SourcePath,
			DestPath:     d.DestPath,
			SizeBytes:    d.SizeBytes,
			CopiedBytes:  d.CopiedBytes,
			NextChangeID: d.NextChangeID,
		})
	}
	if perr != nil {
		res.Error = perr.Error()
		res.CBTReset = errors.Is(perr, vmware.ErrCBTReset)
	}
	r.recordMigrationPass(res)

	if perr != nil {
		return "", perr
	}

	what := "delta pass"
	switch {
	case req.Final:
		what = "final pass, source powered off"
	case len(markers) == 0:
		what = "base copy"
	}
	msg := fmt.Sprintf("%s: %s across %d disk(s)", what, humanBytes(out.CopiedBytes), len(out.Disks))
	/* Said in the message, because it changes what the next step costs.

	   A disk read end to end for the want of change tracking transfers the whole
	   volume rather than only what is allocated, and there is no marker for a
	   delta to resume from. An operator sizing the next window needs that, and
	   the byte count alone does not carry it — a thick disk and a full read of a
	   thin one look identical. */
	if out.FullRead {
		msg += ". This VM has no change tracking, so the disks were read in full rather than only where data is"
	}
	return msg, nil
}

// migrationEnableCBT turns Changed Block Tracking on for the source VM.
func (r *Reconciler) migrationEnableCBT(ctx context.Context, p map[string]string) (string, error) {
	ep, err := migrationEndpoint(p)
	if err != nil {
		return "", err
	}
	moRef := strings.TrimSpace(p["moRef"])
	if moRef == "" {
		return "", errors.New("enable change block tracking: no source VM reference was sent")
	}
	c, err := vmware.Connect(ctx, ep)
	if err != nil {
		return "", err
	}
	defer c.Close(context.WithoutCancel(ctx))

	if err := c.EnableCBT(ctx, moRef); err != nil {
		return "", err
	}
	return "changed block tracking is on, so later passes copy only what changed", nil
}

/*
migrationCleanup removes the snapshot Ballast left on the source.

	Runs on the way out of every terminal phase, failure included. It also
	tolerates finding nothing: on a cleanup path the caller does not know how far
	a failed attempt got, and reporting "there was no snapshot" as a failure would
	leave an operator chasing a problem that has already resolved itself.
*/
func (r *Reconciler) migrationCleanup(ctx context.Context, p map[string]string) (string, error) {
	ep, err := migrationEndpoint(p)
	if err != nil {
		return "", err
	}
	moRef := strings.TrimSpace(p["moRef"])
	name := strings.TrimSpace(p["migration"])
	if moRef == "" || name == "" {
		return "", errors.New("migration cleanup: the job must name both the source VM and the migration, or there is no way to know which snapshot is Ballast's")
	}
	c, err := vmware.Connect(ctx, ep)
	if err != nil {
		return "", err
	}
	defer c.Close(context.WithoutCancel(ctx))

	// By name here, not by reference: cleanup runs after failures where the
	// reference was never recorded, and the name is what Ballast can always
	// reconstruct. It is Ballast's own name, carrying the migration's identity,
	// so it cannot match a snapshot somebody else took.
	if err := c.RemoveSnapshot(ctx, moRef, vmware.SnapshotName(name)); err != nil {
		return "", err
	}
	r.forgetMigrationPass(name)
	return "the source VM has no Ballast snapshot left on it", nil
}

/*
parseMarkers reads the per-disk change markers the previous pass reported.

	Malformed is a refusal, not an empty map. An unreadable marker treated as
	"none" silently turns a delta into a full re-copy — which finishes, looks
	right, and costs hours nobody accounted for.
*/
func parseMarkers(s string) (map[int32]string, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" {
		return nil, nil
	}
	var raw map[string]string
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("the change markers from the last pass could not be read (%w), and treating them as absent "+
			"would silently re-copy every disk in full", err)
	}
	out := make(map[int32]string, len(raw))
	for k, v := range raw {
		n, err := strconv.Atoi(k)
		if err != nil {
			return nil, fmt.Errorf("%q is not a disk key in the change markers from the last pass", k)
		}
		out[int32(n)] = v
	}
	return out, nil
}

/*
diskProvisioner is the host's ability to create and attach destination disks.

	A type assertion rather than another dozen methods on hyperv.Interface: this
	needs a real PowerShell host and nothing else can stand in for one. The
	refusal says so plainly instead of failing later with a nil dereference.
*/
func (r *Reconciler) diskProvisioner() (vmware.Provisioner, error) {
	ps, ok := r.hv.(*hyperv.PowerShell)
	if !ok {
		return nil, errors.New("this agent cannot create destination disks: migration needs a real Hyper-V host, and this one is running a stand-in implementation")
	}
	return vmware.HostDisks{PS: ps}, nil
}

// migrationProgressEvery throttles in-flight progress reports. The copy calls
// back on every chunk; host status goes out on a heartbeat, so anything faster
// than that is work nobody ever sees.
const migrationProgressEvery = 5 * time.Second

/*
recordMigrationPass stores the pass so the next status report carries it.

	Keyed by migration: only the LAST pass for each is kept. A host does not
	carry the history of every VM it has pulled off VMware — the Migration object
	at the centre is where that lives — and an unbounded list on a host that has
	migrated two hundred VMs would be reported in full on every heartbeat.
*/
func (r *Reconciler) recordMigrationPass(res types.MigrationPassResult) {
	r.migrationMu.Lock()
	defer r.migrationMu.Unlock()
	for i, existing := range r.migrationPasses {
		if existing.Migration == res.Migration {
			r.migrationPasses[i] = res
			return
		}
	}
	r.migrationPasses = append(r.migrationPasses, res)
}

// forgetMigrationPass drops a finished migration's result. Cleanup is the last
// thing every migration does, successful or not, so it is the right place: the
// host stops reporting a migration nothing is doing any more.
func (r *Reconciler) forgetMigrationPass(name string) {
	r.migrationMu.Lock()
	defer r.migrationMu.Unlock()
	out := r.migrationPasses[:0]
	for _, v := range r.migrationPasses {
		if v.Migration != name {
			out = append(out, v)
		}
	}
	r.migrationPasses = out
}

// MigrationPasses is what the pass reports to the centre.
func (r *Reconciler) MigrationPasses() []types.MigrationPassResult {
	r.migrationMu.Lock()
	defer r.migrationMu.Unlock()
	if len(r.migrationPasses) == 0 {
		return nil
	}
	return append([]types.MigrationPassResult(nil), r.migrationPasses...)
}

// humanBytes renders a size the way the console does, so a job message and the
// migration table agree on what was copied.
func humanBytes(n int64) string {
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
