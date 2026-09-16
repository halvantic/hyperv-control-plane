package reconcile

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* Reporting the VMs that are on this host's storage but registered nowhere.

   The scan is a JOB and the reporting is part of every pass, and the split is
   the whole design. Compare-VM loads each configuration it finds, so the cost
   scales with how much data somebody else left on the volume — the one thing in
   a reconcile pass that Ballast does not control. On Secondary a single
   unbounded step ate 3m43s of a 5m pass and everything after it never ran; that
   is the failure this avoids by construction rather than by tuning.

   So: an operator asks, the agent scans once and remembers, and every pass
   afterwards reports what was found together with WHEN it was found. The second
   half is not decoration. A list of VMs with no age on it is a stale reading
   presented as current fact, which is the bug class this repository pays for
   most often. */

// scanImportableVMs runs the walk and stores the result for the reporting path.
// A single path scans just that volume; empty scans every root the host has.
func (r *Reconciler) scanImportableVMs(ctx context.Context, path string) (string, error) {
	roots := r.importRoots()
	if p := strings.TrimSpace(path); p != "" {
		// A single volume, but only one the host actually reports. Scanning an
		// arbitrary path sent in a job would let the console point the agent at
		// anything on the box, and "walk this path" is not a capability this
		// feature needs.
		matched := ""
		for _, root := range roots {
			if strings.EqualFold(strings.TrimRight(root, `\/`), strings.TrimRight(p, `\/`)) {
				matched = root
				break
			}
		}
		if matched == "" {
			return "", fmt.Errorf("scan %s: this host does not report a volume at that path. It scans the cluster shared volumes and data drives it can see; a path outside those is not something this job will walk", p)
		}
		roots = []string{matched}
	}

	vms, st, err := r.hv.ScanImportableVMs(ctx, roots)
	if err != nil {
		return "", err
	}
	r.setImportScan(vms, st)

	// The message says what was looked at as well as what was found. "No VMs
	// found" without the roots is unfalsifiable — it reads the same whether the
	// volume is empty or the scan looked in the wrong place.
	where := "no storage"
	if st != nil && len(st.Roots) > 0 {
		where = strings.Join(st.Roots, ", ")
	}
	if st != nil && st.Message != "" && len(vms) == 0 {
		return "", fmt.Errorf("scan of %s did not complete: %s", where, st.Message)
	}
	switch {
	case len(vms) == 0:
		return "scanned " + where + " and found no virtual machines that are not already registered", nil
	case st != nil && st.Truncated:
		return fmt.Sprintf("scanned %s and found %d unregistered virtual machine(s), and stopped at the cap — there may be more", where, len(vms)), nil
	default:
		return fmt.Sprintf("scanned %s and found %d unregistered virtual machine(s)", where, len(vms)), nil
	}
}

// importRoots is the storage worth walking, from what the host last reported.
func (r *Reconciler) importRoots() []string {
	r.importScanMu.Lock()
	defer r.importScanMu.Unlock()
	return append([]string(nil), r.importRootsCache...)
}

// rememberImportRoots records the volumes the host reported, so a scan job knows
// where to look without being told by the caller. Called from the host pass.
func (r *Reconciler) RememberImportRoots(res types.HostResources) {
	roots := types.ImportScanRoots(res)
	r.importScanMu.Lock()
	defer r.importScanMu.Unlock()
	r.importRootsCache = roots
}

func (r *Reconciler) setImportScan(vms []types.ImportableVM, st *types.ImportScanStatus) {
	r.importScanMu.Lock()
	defer r.importScanMu.Unlock()
	r.importVMs = vms
	r.importScan = st
}

/*
forgetImportable drops a configuration that has just been imported.

	Without this the console goes on offering Import for a VM that now exists,
	until the next scan — and the second import is refused, correctly, with a
	message about two claims on one set of disks. Correct refusals for actions
	the console should not have offered still read as the product being broken.
*/
func (r *Reconciler) forgetImportable(configPath string) {
	r.importScanMu.Lock()
	defer r.importScanMu.Unlock()
	out := r.importVMs[:0]
	for _, v := range r.importVMs {
		if !strings.EqualFold(v.ConfigPath, configPath) {
			out = append(out, v)
		}
	}
	r.importVMs = out
}

/*
importStatus is what the pass reports.

	Returns the scan record even when the list is empty, and that is the point:
	"this volume holds no unregistered VMs" and "nobody has looked" are the same
	empty list and have opposite meanings to somebody deciding whether a LUN they
	just adopted is safe to reformat.
*/
func (r *Reconciler) ImportStatus() ([]types.ImportableVM, *types.ImportScanStatus) {
	r.importScanMu.Lock()
	defer r.importScanMu.Unlock()
	if r.importScan == nil {
		return nil, nil
	}
	vms := append([]types.ImportableVM(nil), r.importVMs...)
	st := *r.importScan
	return vms, &st
}

// importScanStale reports a scan old enough that the console should say so
// rather than present it as current. Storage changes when somebody adopts a LUN
// or imports a VM, neither of which the agent is told about, so age is the only
// honest signal available.
const importScanStaleAfter = 30 * time.Minute

// StaleImportScan reports whether a scan record is old enough to need saying so.
func StaleImportScan(st *types.ImportScanStatus, now time.Time) bool {
	if st == nil || st.ScannedAt.IsZero() {
		return false
	}
	return now.Sub(st.ScannedAt) > importScanStaleAfter
}
