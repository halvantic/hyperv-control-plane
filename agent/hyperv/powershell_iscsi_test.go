package hyperv

import (
	"context"
	"strings"
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
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
	s := iscsiScript(iscsiSpec(), false, true, nil)
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
	s := iscsiScript(iscsiSpec(), true, true, nil)
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
	s := iscsiScript(iscsiSpec(), false, true, nil)
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
	s := iscsiScript(iscsiSpec(), true, true, nil)
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
	s := iscsiScript(iscsiSpec(), false, true, nil)
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
	s := iscsiScript(iscsiSpec(), false, true, nil)
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
	s := iscsiScript(spec, false, true, nil)
	if !strings.Contains(s, "$env:BALLAST_CHAP_SECRET") {
		t.Fatal("the secret must come from the environment")
	}
	if !strings.Contains(s, "$c['AuthenticationType'] = 'ONEWAYCHAP'") {
		t.Error("a credential means CHAP")
	}
	// The cmdlet rejects a DUPLICATED ChapSecret, which would fail every login.
	// That rule is per invocation, not per script: discovery and login are two
	// separate calls and each carries the credential once. Counting occurrences
	// across the whole script was a proxy for it, and became wrong the moment
	// discovery started authenticating too.
	if n := strings.Count(s, "$c['ChapSecret']"); n != 1 {
		t.Errorf("the login must pass exactly one ChapSecret, got %d", n)
	}
	// PER INVOCATION, not per script. There are three now — discovery, the
	// discovery retry, and making a session persistent — and each carries the
	// secret exactly once. A script-wide count was a proxy for the real rule and
	// has already broken twice as invocations were added; count within each
	// hashtable instead.
	for _, splat := range []string{"$portalAuthFallback = @{", "$sessionChap = @{"} {
		i := strings.Index(s, splat)
		if i < 0 {
			t.Errorf("expected splat %s", splat)
			continue
		}
		line := s[i:]
		if end := strings.IndexByte(line, '\n'); end > 0 {
			line = line[:end]
		}
		if n := strings.Count(line, "ChapSecret"); n != 1 {
			t.Errorf("%s must pass exactly one ChapSecret, got %d: %s", splat, n, line)
		}
	}
}

func TestMutualCHAPSelectsTheRightAuthType(t *testing.T) {
	spec := iscsiSpec()
	spec.CredentialSecret, spec.MutualCHAP = "nas-chap", true
	s := iscsiScript(spec, false, true, nil)
	if !strings.Contains(s, "$c['AuthenticationType'] = 'MUTUALCHAP'") {
		t.Error("mutual CHAP must be requested as such")
	}
	// Mutual CHAP needs the initiator's own secret set once on the host
	// (Set-IscsiChapSecret), NOT a second -ChapSecret on the call — which the
	// cmdlet rejects outright. One per invocation, discovery and login alike.
	if n := strings.Count(s, "$c['ChapSecret']"); n != 1 {
		t.Errorf("mutual CHAP still takes one ChapSecret on the login, got %d", n)
	}
	// Discovery gets it only as a FALLBACK, not on the first attempt — see
	// TestISCSIDiscoveryIsUnauthenticatedFirst.
	if !strings.Contains(s, "$portalAuthFallback = @{ AuthenticationType = 'MUTUALCHAP'") {
		t.Error("mutual CHAP must survive into the discovery fallback")
	}
}

