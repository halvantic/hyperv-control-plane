package hyperv

import (
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

/* Which interface a session actually leaves through.

   Unbound, the initiator asks the routing table, and the routing table answers
   with ONE interface. Three declared portals then produce three sessions out of
   one vNIC: MPIO reports "in effect", the console reports three paths, and a
   single cable carries all of them. Observed on the rig 2026-08-24 across three
   hosts. Only binding each session to the storage address on the portal's own
   subnet makes the paths distinct. */

func fannedSpec() types.ISCSIStorageSpec {
	return types.ISCSIStorageSpec{
		Portals: []string{"10.0.41.10", "10.0.42.10"},
		Targets: []string{"iqn.2000-01.com.synology:xpenology.Target-1"},
	}
}

func TestEachSessionIsBoundToTheStorageVNICOnItsSubnet(t *testing.T) {
	s := iscsiScript(fannedSpec(), true, true, []string{"10.0.41.11/24", "10.0.42.11/24"})

	if !strings.Contains(s, "$initiatorFor['10.0.41.10'] = '10.0.41.11'") ||
		!strings.Contains(s, "$initiatorFor['10.0.42.10'] = '10.0.42.11'") {
		t.Fatalf("each portal must be bound to the storage address on its own subnet:\n%s", s)
	}
	if !strings.Contains(s, "$c['InitiatorPortalAddress'] = $initiatorFor[$addr]") {
		t.Fatal("the login must actually use the binding")
	}
	// The portal is the key, so a portal with no storage vNIC on its subnet is
	// simply absent — and then nothing is bound rather than something guessed.
	if !strings.Contains(s, "if ($initiatorFor.ContainsKey($addr))") {
		t.Error("an unmatched portal must fall back to leaving the source unbound")
	}
}

// A guess here does not fail cleanly. Binding a session to the wrong source
// address fails at login with a message about the array, which is where the
// operator then goes looking.
func TestAPortalWithNoStorageVNICOnItsSubnetIsNotBound(t *testing.T) {
	s := iscsiScript(fannedSpec(), true, true, []string{"10.0.41.11/24"})
	if strings.Contains(s, "$initiatorFor['10.0.42.10']") {
		t.Fatalf("a portal no storage vNIC can reach must not be bound to one that cannot reach it:\n%s", s)
	}
	if !strings.Contains(s, "$initiatorFor['10.0.41.10'] = '10.0.41.11'") {
		t.Error("the portal that does match must still be bound")
	}
}

// Every host that predates storage vNICs declares none, and must behave exactly
// as it did.
func TestAHostWithNoStorageVNICsBindsNothing(t *testing.T) {
	s := iscsiScript(fannedSpec(), true, true, nil)
	if !strings.Contains(s, "$initiatorFor = @{}") {
		t.Fatal("the table must still exist, or the login script reads it as absent")
	}
	if strings.Contains(s, "$initiatorFor['") {
		t.Errorf("nothing declared means nothing bound:\n%s", s)
	}
}

/*
Discovery leaves through the storage vNIC on the portal's own subnet, exactly

	as the session does.

	It used to be left unbound, on the argument that a pinned portal keeps a
	source address the host may lose. That failure is real but already detected
	and repaired by RepairISCSIPortals; leaving it unbound has a failure of its
	own that nothing catches. On a host with two storage subnets the routing table
	picks ONE interface, so discovery to the second portal goes out the first
	vNIC, and the array answers about a target it does not advertise on that path:
	"the target name is not found or is marked as hidden from login".

	HVNEW01, 2026-08-24 — it failed on 10.0.61.52 while HVNEW03 succeeded, and
	setting Initiator IP by hand in iscsicpl fixed it at once.

	NOTE the earlier version of this test looked at the first line containing
	"New-IscsiTargetPortal", which is a COMMENT. It passed either way and proved
	nothing. Find the CALL.
*/
func portalCallLine(t *testing.T, script string) string {
	t.Helper()
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "New-IscsiTargetPortal -TargetPortalAddress") {
			return line
		}
	}
	t.Fatalf("no New-IscsiTargetPortal call in:\n%s", script)
	return ""
}

