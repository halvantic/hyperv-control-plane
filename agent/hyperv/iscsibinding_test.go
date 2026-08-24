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
	// CHAP still goes with it; discovery needs the credential too.
	if !strings.Contains(call, "@portalAuth") {
		t.Errorf("the discovery credential must survive: %s", call)
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