/*
No credential means no CHAP anywhere — an open target must not be sent empty

	credentials, which fails the login rather than connecting.

	Asserted on the ASSIGNMENTS, not on the flag names appearing somewhere in the
	script. The old form searched for "-ChapSecret" across the whole text and so
	matched a COMMENT that happened to name the parameter, which is the second
	time today a test in this package read prose instead of code.
*/
func TestNoCredentialMeansNoCHAP(t *testing.T) {
	s := iscsiScript(iscsiSpec(), false, true, nil)
	for _, forbidden := range []string{
		"$c['AuthenticationType']",
		"$c['ChapSecret']",
		"$c['ChapUsername']",
	} {
		if strings.Contains(s, forbidden) {
			t.Errorf("an open target must be connected without CHAP: found %s", forbidden)
		}
	}
	// The splats that carry it elsewhere must be empty too, or an empty secret
	// reaches discovery or the persistence registration instead.
	for _, empty := range []string{"$portalAuthFirst = @{}", "$portalAuthFallback = @{}", "$sessionChap = @{}"} {
		if !strings.Contains(s, empty) {
			t.Errorf("with no credential, %s must be empty", empty)
		}
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

// Logged in, paths up, and nothing behind it.
//
// From the rig, 2026-08-23: both DRCluster members held three-path sessions and
// reported ZERO disks, while the cluster's two CSVs sat Offline. Ballast said
// nothing at all about it — no login had failed, so no message was set, and the
// host reported a healthy-looking iSCSI block with an empty disk list. The only
// thing naming a problem was a CSV alarm one layer up, which pointed at the
// cluster rather than at the array that had stopped presenting the LUNs.
//
// The initiator side is working by definition in this state, so the remedy is on
// the storage device — which Ballast does not administer, and must therefore
// name explicitly rather than fail obscurely.
func TestISCSIReportsASessionWithNoLUNsBehindIt(t *testing.T) {
	s := iscsiScript(iscsiSpec(), true, true, nil)

	if !strings.Contains(s, "$connected.Count -gt 0 -and $out.disks.Count -eq 0") {
		t.Fatal("a connected session with no disks must be recognised as its own condition")
	}
	if !strings.Contains(s, "the array is presenting no LUNs on it") {
		t.Error("the message must say what is actually missing")
	}
	// It must not read as a host fault: the login succeeded and the paths are up.
	if !strings.Contains(s, "The initiator side is working") {
		t.Error("the message must place the fault on the array, not on the host")
	}
	if !strings.Contains(s, "$out.initiatorIQN +") {
		t.Error("the message must carry the initiator IQN — it is what gets added to the LUN's masking list")
	}
	// The consequence, so the CSV alarm one layer up is connected to its cause.
	if !strings.Contains(s, "will be Offline until it is") {
		t.Error("the message must link the missing LUNs to the offline cluster volumes")
	}
	// It must not stamp on a login failure, which is a more specific diagnosis.
	if !strings.Contains(s, "if (-not $out.message) {") {
		t.Error("a real login error must win over this fallback")
	}
}

// Absent is not zero. `Get-Disk -ErrorAction SilentlyContinue` turns "the
// Storage service did not answer" into an empty list, which is then reported as
// "this host sees no iSCSI disks" — a different and far more alarming statement
// than "I could not tell". This is the costliest recurring defect in Ballast, so
// the enumeration says which of the two it is.
func TestISCSIDiskEnumerationFailureIsNotReportedAsNoDisks(t *testing.T) {
	s := iscsiScript(iscsiSpec(), true, true, nil)

	if !strings.Contains(s, "$rawDisks = @(Get-Disk -ErrorAction Stop)") {
		t.Fatal("the disk read must fail loudly rather than yielding an empty list")
	}
	if !strings.Contains(s, "$diskReadFailed = ([string]$_.Exception.Message).Trim()") {
		t.Error("the reason the read failed must be captured, not discarded")
	}
	if !strings.Contains(s, "is UNKNOWN rather than none") {
		t.Error("an unreadable disk list must be reported as unknown, never as zero disks")
	}
}

// Discovery returning nothing is a different fault from a login being refused,
// and it has to be named first: the login error that follows names a target and
// points at the array, when the real answer is that nothing was advertised at
// all and no name could have matched.
//
// And "no path" and "not permitted" look identical from the initiator, with
// completely different remedies — one is a network fault on this host, the other
// is a line in the array's masking list. Ballast can tell them apart with a TCP
// probe, so leaving the operator to guess would be a diagnosis it could make and
// did not.
func TestISCSIEmptyDiscoveryIsReportedBeforeTheLoginError(t *testing.T) {
	s := iscsiScript(iscsiSpec(), true, true, nil)

	if !strings.Contains(s, "$advertisedNow.Count -eq 0 -and $discovered.Count -gt 0") {
		t.Fatal("an empty discovery must be recognised as its own condition")
	}
	// The probe that separates the two causes.
	if !strings.Contains(s, "New-Object System.Net.Sockets.TcpClient") {
		t.Fatal("the portals must actually be probed, not guessed about")
	}
	if !strings.Contains(s, "$reachable += ") || !strings.Contains(s, "$unreachable += ") {
		t.Error("each portal must be sorted into reachable or not")
	}

	// Three answers, because there are three situations.
	for _, want := range []string{
		"no path to the array: ",
		"The fault is HERE, not on the array",
		"but advertised NO target to this host, and ",
		"the array answered on every portal",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the diagnosis must distinguish the causes; missing %q", want)
		}
	}
	// The specific trap: an operator checks the array's initiator list, sees the
	// IQN, and concludes masking is fine — but the lists are per target.
	if strings.Count(s, "per target") < 2 {
		t.Error("both masking branches must say allowed-initiator lists are per target")
	}
	if !strings.Contains(s, "this is not a login being refused") {
		t.Error("the message must say what it is NOT")
	}
	// Decided BEFORE the login-failure branch, which is guarded on
	// -not $out.message, so the more specific diagnosis wins.
	if strings.Index(s, "$advertisedNow.Count -eq 0") > strings.Index(s, "could not log in through ") {
		t.Error("empty discovery must be decided BEFORE the login error, or the vaguer message wins")
	}
}

// The reconcile REPORTS a stale portal binding; the repair is a job.
//
// Fixing it means removing a portal entry, and TestISCSIReconcileNeverDisconnects
// forbids that in the loop that runs every cycle — rightly, because a reconcile
// that can pull storage entries is a reconcile that can pull them at three in
// the morning for a reason nobody asked for. So the detection lives in the pass
// and the remedy is operator-initiated, the same shape as RepairPool.
func TestISCSIStalePortalIsReportedByTheReconcileNotFixedByIt(t *testing.T) {
	s := iscsiScript(iscsiSpec(), true, true, nil)

	if !strings.Contains(s, "$bound = [string]$h.InitiatorPortalAddress") {
		t.Fatal("the reconcile must inspect the portal's source binding")
	}
	if !strings.Contains(s, "$stalePortals += (") {
		t.Error("a dead binding must be recorded")
	}
	if !strings.Contains(s, "cannot discover through ") {
		t.Error("the message must say the host cannot discover, not merely that a portal looks odd")
	}
	// It must explain the misleading symptom it causes, or the operator follows
	// the login error to the array and finds nothing wrong.
	if !strings.Contains(s, `fails every login with "the target name is not found"`) {
		t.Error("the message must connect the binding to the login error it produces")
	}
	if !strings.Contains(s, "Run Repair iSCSI portals") {
		t.Error("the message must name the action that fixes it")
	}
	// And the reconcile itself must still not remove anything.
	if strings.Contains(s, "Remove-IscsiTargetPortal") {
		t.Error("the reconcile must stay additive; the removal belongs to the job")
	}
}

// The repair job is narrow on purpose: a binding to a real, present NIC is a
// deliberate choice about which path reaches the array, and is not ours to undo.
func TestISCSIRepairJobOnlyTouchesADeadBinding(t *testing.T) {
	var p PowerShell
	var seen string
	p.run = func(_ context.Context, script string) ([]byte, error) {
		seen = script
		return []byte(`RESULT={"checked":3,"fixed":["10.0.60.52:3260 (was pinned to 10.0.60.9)"],"failed":[],"targets":["iqn.test:t1"]}`), nil
	}
	note, err := p.RepairISCSIPortals(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(seen, "if (-not $bound -or $bound -eq '0.0.0.0' -or ($myIPs -contains $bound)) { continue }") {
		t.Error("a binding to an address the host HAS must be left alone")
	}
	if !strings.Contains(seen, "Remove-IscsiTargetPortal") || !strings.Contains(seen, "New-IscsiTargetPortal") {
		t.Error("the repair must re-register the portal unbound")
	}
	// It re-runs discovery so the operator sees the result now, not next cycle.
	if !strings.Contains(seen, "Update-IscsiTarget") {
		t.Error("the repair must refresh discovery so its own result is visible")
	}
	if !strings.Contains(note, "was pinned to 10.0.60.9") {
		t.Errorf("the result must say what it changed: %q", note)
	}
	if !strings.Contains(note, "discovery now sees iqn.test:t1") {
		t.Errorf("the result must say what discovery sees afterwards: %q", note)
	}
}

// Nothing to fix is an answer, and it must not read as a repair having happened.
// It also has to say what discovery sees, or the operator is left to go and look.
func TestISCSIRepairJobSaysWhenThereWasNothingToFix(t *testing.T) {
	var p PowerShell
	p.run = func(_ context.Context, _ string) ([]byte, error) {
		return []byte(`RESULT={"checked":3,"fixed":[],"failed":[],"targets":[]}`), nil
	}
	note, err := p.RepairISCSIPortals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "none was pinned to a missing address") {
		t.Errorf("expected a plain no-op answer, got %q", note)
	}
	// The remaining possibilities, named — this is the case HVNEW01 lands in if
	// its portals turn out to be fine.
	if !strings.Contains(note, "not on any target's allowed list") {
		t.Errorf("with no targets found it must name what is left to check: %q", note)
	}
}

func TestISCSIRepairJobFailsLoudly(t *testing.T) {
	var p PowerShell
	p.run = func(_ context.Context, _ string) ([]byte, error) {
		return []byte(`RESULT={"checked":1,"fixed":[],"failed":["10.0.60.52:3260 - access denied"],"targets":[]}`), nil
	}
	if _, err := p.RepairISCSIPortals(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "access denied") {
		t.Fatalf("a portal that could not be re-registered must fail the job with the reason, got %v", err)
	}
}

// A persistent login outlives the session, and that is the half that matters
// when the array has DELETED the target.
//
// From the rig, 2026-08-24: HVNEW02 held no session at all, yet the Synology
// logged "Initiator [...hvnew02...] tried to login into a non-existent iSCSI
// iqn [...xpenology.target-1...]" about once a minute, all morning. The
// disconnect job had already run and reported success — it ended the session
// and never touched the registration behind it, because
// Unregister-IscsiSession needs a live session and Disconnect-IscsiTarget only
// ends a connection. Ballast reported nothing wrong; the operator found it in
// the array's own log.
func TestISCSIDisconnectClearsThePersistentLogin(t *testing.T) {
	var p PowerShell
	var seen string
	p.run = func(_ context.Context, script string) ([]byte, error) {
		seen = script
		return []byte(`RESULT={"removed":0,"absent":true,"persistRemoved":["10.0.60.52:3260"],"persistErrors":[]}`), nil
	}
	note, err := p.DisconnectISCSITarget(context.Background(), "iqn.syn:dead-target")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "iscsicli ListPersistentTargets") {
		t.Fatal("the persistent registrations must actually be read; there is no cmdlet for them")
	}
	if !strings.Contains(seen, "iscsicli RemovePersistentTarget") {
		t.Fatal("the registration must be removed, not just the session")
	}
	// A target with no live session is exactly the case this exists for, so the
	// removal must not be inside the has-sessions branch.
	if strings.Index(seen, "Remove-PersistentTarget $t") > strings.Index(seen, "if ($sessions.Count -eq 0)") {
		t.Error("the persistent login must be cleared before the session branch, or a dead target is never reached")
	}
	if !strings.Contains(note, "removed the persistent login through 10.0.60.52:3260") {
		t.Errorf("the result must say what it cleared: %q", note)
	}
	if !strings.Contains(note, "will not be retried again") {
		t.Errorf("the result must say the retrying stops, which is the whole point: %q", note)
	}
}

