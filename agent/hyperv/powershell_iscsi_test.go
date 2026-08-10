package hyperv

import (
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

func iscsiSpec() types.ISCSIStorageSpec {
	return types.ISCSIStorageSpec{
		Portals: []string{"192.168.1.168:3260"},
		Targets: []string{"iqn.2000-01.com.synology:Xpenology.default-target.aa584d33bc2"},
	}
}

// A non-persistent session works perfectly until the node reboots and then
// simply does not come back — on a cluster member that means its disks do not
// arrive and the roles it owned fail over, with nothing saying why. So logins
// are persistent, and an existing non-persistent one is registered rather than
// left as a working-until-restarted state.
func TestISCSILoginsArePersistent(t *testing.T) {
	s := iscsiScript(iscsiSpec(), false, true)
	// Splatted, and carrying the portal it is made through, so each declared path
	// gets its own session. The persistence property asserted is unchanged.
	if !strings.Contains(s, "$c = @{ NodeAddress = $t; IsPersistent = $true; TargetPortalAddress = $addr }") {
		t.Error("a new login must be persistent and made through a named portal")
	}
	if !strings.Contains(s, "Connect-IscsiTarget @c") {
		t.Error("the login must actually be made from the assembled arguments")
	}
	if !strings.Contains(s, "Register-IscsiSession") {
		t.Error("an existing non-persistent session must be made persistent, not left alone")
	}
	// The service must survive a reboot too, or persistent logins mean nothing.
	if !strings.Contains(s, "Set-Service MSiSCSI -StartupType Automatic") {
		t.Error("MSiSCSI defaults to manual start; persistent sessions need it automatic")
	}
}

// Additive only. Removing a login is not the inverse of adding one: a session
// carrying a clustered disk takes the disk with it, and the spec cannot say
// whether an unlisted target is one the operator dropped or one something else
// on the host needs.
func TestISCSIReconcileNeverDisconnects(t *testing.T) {
	s := iscsiScript(iscsiSpec(), true, true)
	for _, forbidden := range []string{
		"Disconnect-IscsiTarget",
		"Remove-IscsiTargetPortal",
		"Unregister-IscsiSession",
	} {
		if strings.Contains(s, forbidden) {
			t.Errorf("the reconcile must not tear down storage: found %q", forbidden)
		}
	}
}

// Idempotency is judged PER PORTAL, not per target.
//
// "Is this target logged in at all?" was the wrong question: with three portals
// and one session the answer is yes, so the target was skipped and the node stayed
// on a single path for ever — multipath in effect and nothing to coalesce. What
// must not be repeated is a login through a portal that already carries one.
func TestISCSIIsIdempotentPerPortal(t *testing.T) {
	s := iscsiScript(iscsiSpec(), false, true)
	if !strings.Contains(s, "if ($have.Count -eq 0) {") {
		t.Error("a portal must only be registered when absent")
	}
	if !strings.Contains(s, "if ($covered -contains $addr) { continue }") {
		t.Error("a portal that already carries a session must be skipped; skipping the whole target strands the node on one path")
	}
	if strings.Contains(s, "if ($existing.Count -gt 0) {") {
		t.Error("the target-level skip is back, which is what stopped the remaining paths ever being established")
	}
	// The portals a session covers come from its connections — a session does not
	// carry the portal it was made through.
	if !strings.Contains(s, "Get-IscsiConnection") {
		t.Error("which portals are already covered must be read from the sessions' connections")
	}
}

// MPIO has to be claimed BEFORE a second path logs in. Afterwards, the duplicate
// disks Windows has already presented stay presented — the same LUN as two
// devices the cluster believes are unrelated, which is a corruption, not a
// tuning problem.
func TestMPIOIsInstalledBeforeAnyLogin(t *testing.T) {
	s := iscsiScript(iscsiSpec(), true, true)
	mpio := strings.Index(s, "Install-WindowsFeature -Name Multipath-IO")
	login := strings.Index(s, "Connect-IscsiTarget")
	if mpio == -1 || login == -1 {
		t.Fatal("both steps must be present")
	}
	if mpio > login {
		t.Fatal("MPIO must be installed before the first login, or duplicates are already presented")
	}
	// And a reboot pending means the claim is not in effect yet, so that has to
	// reach the operator rather than being treated as done.
	if !strings.Contains(s, "$out.rebootRequired = $true") {
		t.Error("an MPIO install needing a reboot must be reported; the claim is not active until then")
	}
}

func TestMPIOIsNotInstalledWhenNotWanted(t *testing.T) {
	s := iscsiScript(iscsiSpec(), false, true)
	if strings.Contains(s, "Install-WindowsFeature -Name Multipath-IO") {
		t.Fatal("a single-path cluster must not have MPIO forced on it")
	}
	// It is still REPORTED, because whether it is present matters the moment a
	// second portal is added.
	if !strings.Contains(s, "$out.mpioInstalled") {
		t.Error("MPIO presence must be reported even when not required")
	}
}

// The initiator name is what the array grants the LUN to, so it is the one value
// an operator needs BEFORE anything works. It must therefore survive the case
// where nothing works yet — Get-InitiatorPort returns nothing without the
// service, so the registry is the fallback.
func TestTheInitiatorNameIsReportedEvenIfNothingElseWorks(t *testing.T) {
	s := iscsiScript(iscsiSpec(), false, true)
	if !strings.Contains(s, "Get-InitiatorPort") {
		t.Fatal("the initiator IQN must be reported")
	}
	if !strings.Contains(s, `CurrentVersion\iSCSI`) {
		t.Error("it must fall back to the registry, which holds it whether or not the service is running")
	}
}

// CHAP secrets travel in the environment, never in the script text: the script
// is what appears in an error message and in a log line.
func TestCHAPSecretsAreNotWrittenIntoTheScript(t *testing.T) {
	spec := iscsiSpec()
	spec.CredentialSecret = "nas-chap"
	s := iscsiScript(spec, false, true)
	if !strings.Contains(s, "$env:BALLAST_CHAP_SECRET") {
		t.Fatal("the secret must come from the environment")
	}
	if !strings.Contains(s, "$c['AuthenticationType'] = 'ONEWAYCHAP'") {
		t.Error("a credential means CHAP")
	}
	// The cmdlet rejects a duplicated ChapSecret, which would fail every login.
	if strings.Count(s, "ChapSecret") != 1 {
		t.Errorf("exactly one ChapSecret argument, got %d", strings.Count(s, "ChapSecret"))
	}
}

func TestMutualCHAPSelectsTheRightAuthType(t *testing.T) {
	spec := iscsiSpec()
	spec.CredentialSecret, spec.MutualCHAP = "nas-chap", true
	s := iscsiScript(spec, false, true)
	if !strings.Contains(s, "$c['AuthenticationType'] = 'MUTUALCHAP'") {
		t.Error("mutual CHAP must be requested as such")
	}
	if strings.Count(s, "ChapSecret") != 1 {
		t.Errorf("mutual CHAP still takes one ChapSecret here, got %d", strings.Count(s, "ChapSecret"))
	}
}

// No credential means no CHAP flags at all — an open target must not be sent
// empty credentials, which fails the login rather than connecting.
func TestNoCredentialMeansNoCHAP(t *testing.T) {
	s := iscsiScript(iscsiSpec(), false, true)
	if strings.Contains(s, "-AuthenticationType") || strings.Contains(s, "-ChapSecret") {
		t.Fatal("an open target must be connected without CHAP flags")
	}
}
