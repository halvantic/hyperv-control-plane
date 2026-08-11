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

/* The adoption deadlocked on the rig, and the cause was ORDER, not logic: the
   contents guard ran before the disk was readable and before the cluster was
   consulted. An offline disk reports partitions but no filesystems, so a volume
   Ballast itself labelled read as unknown data — and the only remedy the refusal
   could offer was a wipe of exactly the volume that was meant to be resumed. */

func adoptSectionOrder(t *testing.T) map[string]int {
	t.Helper()
	at := map[string]int{}
	for _, sec := range []string{
		"# ---- locate the LUN",
		"# ---- already adopted?",
		"# ---- make the disk readable",
		"# ---- refuse to destroy data",
		"# ---- prepare the disk",
		"# ---- hand it to the cluster",
	} {
		i := strings.Index(adoptScript, sec)
		if i < 0 {
			t.Fatalf("adopt script has no %q section", sec)
		}
		at[sec] = i
	}
	return at
}

// A disk that is offline reports its partitions but not their filesystems, so the
// label check cannot match and its own volume reads as somebody else's data.
func TestTheDiskIsMadeReadableBeforeItsContentsAreJudged(t *testing.T) {
	at := adoptSectionOrder(t)
	if at["# ---- make the disk readable"] > at["# ---- refuse to destroy data"] {
		t.Fatal("contents are judged before the disk is readable: an offline disk's own volume reads as unknown data and the adoption refuses itself for ever")
	}
}

// Nothing formats a disk the cluster already holds, so judging its contents can
// only refuse a risk that is not there — which stranded two LUNs the cluster was
// already holding, offline, waiting to become CSVs.
func TestTheClusterIsConsultedBeforeTheContentsGuard(t *testing.T) {
	at := adoptSectionOrder(t)
	if at["# ---- already adopted?"] > at["# ---- refuse to destroy data"] {
		t.Fatal("the contents guard runs before the cluster check, so an already-clustered disk is refused for contents that will never be formatted")
	}
	if !strings.Contains(adoptScript, "if (-not $clusDisk) {") {
		t.Error("the readability and contents steps must be skipped for a disk the cluster holds")
	}
}

// Bringing a disk online is what makes it legible; it must not be reached past the
// cluster for a disk the cluster is managing.
func TestOnliningIsScopedToDisksTheClusterDoesNotHold(t *testing.T) {
	script := adoptScript
	guard := strings.Index(script, "if (-not $clusDisk) {")
	online := strings.Index(script, "Set-Disk -Number $disk.Number -IsOffline $false")
	if guard < 0 || online < 0 || online < guard {
		t.Fatal("the first online step must sit inside the not-clustered guard")
	}
}

// "a Basic partition" and "a partition we cannot read" are different facts, and
// only the second makes a wipe a reckless suggestion.
func TestAnUnreadableFilesystemIsReportedAsSuchNotAsBareData(t *testing.T) {
	if !strings.Contains(adoptScript, "whose filesystem could not be read") {
		t.Error("a partition with no readable filesystem must say so rather than read as empty")
	}
	if !strings.Contains(adoptScript, "will not format a LUN it cannot read") {
		t.Error("the refusal for unreadable contents must not offer a wipe as the remedy")
	}
}

// Being in the cluster is not the same as being available. Both DR LUNs were
// adopted, named correctly, and Offline — so C:\ClusterStorage held nothing for
// them and the mount-point step reported no path, while the adoption itself
// reported success.
func TestAnAdoptedDiskIsBroughtOnline(t *testing.T) {
	if !strings.Contains(adoptScript, "Start-ClusterResource") {
		t.Fatal("nothing starts the cluster resource: an offline CSV has no mount path, so the volume is present and unusable")
	}
}

// The early returns are the ones that mattered: a CSV that already existed but
// sat offline returned "already a CSV" and was never started.
func TestTheAlreadyAdoptedPathsStillEnsureOnline(t *testing.T) {
	// Anchored on the call itself, then on the report following it. Searching for
	// the report first found the prose in the comment above it.
	for _, c := range []struct{ call, reports string }{
		{"$started = Ensure-ResourceOnline $csv", "already a CSV"},
		{"$started = Ensure-ResourceOnline $clusDisk", "already a clustered disk"},
	} {
		i := strings.Index(adoptScript, c.call)
		if i < 0 {
			t.Fatalf("no %q call in the script", c.call)
		}
		rest := adoptScript[i:]
		end := strings.Index(rest, "return")
		if end < 0 {
			t.Fatalf("%q is not followed by a return", c.call)
		}
		if !strings.Contains(rest[:end], c.reports) {
			t.Errorf("the %q path does not report through the call that ensures it is online", c.reports)
		}
	}
}

// An offline resource that will not start is a real failure with a real remedy,
// and reporting the adoption as done would hide it.
func TestAResourceThatWillNotStartIsReported(t *testing.T) {
	if !strings.Contains(adoptScript, "would not come online") {
		t.Error("a resource that refuses to start must be reported, not swallowed")
	}
	if !strings.Contains(adoptScript, "An offline volume has no mount path") {
		t.Error("the failure must say why an offline volume matters")
	}
}

// The object IS the resource. Re-fetching it by name returned nothing on the rig
// — the cluster reported no Physical Disk resources while handing back two
// offline CSVs — so the online step found nothing, reported nothing to do, and
// two offline volumes survived the upgrade written to fix exactly that.
func TestTheOnlineStepDoesNotReFetchTheResourceByName(t *testing.T) {
	i := strings.Index(adoptScript, "function Ensure-ResourceOnline")
	if i < 0 {
		t.Fatal("no Ensure-ResourceOnline in the script")
	}
	body := adoptScript[i:]
	if end := strings.Index(body, "\n# ---- locate"); end > 0 {
		body = body[:end]
	}
	// Comment lines skipped: the function explains why it does not look the
	// resource up by name, and naming the call there is the point.
	for _, line := range strings.Split(body, "\n") {
		code := strings.TrimSpace(line)
		if !strings.HasPrefix(code, "#") && strings.Contains(code, "Get-ClusterResource -Name") {
			t.Errorf("the resource is looked up by name again; whatever it is called on this build, the caller already holds it: %q", code)
		}
	}
	if !strings.Contains(body, "Start-ClusterResource -InputObject $res") {
		t.Error("the resource must be started through the object it was given")
	}
}