// A removal that failed must say so, and say what it costs — otherwise the
// array goes on logging a warning a minute and nobody connects the two.
func TestISCSIDisconnectSaysWhenThePersistentLoginSurvives(t *testing.T) {
	var p PowerShell
	p.run = func(_ context.Context, _ string) ([]byte, error) {
		return []byte(`RESULT={"removed":1,"absent":false,"persistRemoved":[],"persistErrors":["10.0.60.52:3260 - access denied"]}`), nil
	}
	note, err := p.DisconnectISCSITarget(context.Background(), "iqn.syn:dead-target")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "could NOT be removed") || !strings.Contains(note, "access denied") {
		t.Errorf("a failed removal must be named with its reason: %q", note)
	}
	if !strings.Contains(note, "once a minute") {
		t.Errorf("it must say what the leftover actually does: %q", note)
	}
}

func TestISCSIDisconnectSaysWhenThereWasNoPersistentLogin(t *testing.T) {
	var p PowerShell
	p.run = func(_ context.Context, _ string) ([]byte, error) {
		return []byte(`RESULT={"removed":2,"absent":false,"persistRemoved":[],"persistErrors":[]}`), nil
	}
	note, err := p.DisconnectISCSITarget(context.Background(), "iqn.syn:t")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "no persistent login for it was registered") {
		t.Errorf("nothing found must read as nothing found, not as a removal: %q", note)
	}
}

