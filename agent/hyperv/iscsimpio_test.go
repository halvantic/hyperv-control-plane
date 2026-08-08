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
	s := iscsiScript(multiPortalSpec(), true)

	if !strings.Contains(s, "$mpioEffective = ($out.mpioInstalled -and $out.mpioClaimed -and -not $out.rebootRequired)") {
		t.Fatal("effectiveness must require the claim AND no pending reboot; installed alone protects nothing")
	}
	if !strings.Contains(s, "if (-not $mpioEffective -and $portals.Count -gt 1)") {
		t.Fatal("with multipath not in effect and more than one portal, the login must be restricted to a single path")
	}
	if !strings.Contains(s, "if ($restrictPortal) { $c['TargetPortalAddress'] = $restrictPortal }") {
		t.Fatal("the restriction has to reach the login call, or it is decoration")
	}

	// The restriction must be decided BEFORE the logins, not reported after them.
	decide := strings.Index(s, "$restrictPortal = $first")
	login := strings.Index(s, "Connect-IscsiTarget @c")
	if decide == -1 || login == -1 || decide > login {
		t.Fatal("the single-path decision must precede the login it constrains")
	}
}

// The claim is what makes MPIO act on iSCSI at all, and it has to be observed
// rather than assumed: the feature can be present with no bus type claimed,
// which looks configured and protects nothing.
func TestTheClaimIsObservedNotAssumed(t *testing.T) {
	s := iscsiScript(multiPortalSpec(), true)

	if !strings.Contains(s, "$out.mpioClaimed = [bool](Get-MSDSMSupportedHW") {
		t.Fatal("whether iSCSI is actually claimed must be read back, not inferred from the install")
	}
	claim := strings.Index(s, "Enable-MSDSMAutomaticClaim")
	read := strings.Index(s, "$out.mpioClaimed = [bool](Get-MSDSMSupportedHW")
	if claim == -1 || read == -1 || read < claim {
		t.Fatal("the claim must be read after the attempt to make it, or a fresh claim reports as absent")
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
	s := iscsiScript(spec, false)
	if !strings.Contains(s, `$restrictPortal = ''`) {
		t.Fatal("the login path must still be well-defined without the MPIO block")
	}
}

// CHAP has to survive the move to splatting. Two spellings of the same login is
// how they drift apart, which is why the restriction is a key rather than a
// second copy of the call.
func TestCHAPStillReachesTheLogin(t *testing.T) {
	spec := multiPortalSpec()
	spec.CredentialSecret = "chap"
	s := iscsiScript(spec, true)

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
	s := iscsiScript(spec, true)

	if !strings.Contains(s, "$c['AuthenticationType'] = 'MUTUALCHAP'") {
		t.Fatal("mutual CHAP must reach the login as MUTUALCHAP")
	}
}
