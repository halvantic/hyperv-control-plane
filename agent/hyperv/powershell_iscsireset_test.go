package hyperv

import (
	"context"
	"strings"
	"testing"
)

/* Starting the initiator again from scratch.

   The reconcile is strictly additive and never prunes, so iSCSI state
   accumulates. In one day on one fleet (2026-08-23/24) that produced: favourite
   targets for a LUN the array had deleted, retried once a minute for ever and
   logged as a warning by the array every time; a discovery portal pinned to a
   source address the host no longer had, returning nothing and failing every
   login with a message that pointed at the array; and disk objects for LUNs no
   session could reach, counted as capacity.

   None of those is a fault the reconcile can fix, because fixing them means
   REMOVING things, and removing storage as a side effect of an edit is worse
   than leaving it. Hence a deliberate reset. */

func resetStub(t *testing.T, result string) *PowerShell {
	t.Helper()
	var p PowerShell
	p.run = func(_ context.Context, _ string) ([]byte, error) { return []byte(result), nil }
	return &p
}

func TestResetRefusesWhileDisksAreInUse(t *testing.T) {
	var p PowerShell
	var seen string
	p.run = func(_ context.Context, script string) ([]byte, error) {
		seen = script
		return []byte(`RESULT={}`), nil
	}
	_, _ = p.ResetISCSIInitiator(context.Background())

	if !strings.Contains(seen, "refusing to reset the iSCSI initiator") {
		t.Fatal("a reset must refuse while disks are carrying data")
	}
	// A clustered disk always blocks it.
	if !strings.Contains(seen, "is a clustered disk") {
		t.Error("a clustered disk must block it")
	}
	// ONLINE IS NOT THE TEST, and was: it refused the exact cleanup this exists
	// for. All three S2D members refused with "disk 1 is online" over a RAW disk
	// left by a torn-down pool — no partitions, no filesystem, nothing mounted.
	if !strings.Contains(seen, "if ([bool]$d.IsOffline) { continue }") {
		t.Error("an offline disk is nobody's concern")
	}
	if !strings.Contains(seen, "if ($parts.Count -eq 0) { continue }") {
		t.Fatal("an online disk carrying NO partition must not block a reset — that is the case this refused wrongly")
	}
	if !strings.Contains(seen, "is online and carries ") {
		t.Error("a disk that does carry partitions must say so, and how many")
	}
	// Where it is mounted is the part an operator acts on.
	if !strings.Contains(seen, "mounted at ") {
		t.Error("a mounted partition must name its drive letter or access path")
	}
	// Every partition names itself \?\Volume{...}; listing that would make
	// every disk look occupied.
	if !strings.Contains(seen, "$volGuidPrefix") {
		t.Error("the volume-GUID access path must be excluded, or nothing ever passes the check")
	}
	// Checked BEFORE anything changes, so a refusal leaves the host as it was.
	if strings.Index(seen, "refusing to reset") > strings.Index(seen, "Unregister-IscsiSession") {
		t.Error("the refusal must be decided before the first change, or a refused reset still half-ran")
	}
	// A refusal without a way forward is not an answer.
	if !strings.Contains(seen, "Take the volumes offline") {
		t.Error("the refusal must name what to do about it")
	}
}

// Order matters and is not arbitrary.
func TestResetClearsInTheRightOrder(t *testing.T) {
	var p PowerShell
	var seen string
	p.run = func(_ context.Context, script string) ([]byte, error) {
		seen = script
		return []byte(`RESULT={}`), nil
	}
	_, _ = p.ResetISCSIInitiator(context.Background())

	// The CALLS, not any mention of them. A bare Index matched a COMMENT naming
	// Remove-IscsiTargetPortal and reported the order wrong — the third assertion
	// in this package today to read prose instead of code.
	unreg := strings.Index(seen, "$s | Unregister-IscsiSession")
	disc := strings.Index(seen, "$s | Disconnect-IscsiTarget")
	portal := strings.Index(seen, "Remove-IscsiTargetPortal @rm")
	persist := strings.Index(seen, "RemovePersistentTarget $init")
	for n, i := range map[string]int{"unregister": unreg, "disconnect": disc, "portal removal": portal, "persistent removal": persist} {
		if i < 0 {
			t.Fatalf("no %s call found in the script", n)
		}
	}

	// Unregister before disconnect: a persistent session merely disconnected
	// comes back at the next boot, which is a fix that lasts until a restart.
	if unreg > disc {
		t.Error("a session must be unregistered before it is disconnected")
	}
	// Portals last: remove them first and the sessions have nothing naming them.
	if portal < persist || portal < disc {
		t.Error("discovery portals must be removed last")
	}
}