// A host that holds a session has plainly been offered a target.
//
// Get-IscsiTarget can return empty for a moment — after Update-IscsiTarget, or
// on a WMI hiccup — while the sessions built from those targets are up and
// serving. Read on its own it produced, on the rig 2026-08-24, a host reporting
// "Connected · 1 of 1 targets · 3 paths · 2 disks" directly above "the array
// advertised NO target to this host". Two statements from one pass, contradicting
// each other, one of them invented.
func TestISCSIEmptyDiscoveryIsNotClaimedWhileSessionsExist(t *testing.T) {
	s := iscsiScript(iscsiSpec(), true, true, nil)

	if !strings.Contains(s, "$liveSessions = @(Get-IscsiSession -ErrorAction SilentlyContinue)") {
		t.Fatal("the sessions must be read before claiming nothing was advertised")
	}
	if !strings.Contains(s, "$advertisedNow.Count -eq 0 -and $discovered.Count -gt 0 -and $liveSessions.Count -eq 0") {
		t.Fatal("a live session must veto the empty-discovery diagnosis — it is proof a target was offered")
	}
	// The reasoning, kept where the next person will change this.
	if !strings.Contains(s, "An absent reading is not a zero") {
		t.Error("the guard must say why it is there, or it reads as a redundant check")
	}
}