func TestDiscoveryPortalsAreBoundToTheirOwnSubnet(t *testing.T) {
	s := iscsiScript(fannedSpec(), true, true, []string{"10.0.41.11/24", "10.0.42.11/24"})

	call := portalCallLine(t, s)
	if !strings.Contains(call, "@pa") {
		t.Fatalf("the discovery portal must take the binding: %s", call)
	}
	if !strings.Contains(s, "if ($initiatorFor.ContainsKey($addr)) { $pa['InitiatorPortalAddress'] = $initiatorFor[$addr] }") {
		t.Errorf("the binding must come from the same per-portal table the login uses:\n%s", s)
	}
	// The table has to be built BEFORE the portal block, or it is empty there.
	if strings.Index(s, "$initiatorFor = @{}") > strings.Index(s, "New-IscsiTargetPortal -TargetPortalAddress") {
		t.Error("the table must be built before the portals are registered")
	}
	// No CHAP on the first attempt: discovery and target login authenticate
	// independently, and an array that puts CHAP on the target refuses a
	// credential it never asked for. See TestISCSIDiscoveryIsUnauthenticatedFirst.
	if strings.Contains(call, "@portalAuthFallback") {
		t.Errorf("the first discovery attempt must carry no credential: %s", call)
	}
}

// A portal no storage vNIC can reach binds to nothing rather than to a guess.
func TestAnUnmatchedDiscoveryPortalIsNotBound(t *testing.T) {
	s := iscsiScript(fannedSpec(), true, true, nil)
	if strings.Contains(s, "$initiatorFor['") {
		t.Fatalf("nothing declared means nothing bound:\n%s", s)
	}
	// The guard still has to be present, so the script is valid with an empty map.
	if !strings.Contains(s, "$pa = @{}") {
		t.Error("the splat must exist even when nothing is bound")
	}
}

// The spec allows a port on a portal and an IP parser does not.
func TestAPortalWithAPortStillBinds(t *testing.T) {
	spec := fannedSpec()
	spec.Portals = []string{"10.0.41.10:3260"}
	s := iscsiScript(spec, false, true, []string{"10.0.41.11/24"})
	if !strings.Contains(s, "$initiatorFor['10.0.41.10'] = '10.0.41.11'") {
		t.Fatalf("the key must be the address the login loop uses, without the port:\n%s", s)
	}
}

/*
A session that is up but NOT persistent vanishes at the next reboot and takes

	the node's disks with it. Register-IscsiSession's failure was swallowed by a
	bare catch, so the console said "Not persistent — this node loses its disks on
	the next restart" and could not say why or what to do. HVNEW03, 2026-08-24.
*/
func TestAFailureToMakeASessionPersistentKeepsItsReason(t *testing.T) {
	s := iscsiScript(fannedSpec(), true, true, nil)

	if strings.Contains(s, "-ErrorAction Stop; $changed = $true } catch {}") {
		t.Fatal("the reason must not be swallowed by a bare catch")
	}
	if !strings.Contains(s, "$persistErrs += ") {
		t.Fatalf("the registration failure must be collected:\n%s", s)
	}
	// Reported even when the logins all worked — otherwise it is invisible until
	// the reboot that costs the node its storage.
	if !strings.Contains(s, "will not be restored at boot") {
		t.Error("the state must say what it costs")
	}
	if !strings.Contains(s, "if ($out.message) { $out.message = $out.message + '. ' + $note }") {
		t.Error("it must not overwrite a login failure already being reported")
	}
}

/* The empty-discovery diagnosis has to tell the truth about CHAP.

   It branches on $usedChap, and NOTHING EVER SET IT. An undefined variable is
   $null in PowerShell, which is falsy — so every host on every pass was told
   "this spec sets NO CHAP credential", whether one was declared or not.
   Primary1 declared one throughout and all three members were sent to add what
   was already there (2026-08-24). The true branch had never been reached.

   A diagnosis that states a fact about the spec must read the spec. */
func TestTheCHAPDiagnosisReflectsTheSpec(t *testing.T) {
	withChap := fannedSpec()
	withChap.CredentialSecret = "ballast"
	s := iscsiScript(withChap, true, true, nil)

	if !strings.Contains(s, "$usedChap = $true") {
		t.Fatalf("a spec WITH a credential must say so:\n%s", s)
	}
	if strings.Contains(s, "$usedChap = $false") {
		t.Error("both branches must not be emitted")
	}

	s = iscsiScript(fannedSpec(), true, true, nil)
	if !strings.Contains(s, "$usedChap = $false") {
		t.Fatalf("a spec with no credential must say so:\n%s", s)
	}

	// And it must be SET before it is read, or it is $null either way.
	set := strings.Index(s, "$usedChap = ")
	read := strings.Index(s, "$(if ($usedChap)")
	if set < 0 || read < 0 || set > read {
		t.Fatal("$usedChap must be assigned before the diagnosis reads it")
	}
}