// Judged on what is LEFT, not on what was attempted — the same rule the S2D
// teardown had to learn the hard way when it reported destroying a pool it had
// never touched.
func TestResetFailsWhenSomethingSurvives(t *testing.T) {
	p := resetStub(t, `RESULT={"sessions":3,"persistent":3,"portals":3,"leftSessions":1,"leftPortals":0,"persistErrors":[],"portalErrors":[]}`)
	_, err := p.ResetISCSIInitiator(context.Background())
	if err == nil {
		t.Fatal("a surviving session must fail the job, not be rounded up to success")
	}
	if !strings.Contains(err.Error(), "PARTLY reset") || !strings.Contains(err.Error(), "1 sessions are still connected") {
		t.Errorf("it must say what survived: %v", err)
	}
}

func TestResetFailsWhenAPortalWillNotGo(t *testing.T) {
	p := resetStub(t, `RESULT={"sessions":0,"persistent":0,"portals":1,"leftSessions":0,"leftPortals":1,"persistErrors":[],"portalErrors":["10.0.60.52 - access denied"]}`)
	_, err := p.ResetISCSIInitiator(context.Background())
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("the portal's own reason must reach the operator: %v", err)
	}
}

func TestResetReportsWhatItRemovedAndWhatHappensNext(t *testing.T) {
	p := resetStub(t, `RESULT={"sessions":3,"persistent":6,"portals":3,"leftSessions":0,"leftPortals":0,"persistErrors":[],"portalErrors":[]}`)
	note, err := p.ResetISCSIInitiator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "removed 3 sessions, 6 persistent logins and 3 discovery portals") {
		t.Errorf("the counts must be reported: %q", note)
	}
	// The half that makes a reset safe to press: it is not a teardown, the
	// reconcile puts back whatever is still declared.
	if !strings.Contains(note, "re-registers whatever the storage spec still declares") {
		t.Errorf("it must say what happens next: %q", note)
	}
}

// A persistent login that survived is the one leftover that goes on costing
// something — the array logs a warning a minute — so it is named even when the
// reset otherwise worked.
func TestResetNamesAPersistentLoginItCouldNotRemove(t *testing.T) {
	p := resetStub(t, `RESULT={"sessions":1,"persistent":2,"portals":3,"leftSessions":0,"leftPortals":0,"persistErrors":["iqn.syn:dead via 10.0.60.52"],"portalErrors":[]}`)
	note, err := p.ResetISCSIInitiator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "1 persistent logins could not be removed") || !strings.Contains(note, "iqn.syn:dead") {
		t.Errorf("a surviving persistent login must be named: %q", note)
	}
	if !strings.Contains(note, "may keep retrying them") {
		t.Errorf("it must say what that leftover costs: %q", note)
	}
}

/*
The portal OBJECT is piped. The port is never named.

	Remove-IscsiTargetPortal has an InputObject parameter set fed straight from
	Get-IscsiTargetPortal, and Microsoft's example removes a portal without
	mentioning a port at all. -TargetPortalPortNumber failed with "Type mismatch
	for parameter" whatever was put in it — [int] first, then [uint16], which was
	a guess and the wrong way round: Remove declares Int32 while New declares
	UInt16. The two do not agree, and the port was never the fixable part.

	Same lesson as Remove-ClusterGroup -Name in RemoveReplicaBroker: with these
	CDXML cmdlets, pipe the object rather than rebuilding its key. Every member of
	Primary1 lost every session and kept every portal, twice, before this.
*/
func TestPortalRemovalUsesTheBindingThenThePipe(t *testing.T) {
	var p PowerShell
	var seen string
	p.run = func(_ context.Context, script string) ([]byte, error) { seen = script; return []byte(`RESULT={}`), nil }
	_, _ = p.ResetISCSIInitiator(context.Background())

	// The source binding is part of what identifies a portal. One registered with
	// -InitiatorPortalAddress is not found by target address alone: the removal
	// reports "The specified portal was not found" and the portal survives.
	if !strings.Contains(seen, "$rm['InitiatorPortalAddress'] = $ipa") {
		t.Fatalf("a bound portal must be removed by its binding:\n%s", seen)
	}
	if !strings.Contains(seen, "Remove-IscsiTargetPortal @rm -Confirm:$false -ErrorAction Stop") {
		t.Fatal("the removal must be splatted, so the binding is included or omitted per portal")
	}
	// Piping carries the CIM key itself, and stays as the fallback.
	if !strings.Contains(seen, "$pt | Remove-IscsiTargetPortal -Confirm:$false -ErrorAction Stop") {
		t.Error("the piped form must remain as a fallback")
	}
	// The parameter that could never bind must not appear on a removal at all:
	// Remove declares Int32 and New declares UInt16, and neither width worked.
	if strings.Contains(seen, "Remove-IscsiTargetPortal -TargetPortalAddress $pa -TargetPortalPortNumber") {
		t.Error("naming the port is what produced \"Type mismatch for parameter\"")
	}
	// What was tried reaches the operator: bound and unbound fail identically.
	if !strings.Contains(seen, "' (via ' + $ipa + ')'") || !strings.Contains(seen, "' (unbound)'") {
		t.Error("the failure must say whether the portal was bound, and to what")
	}
	// The FIRST reason is reported, not the fallback's — they are different faults.
	if !strings.Contains(seen, "+ ' - ' + $first)") {
		t.Error("the original reason must survive the fallback attempt")
	}
}