// One portal must not take the whole pass with it.
//
// From the rig, 2026-08-24. New-IscsiTargetPortal ran with -ErrorAction Stop and
// no catch, under an $ErrorActionPreference of Stop, so a single portal refusing
// ("New-IscsiTargetPortal : Target Error", HRESULT 0xefff0012) aborted the ENTIRE
// script. The login loop, the sessions, the disks and every diagnosis after it
// never ran — and the host reported no iSCSI state at all. Not a fault it could
// describe: silence. HVNEW02 and HVNEW05 sat like that for a day while the
// console had nothing whatever to show for them.
//
// And it blocked first: registering a portal performs discovery, and against an
// address that does not answer that runs for minutes. Three of them inside a
// reconcile is most of a cycle — the cluster pass sat stalled for 22 minutes.
func TestISCSIAFailingPortalDoesNotAbortThePass(t *testing.T) {
	s := iscsiScript(iscsiSpec(), true, true, nil)

	// The registration is inside a try. This is the whole bug.
	idx := strings.Index(s, "New-IscsiTargetPortal -TargetPortalAddress $addr")
	if idx < 0 {
		t.Fatal("the portal registration went missing")
	}
	before := s[:idx]
	if !strings.Contains(before[strings.LastIndex(before, "foreach ($p in $portals)"):], "try {") {
		t.Fatal("the registration must be inside a try — without one, a refused portal aborts the whole script")
	}
	if !strings.Contains(s, "$portalErrs += ($addr + ':' + $port + ' - '") {
		t.Error("a refused portal must be recorded rather than thrown")
	}
	// And said out loud: a missing portal explains a missing target.
	if !strings.Contains(s, "could not register ") {
		t.Error("a portal that could not be registered must reach the operator")
	}
	if !strings.Contains(s, "Any target reached only through those portals will be missing") {
		t.Error("the message must connect the portal to the consequence")
	}
}

