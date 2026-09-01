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
