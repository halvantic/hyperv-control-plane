package hyperv

import (
	"context"
	"strings"
	"testing"
)

/* A removal that removed nothing and said it had.

   RemoveCSV was one line of S2D: find a Storage Spaces virtual disk of that
   name, Remove-VirtualDisk it, and treat "no such virtual disk" as nothing to
   do. On S2D that reasoning holds. On an iSCSI-backed cluster there IS no
   virtual disk — the CSV sits on a LUN from an array — so it found nothing,
   returned NOOP, and the job reported "removed volume DS1".

   Twice on Primary1, 2026-09-01, against a CSV the cluster was reporting Online
   and 1 TB in a reading twenty-four seconds old. The script had always
   distinguished NOOP from REMOVED; nothing read the result. */
func TestRemoveCSVAsksTheClusterBeforeConcludingThereIsNothing(t *testing.T) {
	var script string
	p := &PowerShell{}
	p.run = func(_ context.Context, s string) ([]byte, error) {
		script = s
		return []byte("RESULT=removed volume 'DS1' and the storage behind it"), nil
	}
	if _, err := p.RemoveCSV(context.Background(), "DS1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(script, "Get-ClusterSharedVolume") {
		t.Fatalf("a missing virtual disk is still taken to mean the volume is gone:\n%s", script)
	}
	// The S2D path is unchanged: there, the virtual disk IS the storage.
	if !strings.Contains(script, "Remove-VirtualDisk -Confirm:$false") {
		t.Errorf("the S2D path is gone:\n%s", script)
	}
	if !strings.Contains(script, "Remove-ClusterSharedVolume -InputObject $csv") {
		t.Errorf("a CSV on array storage cannot be taken out of the cluster:\n%s", script)
	}
}

/* Remove-ClusterSharedVolume demotes a CSV, it does not evict it. Left there
   the disk resource stays claimed by the cluster for ever under the same
   name — a permanent orphan, indistinguishable from storage an operator
   deliberately wants kept in reserve. Confirmed live on WLGDC, 2026-09-18:
   after a "removal", the LUN was still listed as attached to the cluster
   in Failover Cluster Manager. */
func TestRemoveCSVEvictsTheDiskResourceLeftBehind(t *testing.T) {
	var script string
	p := &PowerShell{}
	p.run = func(_ context.Context, s string) ([]byte, error) {
		script = s
		return []byte("RESULT=took 'DS1' out of the cluster entirely. The disk behind it (C:\\ClusterStorage\\DS1) still holds its data."), nil
	}
	if _, err := p.RemoveCSV(context.Background(), "DS1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(script, "Stop-ClusterResource -InputObject $res") {
		t.Errorf("the leftover disk resource is not stopped before removal:\n%s", script)
	}
	if !strings.Contains(script, "Remove-ClusterResource -InputObject $res -Force") {
		t.Errorf("the leftover disk resource is not evicted:\n%s", script)
	}
	// Re-fetched by name filter, never -Name — Remove-ClusterSharedVolume just
	// changed this resource's own type, and this build does not always find
	// one by -Name (see powershell_volumeonline.go, powershell_iscsiadopt.go).
	if strings.Contains(script, "Get-ClusterResource -Name") {
		t.Errorf("the resource is looked up by -Name, which this build does not always find:\n%s", script)
	}
}

// A resource that resists eviction is named, not swallowed — CLAUDE.md's rule
// against a known failure with no remedy applies here just as much as it does
// to the LUN's own data never being touched.
func TestRemoveCSVReportsWhenTheDiskResourceCouldNotBeEvicted(t *testing.T) {
	p := &PowerShell{}
	p.run = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("RESULT=took 'DS1' out of the cluster as a shared volume, but could not evict the disk resource left behind (Access is denied). " +
			"It is now Available Storage rather than a shared volume, still claimed by the cluster. " +
			"The disk behind it (C:\\ClusterStorage\\DS1) still holds its data either way. Ballast does not administer the array that serves this LUN, so deleting it has to be done there."), nil
	}
	msg, err := p.RemoveCSV(context.Background(), "DS1")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	for _, want := range []string{"could not evict the disk resource", "Access is denied", "still claimed by the cluster"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the result does not say %q:\n%s", want, msg)
		}
	}
}

// Nothing found is reported as nothing found. Reporting it as a removal is the
// entire bug.
func TestRemoveCSVFailsWhenThereIsNothingToRemove(t *testing.T) {
	p := &PowerShell{}
	p.run = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("RESULT=NOTHING there is no virtual disk or cluster shared volume called 'DS1' on this cluster"), nil
	}
	msg, err := p.RemoveCSV(context.Background(), "DS1")
	if err == nil {
		t.Fatalf("a no-op was reported as success: %q", msg)
	}
	if !strings.Contains(err.Error(), "no virtual disk or cluster shared volume") {
		t.Errorf("the failure does not say what was looked for: %v", err)
	}
	// And it says the host is untouched, so a retry is obviously safe.
	if !strings.Contains(err.Error(), "Nothing has been changed") {
		t.Errorf("the failure does not say the cluster is unchanged: %v", err)
	}
}

/* The half Ballast does not own is named rather than silently skipped.

   Taking the CSV out of the cluster is Ballast's domain. The LUN behind it is on
   an array it has no presence on, and CLAUDE.md is explicit that where an action
   lies outside that boundary the console says so plainly and names the one step,
   rather than leaving an operator to discover that the disk is still full. */
func TestRemoveCSVSaysWhatItCouldNotDo(t *testing.T) {
	p := &PowerShell{}
	p.run = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("RESULT=took 'DS1' out of the cluster. It is now Available Storage rather than a shared volume, and " +
			"the disk behind it (C:\\ClusterStorage\\DS1) still holds its data. Ballast does not administer the array that " +
			"serves this LUN, so deleting it has to be done there."), nil
	}
	msg, err := p.RemoveCSV(context.Background(), "DS1")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	for _, want := range []string{"out of the cluster", "still holds its data", "has to be done there"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the result does not say %q:\n%s", want, msg)
		}
	}
}