// A portal that is not answering is not registered at all: the probe is what
// keeps a dead address from costing minutes of a cycle.
func TestISCSIPortalIsProbedBeforeRegistering(t *testing.T) {
	s := iscsiScript(iscsiSpec(), true, true, nil)

	reg := strings.Index(s, "New-IscsiTargetPortal -TargetPortalAddress $addr")
	probe := strings.Index(s, "$sock = New-Object System.Net.Sockets.TcpClient")
	if probe < 0 {
		t.Fatal("a portal must be probed before a blocking registration is attempted")
	}
	if probe > reg {
		t.Error("the probe must come BEFORE the registration, or it saves nothing")
	}
	if !strings.Contains(s, "WaitOne(3000, $false)") {
		t.Error("the probe must be bounded; an unbounded check is the thing it replaces")
	}
	if !strings.Contains(s, "did not answer on the iSCSI port, so it was not registered") {
		t.Error("skipping a portal must be reported, not silent")
	}
}

/*
Discovery is tried WITHOUT CHAP first.

	iSCSI has two session types and they authenticate independently: the
	discovery (SendTargets) session, and the normal session that logs in to a
	target. Most arrays — Synology among them — put CHAP on the TARGET, so it
	applies to the normal session, and mask discovery by allowed-initiator list
	instead.

	This code used to send CHAP on discovery whenever a credential was declared.
	An array that does not want it does not ignore it: it refuses, and
	New-IscsiTargetPortal fails with "Authentication Failure". All three members
	of Primary1 hit that on 2026-08-25 — every portal refused, nothing
	discovered, nothing logged in.

	The belief came from an operator connecting a host by hand through iscsicpl
	with CHAP filled in. That was the CONNECT dialog — the normal-session login —
	and what actually fixed it was the initiator-address binding beside it. The
	operator then added a discovery portal with NO password and it worked at
	once, which is the direct disproof. These tests encoded the wrong belief and
	passed the whole time.
*/
func TestISCSIDiscoveryIsUnauthenticatedFirst(t *testing.T) {
	spec := iscsiSpec()
	spec.CredentialSecret = "nas-chap"
	s := iscsiScript(spec, true, true, nil)

	// The first attempt's splat is EMPTY, so it carries the binding and nothing
	// else. Asserted on the splat rather than the call text: the call always
	// names it, and what matters is what is in it.
	if !strings.Contains(s, "$portalAuthFirst = @{}") {
		t.Fatalf("the first discovery attempt must carry no credential:\n%s", s)
	}
	// The credential is prepared, and held back for the retry.
	if !strings.Contains(s, "$portalAuthFallback = @{ AuthenticationType = 'ONEWAYCHAP'") {
		t.Error("the credential must still be available as a fallback")
	}
}

// An array that GENUINELY requires discovery CHAP still works: the credential is
// offered only after the unauthenticated attempt is refused, so both kinds of
// array succeed and neither is sent something it will reject.
func TestISCSIDiscoveryFallsBackToCHAPWhenRefused(t *testing.T) {
	spec := iscsiSpec()
	spec.CredentialSecret = "nas-chap"
	s := iscsiScript(spec, true, true, nil)

	if !strings.Contains(s, "if ($portalAuthFallback.Count -gt 0) {") {
		t.Fatalf("the retry must be guarded on a credential existing:\n%s", s)
	}
	if !strings.Contains(s, "-TargetPortalPortNumber $port @pa @portalAuthFallback -ErrorAction Stop") {
		t.Fatal("the retry must carry the credential")
	}
	// Both reasons reach the operator: an array wanting no CHAP and an array
	// wanting a different one fail differently and must read differently.
	if !strings.Contains(s, "(and again with the CHAP credential: ") {
		t.Error("a failed retry must report both attempts, not just the second")
	}

	spec.MutualCHAP = true
	if !strings.Contains(iscsiScript(spec, true, true, nil), "AuthenticationType = 'MUTUALCHAP'") {
		t.Error("mutual CHAP must survive into the fallback")
	}
}