/*
A session that refuses to go used to leave no reason at all: both calls were

	SilentlyContinue inside a bare catch, so the job reported "2 sessions are
	still connected" and nothing about why. A teardown that cannot say what
	stopped it sends an operator to the one place this exists to keep them out
	of.
*/
func TestASessionThatWillNotDisconnectSaysWhy(t *testing.T) {
	var p PowerShell
	var seen string
	p.run = func(_ context.Context, script string) ([]byte, error) { seen = script; return []byte(`RESULT={}`), nil }
	_, _ = p.ResetISCSIInitiator(context.Background())

	if strings.Contains(seen, "Disconnect-IscsiTarget -NodeAddress $t -SessionIdentifier $s.SessionIdentifier -Confirm:$false -ErrorAction SilentlyContinue") {
		t.Fatal("the disconnect reason must not be swallowed")
	}
	if !strings.Contains(seen, "$sessionErr += ($t + ': '") {
		t.Errorf("a failed disconnect must record its reason:\n%s", seen)
	}
	// A session that was never persistent has nothing to unregister, and that is
	// not a failure worth reporting.
	// Includes "Failed to remove persistent login": an entry the initiator has
	// already dropped is nothing to do either, and reporting it buried the one
	// error that mattered under noise on HVNEW05.
	if !strings.Contains(seen, "$m -notmatch 'not persistent|does not exist|not found|Failed to remove persistent login'") {
		t.Error("an unregister that had nothing to do must not be reported as an error")
	}

	// And the reason must reach the operator, not just the script.
	p2 := resetStub(t, `RESULT={"sessions":2,"persistent":0,"portals":0,"leftSessions":2,"leftPortals":0,"persistErrors":[],"portalErrors":[],"sessionErrors":["iqn.syn:t2: The session is in use by a clustered disk"]}`)
	_, err := p2.ResetISCSIInitiator(context.Background())
	if err == nil {
		t.Fatal("surviving sessions must fail the job")
	}
	if !strings.Contains(err.Error(), "in use by a clustered disk") {
		t.Errorf("the reason must be in the message an operator reads: %v", err)
	}
}

/*
iSCSI target names are CASE-SENSITIVE, and the session reports them lowercased.

	Disconnect-IscsiTarget was given -NodeAddress $s.TargetNodeAddress.
	Get-IscsiSession echoes the name lowercased while the initiator holds the
	array's own capitalisation — "…:Xpenology.default-target…" against
	"…:xpenology.default-target…" — so per RFC 3720 the initiator found no such
	target and refused with "The parameter is incorrect", while the session it was
	looking at sat there connected. HVNEW04 and HVNEW05, 2026-08-25.

	This repo already recorded that these names are case-sensitive, for the LOGIN
	path. The disconnect kept rebuilding the key anyway.
*/
func TestTheDisconnectDoesNotRebuildTheTargetName(t *testing.T) {
	var p PowerShell
	var seen string
	p.run = func(_ context.Context, script string) ([]byte, error) { seen = script; return []byte(`RESULT={}`), nil }
	_, _ = p.ResetISCSIInitiator(context.Background())

	if strings.Contains(seen, "Disconnect-IscsiTarget -NodeAddress") {
		t.Fatal("naming the target reintroduces the case mismatch — pipe the session instead")
	}
	if !strings.Contains(seen, "$s | Disconnect-IscsiTarget -Confirm:$false -ErrorAction Stop") {
		t.Fatalf("the session object carries the identity the initiator assigned:\n%s", seen)
	}
	// Same for the unregister: a session identifier enumerated a moment ago can
	// already be gone, which is what "Invalid Session Id" was.
	if !strings.Contains(seen, "$s | Unregister-IscsiSession -ErrorAction Stop") {
		t.Error("the unregister must pipe the session too")
	}
}
