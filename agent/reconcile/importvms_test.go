package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/joshua-fourie/ballast/agent/hyperv"
	"github.com/joshua-fourie/ballast/api/types"
)

/* The scan/report split, and the two places it can lie.

   A scan that never ran and a scan that found nothing are the same empty list.
   A scan taken an hour ago and one taken a second ago are the same list too.
   Both are the stale-reading-as-current-fact bug, and both are load-bearing
   here: an operator reads this page to decide whether a LUN they just adopted
   still holds somebody's VMs before they reformat it. */

type importStubHV struct {
	hyperv.Interface
	roots    []string
	vms      []types.ImportableVM
	scan     *types.ImportScanStatus
	err      error
	imported []hyperv.VMImport
	importFn func(hyperv.VMImport) (string, error)
}

func (s *importStubHV) ScanImportableVMs(_ context.Context, roots []string) ([]types.ImportableVM, *types.ImportScanStatus, error) {
	s.roots = roots
	return s.vms, s.scan, s.err
}

func (s *importStubHV) ImportVM(_ context.Context, v hyperv.VMImport) (string, error) {
	s.imported = append(s.imported, v)
	if s.importFn != nil {
		return s.importFn(v)
	}
	return "imported " + v.ConfigPath, nil
}

func recWithRoots(hv hyperv.Interface, roots ...string) *Reconciler {
	r := testReconciler(hv)
	r.importRootsCache = roots
	return r
}

func TestNothingIsReportedUntilAScanHasRun(t *testing.T) {
	r := recWithRoots(&importStubHV{}, `C:\ClusterStorage\DS1`)
	vms, st := r.ImportStatus()
	if vms != nil || st != nil {
		t.Fatalf("a host nobody has scanned reported %d VMs and scan %+v; an empty list with no record reads as an empty volume", len(vms), st)
	}
}

func TestAScanThatFoundNothingIsNotTheSameAsNoScan(t *testing.T) {
	hv := &importStubHV{scan: &types.ImportScanStatus{Roots: []string{`C:\ClusterStorage\DS1`}, ScannedAt: time.Now()}}
	r := recWithRoots(hv, `C:\ClusterStorage\DS1`)

	if _, err := r.scanImportableVMs(context.Background(), ""); err != nil {
		t.Fatalf("scan: %v", err)
	}
	vms, st := r.ImportStatus()
	if len(vms) != 0 {
		t.Fatalf("expected no VMs, got %d", len(vms))
	}
	// The RECORD is what makes the empty list mean something.
	if st == nil || len(st.Roots) == 0 {
		t.Fatal("a completed scan that found nothing left no record, so the console cannot tell it from a host nobody looked at")
	}
}

