package hyperv

import (
	"context"
	"strings"
	"testing"
)

func releaseScript(t *testing.T, deviceID string) string {
	t.Helper()
	f := &fakeRunner{responses: [][]byte{[]byte("RESULT released=0 nowPoolable=0 skipped=0 poolsRemoved=0")}}
	if _, err := newTestPS(f).ReleasePoolDisks(context.Background(), deviceID); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("expected one script call, got %d", len(f.calls))
	}
	return f.calls[0]
}

// The properties that make this safe to offer from the console at all. Each one
// is a way the job could destroy storage it was never meant to touch.
func TestReleasePoolDisksScriptGuards(t *testing.T) {
	script := releaseScript(t, "")
	for _, want := range []struct{ needle, why string }{
		{"Get-Cluster -ErrorAction SilentlyContinue", "must check whether this host is clustered"},
		{"refusing to release pool disks", "a cluster member must be refused with a reason"},
		{"Get-StorageNode", "disks must be resolved as local to THIS node"},
		{"-PhysicallyConnected", "only physically connected disks may be touched"},
		{"$disk.IsBoot -or $disk.IsSystem", "the OS/boot disk must be skipped"},
		{"'Spaces'", "Spaces virtual disks (CSVs) must be skipped"},
		{"Reset-PhysicalDisk", "the pool claim is only cleared by resetting the disk"},
		{"Remove-StoragePool", "the leftover pool holding the claim must be removed"},
		{"IsReadOnly $false", "an orphaned pool comes up read-only and refuses removal"},
	} {
		if !strings.Contains(script, want.needle) {
			t.Errorf("script missing %q — %s\n---\n%s", want.needle, want.why, script)
		}
	}
}

// Clearing partitions does not release a pool claim; it lives in the disk's pool
// metadata. A script that only formatted would look like it worked and leave the
// disk exactly as stuck as before.
func TestReleasePoolDisksDoesNotRelyOnClearDiskAlone(t *testing.T) {
	script := releaseScript(t, "")
	reset := strings.Index(script, "Reset-PhysicalDisk")
	clear := strings.Index(script, "Clear-Disk")
	if reset < 0 {
		t.Fatal("Reset-PhysicalDisk is what actually releases the claim and must be present")
	}
	if clear >= 0 && clear < reset {
		t.Error("Clear-Disk runs before Reset-PhysicalDisk; the claim must be released first")
	}
}

// A disk id is unique per bus, not per host, so an ambiguous id must be refused
// rather than resolved by guessing — the same rule FormatDisk already follows.
func TestReleasePoolDisksRefusesAnAmbiguousID(t *testing.T) {
	script := releaseScript(t, "1")
	if !strings.Contains(script, "identify the disk by its unique id instead") {
		t.Errorf("an id matching several disks must be refused, not guessed\n---\n%s", script)
	}
	if !strings.Contains(script, "$_.UniqueId -eq $target") {
		t.Errorf("the unique id must be the preferred match\n---\n%s", script)
	}
}

// Targeting one disk must not become "all of them" if the id fails to match.
func TestReleasePoolDisksTargetsOnlyTheNamedDisk(t *testing.T) {
	script := releaseScript(t, "abc123")
	if !strings.Contains(script, "$target = 'abc123'") {
		t.Errorf("the device id must be quoted into the script, got\n---\n%s", script)
	}
	if !strings.Contains(script, "no local physical disk matches") {
		t.Error("a named disk that matches nothing must be an error, not a whole-host wipe")
	}
}

// The summary has to distinguish "acted on" from "actually poolable again". A
// disk still claimed after the run is the outcome the operator most needs told.
func TestReleasePoolDisksReportsDisksStillClaimed(t *testing.T) {
	script := releaseScript(t, "")
	if !strings.Contains(script, "nowPoolable=") {
		t.Error("the summary must report how many disks came back poolable")
	}
	if !strings.Contains(script, "still claimed") {
		t.Error("disks that did not come back poolable must be called out, with the remedy")
	}
}
