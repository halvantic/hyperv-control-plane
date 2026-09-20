package hyperv

import (
	"context"
	"strings"
	"testing"
)

/*
Clearing the discovery portals, which an operator had to do in iscsicpl.

	Windows keeps its own discovered-target database, and a stale entry makes
	Connect-IscsiTarget refuse with "The target name is not found or is marked as
	hidden from login" — a message that reads like the array refusing the node and
	is not. On the rig, 2026-09-01, two of three nodes were on one path each with
	exactly that; deleting every discovery portal by hand and letting the reconcile
	re-add them fixed it outright, and the array was never touched.

	Repair portals could not do it: it deliberately touches only portals bound to
	an address the host no longer has, which is right for something the reconcile
	runs and wrong as the only thing an operator can reach for.
*/
func rediscoverScript(t *testing.T) string {
	t.Helper()
	var script string
	p := &PowerShell{}
	p.run = func(_ context.Context, s string) ([]byte, error) {
		script = s
		return []byte("RESULT=cleared 2 discovery portal(s)"), nil
	}
	if _, err := p.RediscoverISCSI(context.Background()); err != nil {
		t.Fatalf("rediscover: %v", err)
	}
	return script
}

func TestRediscoverClearsEveryPortalNotOnlyThePinnedOnes(t *testing.T) {
	s := rediscoverScript(t)
	if !strings.Contains(s, "foreach ($h in $before)") {
		t.Fatalf("it does not walk every portal:\n%s", s)
	}
	// The narrow repair's test — leave a portal bound to a present address alone
	// — must NOT hold here. That exclusion is exactly what stopped it helping.
	if strings.Contains(s, "$myIPs -contains $bound") {
		t.Errorf("it inherited the narrow repair's exclusion and cannot clear a healthy-looking portal:\n%s", s)
	}
}

/*
Sessions are left alone, which is what makes this safe to offer.

	Removing a discovery portal does not drop a session, so the node keeps its
	disks throughout and the worst case is that the reconcile re-adds exactly what
	was there. Disconnecting would make this ResetISCSIInitiator, which is a
	different and much larger decision.
*/
func TestRediscoverDoesNotTouchSessionsOrPersistentLogins(t *testing.T) {
	s := rediscoverScript(t)
	for _, forbidden := range []string{"Disconnect-IscsiTarget", "Unregister-IscsiSession", "iscsicli"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("it drops sessions or persistent logins (%s), which is the reset's job:\n%s", forbidden, s)
		}
	}
	// And it says how many survived, so the operator can see nothing was lost.
	if !strings.Contains(s, "existing session(s) left connected") {
		t.Errorf("it does not report that sessions survived:\n%s", s)
	}
}

/*
Removing nothing at all is a failure, not a success — reporting a no-op as a

	repair is the bug this session found in RemoveCSV.

	The rule that enforces it changed, and is stronger for it. It was "no removal
	call succeeded"; it is now "the portals are still there afterwards". The old
	one reported Failed on HVNEW04 and HVNEW05 for portals that ended in exactly
	the wanted state, because the cmdlet complained on its way to doing what was
	asked. A cmdlet's verdict is not the end state, and the end state is the
	question an operator actually has.
*/
func TestRediscoverFailsWhenThePortalsAreStillThere(t *testing.T) {
	s := rediscoverScript(t)
	if !strings.Contains(s, "$after = @(Get-IscsiTargetPortal") {
		t.Fatalf("it does not re-read the portals, so it cannot know whether they went:\n%s", s)
	}
	if !strings.Contains(s, "if ($stillThere.Count -gt 0) {") || !strings.Contains(s, "still on this host after being asked to go") {
		t.Fatalf("a portal that survived would be reported as a repair:\n%s", s)
	}
	// And a removal that complained but WORKED is not a failure. That is the
	// case that was being reported wrongly.
	if !strings.Contains(s, "they are gone, though the removal reported") {
		t.Errorf("a portal that went despite an error is not reported as gone:\n%s", s)
	}
	// No portals at all is a different answer again, and says so rather than
	// throwing: there is genuinely nothing to clear.
	if !strings.Contains(s, "there are no discovery portals on this host to clear") {
		t.Errorf("a host with no portals is not distinguished from a failure:\n%s", s)
	}
}

/*
Re-queries each portal immediately before removing it, rather than removing
the object enumerated at the top of the script.

	$before is a snapshot. The reconcile loop runs on its own timer and, for a
	portal still declared in the spec — the normal case for something an
	operator reaches for Rediscover on — can re-add it in the gap between
	enumeration and removal. Remove-IscsiTargetPortal is a CDXML cmdlet: it
	re-resolves the piped object's own key against current WMI state rather
	than acting on the instance held in memory, so a stale $h fails with "The
	specified portal was not found" even though the address is still there a
	moment later. HVNEW01 and HVNEW02, 2026-09-17: iscsicpl succeeded by hand
	on the same portals because a human's timing does not collide with
	reconcile the way a scripted run reliably does.
*/
func TestRediscoverRequeriesBeforeRemoving(t *testing.T) {
	s := rediscoverScript(t)
	if !strings.Contains(s, "$fresh = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue | Where-Object {") {
		t.Fatalf("it does not re-query the portal before removing it:\n%s", s)
	}
	// Matched on source binding as well as address: two entries can share a
	// TargetPortalAddress with different InitiatorPortalAddress bindings, and
	// re-querying by address alone risks removing the wrong one.
	if !strings.Contains(s, "[string]$_.InitiatorPortalAddress -eq $ipa") {
		t.Errorf("the re-query does not account for the source binding:\n%s", s)
	}
}

// Piped, never named: -TargetPortalPortNumber fails with "Type mismatch for
// parameter" on this cmdlet whatever is put in it.
func TestRediscoverPipesThePortalObject(t *testing.T) {
	s := rediscoverScript(t)
	if !strings.Contains(s, "$h | Remove-IscsiTargetPortal -Confirm:$false") {
		t.Fatalf("the portal is not piped:\n%s", s)
	}
	if strings.Contains(s, "Remove-IscsiTargetPortal -TargetPortalAddress") {
		t.Errorf("the portal is named rather than piped, which fails on this cmdlet:\n%s", s)
	}
}