func TestScanWalksTheHostsOwnRoots(t *testing.T) {
	hv := &importStubHV{scan: &types.ImportScanStatus{}}
	r := recWithRoots(hv, `C:\ClusterStorage\DS1`, `D:\`)
	if _, err := r.scanImportableVMs(context.Background(), ""); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(hv.roots) != 2 {
		t.Fatalf("scanned %q, want both roots", hv.roots)
	}
}

func TestScanningOneVolumeScansOnlyThatOne(t *testing.T) {
	hv := &importStubHV{scan: &types.ImportScanStatus{}}
	r := recWithRoots(hv, `C:\ClusterStorage\DS1`, `C:\ClusterStorage\DS2`)
	if _, err := r.scanImportableVMs(context.Background(), `C:\ClusterStorage\DS2`); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(hv.roots) != 1 || hv.roots[0] != `C:\ClusterStorage\DS2` {
		t.Fatalf("scanned %q, want only DS2", hv.roots)
	}
}

/*
"Walk this path" is not a capability this feature needs, and a job that

	accepted one would let anything that can enqueue a job walk the whole box.
*/
func TestScanRefusesAPathTheHostDoesNotReport(t *testing.T) {
	hv := &importStubHV{scan: &types.ImportScanStatus{}}
	r := recWithRoots(hv, `C:\ClusterStorage\DS1`)
	_, err := r.scanImportableVMs(context.Background(), `C:\Windows\System32`)
	if err == nil {
		t.Fatal("the scan accepted an arbitrary path")
	}
	if hv.roots != nil {
		t.Fatalf("it walked %q anyway", hv.roots)
	}
}

/*
A scan that FAILED must not report as a scan that found nothing. This is the

	single most important assertion in the file: the answer decides whether an
	operator reformats a volume.
*/
func TestAFailedScanIsAnError(t *testing.T) {
	hv := &importStubHV{scan: &types.ImportScanStatus{
		Roots:   []string{`C:\ClusterStorage\DS1`},
		Message: "the scan did not complete: context deadline exceeded",
	}}
	r := recWithRoots(hv, `C:\ClusterStorage\DS1`)
	msg, err := r.scanImportableVMs(context.Background(), "")
	if err == nil {
		t.Fatalf("a scan that did not complete reported success: %q", msg)
	}
	if !strings.Contains(err.Error(), "did not complete") {
		t.Errorf("the failure did not say the scan was incomplete: %v", err)
	}
}

func TestScanSaysWhereItLooked(t *testing.T) {
	hv := &importStubHV{scan: &types.ImportScanStatus{Roots: []string{`C:\ClusterStorage\DS1`}}}
	r := recWithRoots(hv, `C:\ClusterStorage\DS1`)
	msg, err := r.scanImportableVMs(context.Background(), "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// "No VMs found" without the roots is unfalsifiable — it reads the same
	// whether the volume is empty or the scan looked somewhere else entirely.
	if !strings.Contains(msg, `C:\ClusterStorage\DS1`) {
		t.Errorf("the result does not say where it looked: %q", msg)
	}
}

func TestTruncationIsReportedRatherThanImplied(t *testing.T) {
	hv := &importStubHV{
		vms:  []types.ImportableVM{{Name: "DC01"}},
		scan: &types.ImportScanStatus{Roots: []string{`D:\`}, Truncated: true},
	}
	r := recWithRoots(hv, `D:\`)
	msg, err := r.scanImportableVMs(context.Background(), "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !strings.Contains(msg, "there may be more") {
		t.Errorf("a capped scan presented its count as complete: %q", msg)
	}
}

/*
An imported VM must stop being offered immediately. Leaving it means the

	console offers Import for a VM that now exists, and the second attempt is
	refused — correctly — with a message about corrupting disks. Correct refusals
	for actions the console should not have offered still read as breakage.
*/
func TestAnImportedVMStopsBeingOffered(t *testing.T) {
	path := `C:\ClusterStorage\DS1\DC01\Virtual Machines\a.vmcx`
	hv := &importStubHV{
		vms: []types.ImportableVM{
			{Name: "DC01", ConfigPath: path},
			{Name: "FS01", ConfigPath: `C:\ClusterStorage\DS1\FS01\Virtual Machines\b.vmcx`},
		},
		scan: &types.ImportScanStatus{Roots: []string{`C:\ClusterStorage\DS1`}},
	}
	r := recWithRoots(hv, `C:\ClusterStorage\DS1`)
	if _, err := r.scanImportableVMs(context.Background(), ""); err != nil {
		t.Fatalf("scan: %v", err)
	}

	out, err := r.ExecuteJob(context.Background(), types.Job{
		Kind:   types.JobImportVM,
		Params: map[string]string{"path": path, "mode": "register"},
	}, nil)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if out == "" {
		t.Error("the import reported nothing")
	}
	vms, _ := r.ImportStatus()
	if len(vms) != 1 || vms[0].Name != "FS01" {
		t.Fatalf("after importing DC01 the host still offers %+v", vms)
	}
}

/*
A FAILED import must leave the VM on the list. Dropping it would hide a VM

	that is still sitting on the storage unimported — the exact thing the feature
	exists to surface.
*/
func TestAFailedImportLeavesTheVMOffered(t *testing.T) {
	path := `D:\DC01\Virtual Machines\a.vmcx`
	hv := &importStubHV{
		vms:      []types.ImportableVM{{Name: "DC01", ConfigPath: path}},
		scan:     &types.ImportScanStatus{Roots: []string{`D:\`}},
		importFn: func(hyperv.VMImport) (string, error) { return "", errors.New("a virtual disk is open") },
	}
	r := recWithRoots(hv, `D:\`)
	if _, err := r.scanImportableVMs(context.Background(), ""); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if _, err := r.ExecuteJob(context.Background(), types.Job{
		Kind:   types.JobImportVM,
		Params: map[string]string{"path": path, "mode": "register"},
	}, nil); err == nil {
		t.Fatal("the import reported success")
	}
	vms, _ := r.ImportStatus()
	if len(vms) != 1 {
		t.Fatalf("a failed import removed the VM from the list: %+v", vms)
	}
}

/*
Register and copy fail in opposite, expensive directions, so an unset mode is

	a refusal rather than a default.
*/
func TestImportRefusesAnUnsetMode(t *testing.T) {
	hv := &importStubHV{}
	r := recWithRoots(hv, `D:\`)
	for _, mode := range []string{"", "REGISTERR", "yes"} {
		_, err := r.ExecuteJob(context.Background(), types.Job{
			Kind:   types.JobImportVM,
			Params: map[string]string{"path": `D:\a\Virtual Machines\a.vmcx`, "mode": mode},
		}, nil)
		if err == nil {
			t.Errorf("mode %q was accepted", mode)
		}
	}
	if len(hv.imported) != 0 {
		t.Fatalf("the host was asked to import anyway: %+v", hv.imported)
	}
}

func TestImportRefusesAnEmptyPath(t *testing.T) {
	hv := &importStubHV{}
	r := recWithRoots(hv, `D:\`)
	if _, err := r.ExecuteJob(context.Background(), types.Job{
		Kind:   types.JobImportVM,
		Params: map[string]string{"mode": "register"},
	}, nil); err == nil {
		t.Fatal("the import accepted an empty path")
	}
}

func TestImportPassesTheOperatorsChoicesThrough(t *testing.T) {
	hv := &importStubHV{}
	r := recWithRoots(hv, `D:\`)
	if _, err := r.ExecuteJob(context.Background(), types.Job{
		Kind: types.JobImportVM,
		Params: map[string]string{
			"path": `D:\a\Virtual Machines\a.vmcx`, "mode": "copy",
			"cluster": "true", "discardSavedState": "true",
		},
	}, nil); err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(hv.imported) != 1 {
		t.Fatalf("expected one import, got %d", len(hv.imported))
	}
	got := hv.imported[0]
	if !got.Copy || !got.Cluster || !got.DiscardSavedState {
		t.Fatalf("the operator's choices did not reach the host: %+v", got)
	}
}

func TestRootsComeFromWhatTheHostReported(t *testing.T) {
	r := testReconciler(&importStubHV{})
	r.RememberImportRoots(types.HostResources{Volumes: []types.StorageVolume{
		{Path: `C:\`}, {Path: `C:\ClusterStorage\DS1`}, {Path: `D:\`},
	}})
	got := r.importRoots()
	if len(got) != 2 {
		t.Fatalf("roots = %q, want the CSV and the data drive without the system volume", got)
	}
}

func TestStaleScanIsCallable(t *testing.T) {
	now := time.Now()
	if StaleImportScan(nil, now) {
		t.Error("a missing scan reported as stale; it is absent, which is a different thing")
	}
	if StaleImportScan(&types.ImportScanStatus{}, now) {
		t.Error("a scan with no timestamp reported as stale rather than as never run")
	}
	if StaleImportScan(&types.ImportScanStatus{ScannedAt: now.Add(-time.Minute)}, now) {
		t.Error("a scan from a minute ago reported as stale")
	}
	if !StaleImportScan(&types.ImportScanStatus{ScannedAt: now.Add(-2 * time.Hour)}, now) {
		t.Error("a two-hour-old scan did not report as stale")
	}
}