// With no credential declared there is nothing to fall back to, and the single
// attempt must be the plain one.
func TestISCSIDiscoveryWithNoCredentialTriesOnce(t *testing.T) {
	s := iscsiScript(iscsiSpec(), true, true, nil)
	if !strings.Contains(s, "$portalAuthFallback = @{}") {
		t.Fatal("with no credential there is no fallback")
	}
	if strings.Contains(s, "AuthenticationType = 'ONEWAYCHAP'") {
		t.Error("no credential was declared, so none may be invented")
	}
}

// The diagnosis sent the operator to the array's masking list and named nothing
// else. CHAP-on-discovery was the cause it could not see, and it is the one that
// was actually happening.
func TestISCSIEmptyDiscoveryNamesCHAPAsACause(t *testing.T) {
	withChap := func(on bool) string {
		spec := iscsiSpec()
		if on {
			spec.CredentialSecret = "nas-chap"
		}
		return iscsiScript(spec, true, true, nil)
	}
	for _, s := range []string{withChap(true), withChap(false)} {
		if !strings.Contains(s, "The array may require CHAP for DISCOVERY, not only for login") {
			t.Fatal("the diagnosis must offer CHAP-on-discovery as a cause")
		}
		if !strings.Contains(s, "not on the allowed-initiator list of the SPECIFIC target") {
			t.Error("masking must remain the other candidate, not be replaced by it")
		}
	}
	// And it must say which of the two situations THIS spec is in.
	if !strings.Contains(withChap(true), "this spec does set a CHAP credential") {
		t.Error("a spec with a credential must say to check the secret")
	}
	if !strings.Contains(withChap(false), "this spec sets NO CHAP credential") {
		t.Error("a spec without one must say to add it if the array wants it")
	}
}

/*
Where the credential is presented, when the operator has said.

	Auto is right without knowing the array, but "just try it" is not always
	free: some arrays log an unauthenticated discovery attempt as an intrusion or
	lock the initiator out after a few, and a shop whose policy mandates CHAP
	everywhere wants it declared rather than inferred.
*/
func TestCHAPScopeDecidesWhereTheCredentialGoes(t *testing.T) {
	base := iscsiSpec()
	base.CredentialSecret = "nas-chap"

	// DiscoveryAndTarget: on the first attempt, with nothing held back — there is
	// no second attempt to make.
	spec := base
	spec.CHAPScope = types.CHAPDiscoveryAndTarget
	s := iscsiScript(spec, true, true, nil)
	if !strings.Contains(s, "$portalAuthFirst = @{ AuthenticationType = 'ONEWAYCHAP'") {
		t.Fatalf("DiscoveryAndTarget must authenticate the first attempt:\n%s", s)
	}
	if !strings.Contains(s, "$portalAuthFallback = @{}") {
		t.Error("nothing is held back when the credential is already on the first attempt")
	}

	// TargetOnly: never on discovery, and NOT retried with it either. The
	// operator has stated the array does not want it; retrying anyway would be
	// the console overruling them.
	spec = base
	spec.CHAPScope = types.CHAPTargetOnly
	s = iscsiScript(spec, true, true, nil)
	if strings.Contains(s, "$portalAuthFirst = @{ AuthenticationType") {
		t.Error("TargetOnly must not authenticate discovery")
	}
	if strings.Contains(s, "$portalAuthFallback = @{ AuthenticationType") {
		t.Fatalf("TargetOnly must not retry discovery with CHAP either:\n%s", s)
	}
	// The TARGET login still carries it — that is the whole point of the scope.
	if !strings.Contains(s, "$c['AuthenticationType'] = 'ONEWAYCHAP'") {
		t.Error("TargetOnly must still authenticate the target login")
	}

	// Auto: held back, offered only if refused.
	spec = base
	s = iscsiScript(spec, true, true, nil)
	if !strings.Contains(s, "$portalAuthFirst = @{}") || !strings.Contains(s, "$portalAuthFallback = @{ AuthenticationType") {
		t.Errorf("Auto must try plain then fall back:\n%s", s)
	}
}
