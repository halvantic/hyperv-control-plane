package hyperv

import (
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

/* Installing MPIO asks for a restart, and until that restart it protects
   nothing. The script emitted the MPIO block before the logins and treated that
   as sufficient — but "before" in script order is not "before" in reality. A
   first pass on a fresh node installed the feature, skipped the claim because a
   reboot was pending, and then logged in through every declared portal, leaving
   Windows holding one LUN as several devices it believes are unrelated.

   That is the state the script's own comment says must never happen, and a
   condition reported afterwards does not undo it. */

func multiPortalSpec() types.ISCSIStorageSpec {
	return types.ISCSIStorageSpec{
		Portals: []string{"10.0.60.52", "10.0.60.53", "10.0.60.54"},
		Targets: []string{"iqn.2000-01.com.synology:Xpenology.default-target.aa584d33bc2"},
	}
}

func TestLoginIsRestrictedToOnePathUntilMultipathIsInEffect(t *testing.T) {
	s := iscsiScript(multiPortalSpec(), true, true)

	// Effectiveness is OBSERVED on the host, not derived from what this pass did.
	// rebootRequired only ever described the current pass, so a pass that installed
	// nothing and claimed nothing left it false — and a host that had never
	// restarted since MPIO was installed reported multipath as protecting it.
	if !strings.Contains(s, "$mpioEffective = ($out.mpioInstalled -and (Read-MPIOEffective) -and -not $out.rebootRequired)") {
		t.Fatal("effectiveness must be read from the host, not inferred from this pass's actions")
	}
	if !strings.Contains(s, "Get-Service -Name 'mpio'") {
		t.Fatal("the MPIO bus driver is what coalesces the paths; whether it is RUNNING is the observable that distinguishes installed from in effect")
	}
	if !strings.Contains(s, "if (-not $mpioEffective -and $portals.Count -gt 1)") {
		t.Fatal("with multipath not in effect and more than one portal, the login must be restricted to a single path")
	}
	// The restriction reaches the login by narrowing the set of portals a session
	// is established through — every path is one login, so constraining the list is
	// constraining the paths.
	if !strings.Contains(s, "if ($restrictPortal) { $wantPortals = @($restrictPortal) }") {
		t.Fatal("the restriction has to reach the login call, or it is decoration")
	}

	// The restriction must be decided BEFORE the logins, not reported after them.
	decide := strings.Index(s, "$restrictPortal = $first")
	login := strings.Index(s, "Connect-IscsiTarget @c")
	if decide == -1 || login == -1 || decide > login {
		t.Fatal("the single-path decision must precede the login it constrains")
	}
}

// The claim is what makes MPIO act on iSCSI at all, and it has to be read from
// the automatic-claim SETTINGS.
//
// Get-MSDSMSupportedHW lists vendor/product pairs and carries no BusType, so
// filtering it on one matched nothing on every host for ever: the claim read as
// absent however many times it had been enabled, mpioEffective could never become
// true, and no restart cleared "restart required". Seen on the rig 2026-08-09.
func TestTheClaimIsReadFromTheClaimSettings(t *testing.T) {
	s := iscsiScript(multiPortalSpec(), true, true)

	if !strings.Contains(s, "Get-MSDSMAutomaticClaimSettings") {
		t.Fatal("the claim must be read from the automatic-claim settings")
	}
	// Get-Disk genuinely has a BusType and filtering iSCSI disks on it is correct,
	// so the assertion targets the specific mistake: INVOKING the MSDSM hardware
	// list, whose entries are vendor/product pairs with no BusType at all. The
	// name still appears in the comment explaining why it is not used.
	if strings.Contains(s, "Get-MSDSMSupportedHW -") || strings.Contains(s, "Get-MSDSMSupportedHW |") {
		t.Fatal("the MSDSM hardware list is being queried again; its entries have no BusType, so any filter on one silently matches nothing")
	}
	if !strings.Contains(s, "$out.mpioClaimed = Read-ISCSIClaim") {
		t.Fatal("the claim must be read back rather than inferred from the install succeeding")
	}
	claim := strings.Index(s, "Enable-MSDSMAutomaticClaim")
	read := strings.LastIndex(s, "$out.mpioClaimed = Read-ISCSIClaim")
	if claim == -1 || read == -1 || read < claim {
		t.Fatal("the claim must be re-read after the attempt to make it, or a fresh claim reports as absent")
	}
}

// Enabling the claim takes effect at boot, so a host that has just been given it
// is still unprotected. Not saying so would leave mpioEffective true on a host
// running single-path — the exact thing the flag exists to prevent.
func TestEnablingTheClaimAsksForARestart(t *testing.T) {
	s := iscsiScript(multiPortalSpec(), true, true)

	enable := strings.Index(s, "Enable-MSDSMAutomaticClaim")
	reboot := strings.Index(s[enable:], "$out.rebootRequired = $true")
	if enable == -1 || reboot == -1 {
		t.Fatal("a newly enabled claim must set rebootRequired; it does not take effect until the host restarts")
	}
}

// The claim is attempted alongside the install rather than after the restart.
// Deferring it costs two restarts where one would do, and the node spends the gap
// on a single path with its storage unprotected.
func TestTheClaimIsNotDeferredUntilAfterTheRestart(t *testing.T) {
	s := iscsiScript(multiPortalSpec(), true, true)

	if strings.Contains(s, "if ($out.mpioInstalled -and -not $out.rebootRequired) {") {
		t.Fatal("the claim is gated on no pending reboot again, which defers it to a second restart")
	}
	if !strings.Contains(s, "if ($out.mpioInstalled) {") {
		t.Fatal("the claim should be attempted whenever the feature is installed")
	}
}

// A single portal has no second path, so nothing to coalesce and nothing to
// restrict. Constraining the login there would be pointless ceremony.
func TestASinglePortalIsNotRestricted(t *testing.T) {
	spec := types.ISCSIStorageSpec{Portals: []string{"10.0.60.52"}}
	required, _ := spec.MPIORequired()
	if required {
		t.Fatal("one portal is one path; multipath is not required")
	}
	s := iscsiScript(spec, false, true)
	if !strings.Contains(s, `$restrictPortal = ''`) {
		t.Fatal("the login path must still be well-defined without the MPIO block")
	}
}

// Windows refuses a SECOND session to a target unless the login declares itself
// multipath — "The target has already been logged in via an iSCSI session". That
// is what every extra path hit on the rig, leaving both members on one path with
// MPIO genuinely in effect and nothing to coalesce.
func TestASecondPathDeclaresItselfMultipath(t *testing.T) {
	s := iscsiScript(multiPortalSpec(), true, true)

	if !strings.Contains(s, "if ($mpioEffective) { $c['IsMultipathEnabled'] = $true }") {
		t.Fatal("without IsMultipathEnabled Windows refuses the second session outright, so the extra paths can never be established")
	}
	// Only when multipath really is in effect: declaring a session multipath while
	// MPIO is not claiming is how the same LUN arrives twice as unrelated disks.
	if strings.Contains(s, "$c['IsMultipathEnabled'] = $true\n") && !strings.Contains(s, "if ($mpioEffective)") {
		t.Fatal("multipath must be declared conditionally, not unconditionally")
	}
}

// A single-portal spec never enters the MPIO block, so the variable the login
// reads must still exist — otherwise it evaluates as absent and the login quietly
// never declares itself multipath even once MPIO is in effect.
func TestTheMultipathFlagIsDefinedEvenWithoutTheMPIOBlock(t *testing.T) {
	s := iscsiScript(types.ISCSIStorageSpec{Portals: []string{"10.0.60.52"}}, false, true)
	if !strings.Contains(s, "$mpioEffective = $false") {
		t.Fatal("the flag must be defined on every path through the script")
	}
}

// An "already logged in" refusal means the path EXISTS. Reporting it as a failure
// puts a permanent error on a node whose paths are all present.
func TestAnAlreadyLoggedInPathIsNotAnError(t *testing.T) {
	s := iscsiScript(multiPortalSpec(), true, true)
	if !strings.Contains(s, "-notmatch 'already been logged in'") {
		t.Fatal("a path that already exists must not be reported as a login failure")
	}
}

// The portals a session covers come through the session's own association. A
// filter on a SessionIdentifier property matched nothing, so every portal looked
// uncovered and an existing path was retried on every pass.
func TestCoveredPortalsComeFromTheSessionsAssociation(t *testing.T) {
	s := iscsiScript(multiPortalSpec(), true, true)
	if !strings.Contains(s, "foreach ($cn in @($s | Get-IscsiConnection -ErrorAction SilentlyContinue))") {
		t.Fatal("connections must be taken from the session itself, not filtered on a property they do not reliably expose")
	}
}

// A shared LUN must not be brought online automatically on a cluster member.
// Windows' default policy mounts it read/write on every node that can see it,
// and a shared disk online on two nodes at once is the state clustering exists to
// prevent — so the cluster does not offer it and Add-ClusterDisk silently has
// nothing to add. Both DRCluster members reported both LUNs online at once.
func TestAClusterMemberKeepsNewSharedDisksOffline(t *testing.T) {
	s := iscsiScript(multiPortalSpec(), true, true)
	if !strings.Contains(s, "Set-StorageSetting -NewDiskPolicy OfflineShared") {
		t.Fatal("a cluster member must not automount a newly arrived shared LUN")
	}
	// Set only when it differs, so a converged host does not report a change every
	// pass.
	if !strings.Contains(s, "if ($pol -and $pol -ne 'OfflineShared' -and $pol -ne 'OfflineAll') {") {
		t.Error("the policy must only be set when it is not already right")
	}
}

// A standalone host is the opposite case: its LUN SHOULD come online, because it
// is provisioned there like any other local disk. Forcing it offline would leave
// the operator a disk they cannot format.
func TestAStandaloneHostDoesNotForceItsDisksOffline(t *testing.T) {
	s := iscsiScript(multiPortalSpec(), true, false)
	if strings.Contains(s, "Set-StorageSetting -NewDiskPolicy OfflineShared") {
		t.Fatal("a standalone host's LUN must be allowed online; it has nothing to share it with")
	}
}

// CHAP has to survive the move to splatting. Two spellings of the same login is
// how they drift apart, which is why the restriction is a key rather than a
// second copy of the call.
func TestCHAPStillReachesTheLogin(t *testing.T) {
	spec := multiPortalSpec()
	spec.CredentialSecret = "chap"
	s := iscsiScript(spec, true, true)

	for _, want := range []string{
		"$c['AuthenticationType'] = 'ONEWAYCHAP'",
		"$c['ChapUsername'] = $env:BALLAST_CHAP_USER",
		"$c['ChapSecret'] = $env:BALLAST_CHAP_SECRET",
		"Connect-IscsiTarget @c",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q", want)
		}
	}
	// The secret must never be in the script text.
	if strings.Contains(s, "chap") && strings.Contains(s, "-ChapSecret '") {
		t.Error("the CHAP secret must arrive via the environment, never in the script")
	}
}

func TestMutualCHAPKeepsItsAuthenticationType(t *testing.T) {
	spec := multiPortalSpec()
	spec.CredentialSecret = "chap"
	spec.MutualCHAP = true
	s := iscsiScript(spec, true, true)

	if !strings.Contains(s, "$c['AuthenticationType'] = 'MUTUALCHAP'") {
		t.Fatal("mutual CHAP must reach the login as MUTUALCHAP")
	}
}