/* Making an existing session persistent carries the credential.

   Register-IscsiSession takes -ChapUsername and -ChapSecret and writes the
   persistent entry with whatever it is handed. Called without them it writes an
   EMPTY secret, and Windows checks that against its own rule: "Target CHAP
   secret given is invalid. Maximum size of CHAP secret is 16 bytes. Minimum size
   is 12 bytes if IPSec is not used." The complaint is about the nothing we
   passed, and it reads exactly like a bad credential.

   HVNEW01 and HVNEW02 failed here on 2026-08-25 with a credential that logs in
   perfectly. HVNEW03 was clean for the reason that proves the diagnosis: its
   session had been made FRESH by Connect-IscsiTarget, which carries
   IsPersistent and CHAP together and never needs this call. */
func TestMakingASessionPersistentCarriesTheCredential(t *testing.T) {
	spec := fannedSpec()
	spec.CredentialSecret = "nas-chap"
	s := iscsiScript(spec, true, true, nil)

	if !strings.Contains(s, "$sessionChap = @{ ChapUsername = $env:BALLAST_CHAP_USER; ChapSecret = $env:BALLAST_CHAP_SECRET }") {
		t.Fatalf("the credential must be available to the registration:\n%s", s)
	}
	if !strings.Contains(s, "$reg += $sessionChap") || !strings.Contains(s, "Register-IscsiSession @reg -ErrorAction Stop") {
		t.Fatalf("the registration must actually carry it:\n%s", s)
	}
	// Bare, it writes an empty secret and fails its own length check.
	if strings.Contains(s, "Register-IscsiSession -SessionIdentifier $s.SessionIdentifier -ErrorAction Stop") {
		t.Error("registering without the credential is what produced the bogus length error")
	}
	// And registering must not silently change what the session IS.
	if !strings.Contains(s, "if ($s.IsMultipathEnabled) { $reg['IsMultipathEnabled'] = $true }") {
		t.Error("the session's multipath setting must be preserved across registration")
	}

	// An open target passes nothing, or the empty-secret failure comes back by
	// another door.
	if !strings.Contains(iscsiScript(fannedSpec(), true, true, nil), "$sessionChap = @{}") {
		t.Error("with no credential the registration must carry none")
	}
}

/* A slow iSCSI step has to name the cmdlet, not just the step.

   The pass timer reported "clusterReconcile 4m15s (iscsi 4m3s)" on both members
   of Secondary, 2026-08-25. Enough to know the cluster never formed because the
   pass was cut off at five minutes; not enough to know which call blocked.
   Several of these cmdlets hang for minutes against a portal that answers on
   3260 but does not complete the operation, and the TCP probe cannot tell those
   apart. Guessing which one has been wrong repeatedly, so the script times
   itself. */
func TestASlowISCSIStepNamesItsPhases(t *testing.T) {
	got := slowISCSIPhases(map[string]int{"portals": 243000, "logins": 4000, "report": 200}, 247000)
	if got == "" {
		t.Fatal("a four-minute step must be explained")
	}
	if !strings.Contains(got, "portals 243s") {
		t.Errorf("the slow phase must be named: %q", got)
	}
	// Ordered slowest first, and stable — a message that reorders between passes
	// reads as a new one.
	if strings.Index(got, "portals") > strings.Index(got, "logins") {
		t.Error("phases must be ordered slowest first")
	}
	// Sub-second phases are noise.
	if strings.Contains(got, "report") {
		t.Error("a phase under a second must not be listed")
	}
	if !strings.Contains(got, "why anything after it may not have run") {
		t.Error("it must say what a slow step costs, not just that it was slow")
	}
}

// On a healthy host this is noise, and a message that always carries timings is
// one nobody reads.
func TestAFastISCSIStepSaysNothing(t *testing.T) {
	if got := slowISCSIPhases(map[string]int{"portals": 900, "logins": 400}, 1400); got != "" {
		t.Fatalf("a fast pass must not be explained: %q", got)
	}
	if got := slowISCSIPhases(nil, 90000); got != "" {
		t.Error("no phases reported means nothing to say — an older agent must not produce an empty list")
	}
}

// And the script must actually record them, or the report above has nothing.
func TestTheScriptTimesItsOwnPhases(t *testing.T) {
	s := iscsiScript(fannedSpec(), true, true, nil)
	for _, want := range []string{"$phases = [ordered]@{}", "Mark-Phase 'portals'", "Mark-Phase 'logins'", "$out.phases = $phases", "$out.elapsedMs"} {
		if !strings.Contains(s, want) {
			t.Errorf("the script must record %q", want)
		}
	}
}
