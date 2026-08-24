package hyperv

import (
	"context"
	"strings"
	"testing"
)

/* Removing a discovery portal an operator added by mistake.

   The iSCSI reconcile is strictly additive, deliberately — it cannot tell a
   portal somebody retired from one something else on the host depends on. So a
   wrong portal stays for ever. Both members of Primary1 carried 10.0.60.53 and
   10.0.60.54 from a config three edits old on 2026-08-24: they inflated the path
   count the console reported and kept advertising a target from a deleted
   cluster.

   Until now the only ways out were iscsicpl on the host, or ResetISCSIInitiator,
   which is nuclear AND refuses while disks carry partitions — exactly when an
   operator needs this. Opening a PowerShell session on a host to fix a managed
   object is a defect, not a runbook step. */

func prunePS(t *testing.T, result string) (*PowerShell, *string) {
	t.Helper()
	var seen string
	var p PowerShell
	p.run = func(_ context.Context, script string) ([]byte, error) {
		seen = script
		return []byte(result), nil
	}
	return &p, &seen
}

// Declaring nothing would mean removing everything, which is a different job.
func TestPruningWithNothingDeclaredIsRefused(t *testing.T) {
	p, seen := prunePS(t, `RESULT={}`)
	_, err := p.PruneISCSIPortals(context.Background(), nil)
	if err == nil {
		t.Fatal("pruning against an empty declared list must be refused")
	}
	if !strings.Contains(err.Error(), "which is what Reset initiator is for") {
		t.Errorf("the refusal must name the job that does do that: %v", err)
	}
	if *seen != "" {
		t.Error("nothing must run on the host when the request is refused")
	}
}

/* The safety rule. An undeclared portal CARRYING A SESSION is either a spec
   missing a portal the host really uses, or a session nobody declared — both
   for a person to resolve. Guessing either way drops a live storage path. */
func TestAnUndeclaredPortalCarryingASessionIsRefusedNotRemoved(t *testing.T) {
	p, seen := prunePS(t, `RESULT={"removed":["10.0.60.54:3260"],"kept":["10.0.60.53:3260"],"failed":[],"left":["10.0.60.52"]}`)
	note, err := p.PruneISCSIPortals(context.Background(), []string{"10.0.60.52", "10.0.61.52"})
	if err != nil {
		t.Fatal(err)
	}

	// The script must decide this on the host, from the sessions' own
	// connections — a session does not record the portal it was made through.
	if !strings.Contains(*seen, "Get-IscsiConnection") || !strings.Contains(*seen, "if ($busy -contains $addr)") {
		t.Fatalf("a portal carrying a session must be identified and skipped:\n%s", *seen)
	}
	if !strings.Contains(note, "removed 1 undeclared discovery portal(s): 10.0.60.54:3260") {
		t.Errorf("it must say what went: %q", note)
	}
	// Reported, not hidden: the one left behind is what the operator most needs
	// to think about.
	if !strings.Contains(note, "10.0.60.53:3260 were left") || !strings.Contains(note, "would drop a storage path") {
		t.Errorf("a portal left behind must be named, with why: %q", note)
	}
	if !strings.Contains(note, "add them to the spec or disconnect the target first") {
		t.Errorf("it must say what to do about it: %q", note)
	}
}

// A declared portal is never a candidate, whatever else is true of it.
func TestDeclaredPortalsAreNeverTouched(t *testing.T) {
	p, seen := prunePS(t, `RESULT={"removed":[],"kept":[],"failed":[],"left":["10.0.60.52"]}`)
	note, err := p.PruneISCSIPortals(context.Background(), []string{"10.0.60.52", "10.0.61.52:3260"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*seen, "if ($declared -contains $addr) { continue }") {
		t.Fatal("a declared portal must be skipped outright")
	}
	// The port is stripped, because the comparison is against the ADDRESS the
	// host reports and a declared portal may carry ":3260".
	if !strings.Contains(*seen, "$declared = @('10.0.60.52','10.0.61.52')") {
		t.Errorf("declared portals must be compared without their port:\n%s", *seen)
	}
	if !strings.Contains(note, "nothing to prune") {
		t.Errorf("a host with nothing undeclared must say so plainly: %q", note)
	}
}

func TestAPortalThatWillNotGoIsAFailure(t *testing.T) {
	p, _ := prunePS(t, `RESULT={"removed":[],"kept":[],"failed":["10.0.60.53:3260 - access denied"],"left":[]}`)
	_, err := p.PruneISCSIPortals(context.Background(), []string{"10.0.60.52"})
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("the portal's own reason must reach the operator: %v", err)
	}
}

// Discovery re-runs so the result is visible at once rather than next pass.
func TestPruningRefreshesDiscovery(t *testing.T) {
	p, seen := prunePS(t, `RESULT={"removed":[],"kept":[],"failed":[],"left":[]}`)
	_, _ = p.PruneISCSIPortals(context.Background(), []string{"10.0.60.52"})
	if !strings.Contains(*seen, "Update-IscsiTarget") {
		t.Error("discovery must be refreshed after a prune")
	}
}
