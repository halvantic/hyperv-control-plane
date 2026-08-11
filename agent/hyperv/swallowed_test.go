package hyperv

import (
	"strings"
	"testing"
)

/* A swallowed error is only a bug when it makes a FAILURE indistinguishable from
   an ABSENCE. These scripts are full of tolerated failures that are correct — a
   host with no cluster, no array, no S2D answers nothing to most of these reads,
   and treating that as an error would make every standalone host look broken.

   The ones that matter are where the swallowed read feeds a DECISION. Three cost
   a diagnosis in one evening: an adoption that judged an offline disk's contents
   and offered to wipe them, a resource lookup that quietly found nothing and
   reported success, and a membership read that returned empty on failure. These
   tests pin the ones where the consequence is destructive. */

// The worst shape found: "if (read succeeded) { safety check; destroy }". A read
// that fails skips the block — including the check — and carries on.
func TestFormatDiskRefusesADiskItCannotRead(t *testing.T) {
	script := formatDiskScriptForTest()

	if !strings.Contains(script, "Refusing to erase a disk that cannot be read") {
		t.Fatal("a physical disk that resolves to no disk object must be refused, not skipped past")
	}
	// The boot guard must not sit inside a block that an unreadable disk skips.
	guard := strings.Index(script, "refusing to format the OS/boot disk")
	unreadable := strings.Index(script, "Refusing to erase a disk that cannot be read")
	if guard < 0 || unreadable < 0 || unreadable > guard {
		t.Error("the unreadable-disk refusal must come before the OS/boot guard, so the guard is always reached")
	}
}

// Once a disk IS being erased, the steps that prepare it must not fail quietly:
// a wipe against a disk that could not be brought online or made writable is a
// wipe nobody checked the state of.
func TestFormatDiskDoesNotPrepareTheDiskSilently(t *testing.T) {
	script := formatDiskScriptForTest()
	for _, line := range strings.Split(script, "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "#") {
			continue
		}
		if (strings.Contains(code, "Set-Disk") || strings.Contains(code, "Clear-Disk")) &&
			strings.Contains(code, "SilentlyContinue") {
			t.Errorf("a destructive or preparatory step must not swallow its failure: %q", code)
		}
	}
}
