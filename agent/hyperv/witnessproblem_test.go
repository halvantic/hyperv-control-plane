package hyperv

import (
	"strings"
	"testing"
)

// The fault that started this: a share that is not there was reported as a
// share whose permissions are wrong, because Test-Path answers $false for both.
// The operator was sent to grant an account access to something that does not
// exist.
func TestAMissingShareIsNotAPermissionsProblem(t *testing.T) {
	msg := witnessProblemMessage(witnessProblem{
		Kind: "noshare", Code: 53,
		Server: "nfs.lab.example", Share: "witness",
		Path: `\\nfs.lab.example\witness`, CNO: "Secondary", Node: "HVNEW02",
		Raw: `'\\nfs.lab.example\witness' is not a valid file share path.`,
	})
	for _, forbidden := range []string{"Grant the cluster computer account", "read/write access to the share", "cannot read it"} {
		if strings.Contains(msg, forbidden) {
			t.Fatalf("a missing share must not be reported as a permissions fault; message contained %q:\n%s", forbidden, msg)
		}
	}
	for _, want := range []string{"does not have a share called witness", "nfs.lab.example", "NFS export is not an SMB share"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message should say %q:\n%s", want, msg)
		}
	}
}

// Each cause names its own remedy. A witness failure is worth reporting only if
// the sentence tells the operator what to do next.
func TestEachCauseNamesItsOwnRemedy(t *testing.T) {
	base := witnessProblem{
		Server: "nas.example.local", Share: "witness",
		Path: `\\nas.example.local\witness`, CNO: "Secondary", Node: "HVNEW02",
	}
	cases := []struct {
		kind string
		code int
		want []string
	}{
		{"unreachable", -1, []string{"not reachable on SMB (tcp/445)", "name resolves"}},
		{"noshare", 53, []string{"does not have a share called witness", "create the share"}},
		{"denied", 5, []string{"may not read it", "HVNEW02's own computer account", "Grant the cluster computer account Secondary$"}},
		{"credentials", 1326, []string{"would not accept this node's credentials", "says nothing about whether the share is there", "Grant that account read/write access"}},
		{"grant", 0, []string{"exists and is readable", "refused the cluster's attempt to grant itself access"}},
		{"clusterdenied", 0, []string{"was refused access to it", "the share AND on the directory behind it", "copy that share's permissions"}},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			p := base
			p.Kind, p.Code = c.kind, c.code
			msg := witnessProblemMessage(p)
			for _, want := range c.want {
				if !strings.Contains(msg, want) {
					t.Fatalf("%s message should contain %q:\n%s", c.kind, want, msg)
				}
			}
			// Ballast has no agent on a file server, so every message that needs
			// work there has to say the work happens there.
			if c.kind != "unreachable" && !strings.Contains(msg, "nas.example.local") {
				t.Fatalf("%s message should name the server the work happens on:\n%s", c.kind, msg)
			}
		})
	}
}

// The cmdlet's own words are evidence, not a second opinion. Run together with
// Ballast's sentence they read as a contradicting diagnosis — which is exactly
// how "this node cannot read it" and "is not a valid file share path" ended up
// in one message.
func TestTheCmdletsReportIsAttributedNotAppended(t *testing.T) {
	raw := `'\\nas.example.local\witness' is not a valid file share path.`
	msg := witnessProblemMessage(witnessProblem{
		Kind: "noshare", Code: 53, Server: "nas.example.local", Share: "witness",
		Path: `\\nas.example.local\witness`, CNO: "Secondary", Raw: raw,
	})
	if !strings.Contains(msg, "Set-ClusterQuorum's own report was: \""+raw+"\"") {
		t.Fatalf("the raw report should be attributed and quoted:\n%s", msg)
	}
	if strings.HasSuffix(strings.TrimSpace(msg), raw) {
		t.Fatalf("the raw report should not be the trailing sentence, unattributed:\n%s", msg)
	}
}

// An unclassified failure says so. Picking a remedy with nothing established is
// the mistake this replaced.
func TestAnUnknownCauseAdmitsItAndKeepsTheEvidence(t *testing.T) {
	msg := witnessProblemMessage(witnessProblem{
		Kind: "unknown", Code: -1, Server: "nas.example.local",
		Path: `\\nas.example.local\witness`, Raw: "Set-ClusterQuorum : something else entirely",
	})
	if !strings.Contains(msg, "could not be established") {
		t.Fatalf("an unknown cause should say so:\n%s", msg)
	}
	if !strings.Contains(msg, "something else entirely") {
		t.Fatalf("an unknown cause must keep the only evidence there is:\n%s", msg)
	}
	if strings.Contains(msg, "Grant") {
		t.Fatalf("an unknown cause must not name a remedy it has not established:\n%s", msg)
	}
	// A code the classifier did not recognise is still worth reporting.
	withCode := witnessProblemMessage(witnessProblem{Kind: "unknown", Code: 1117, Path: `\\s\w`})
	if !strings.Contains(withCode, "Windows error 1117") {
		t.Fatalf("an unrecognised Win32 code should still be named:\n%s", withCode)
	}
}

// A runaway error record must not bury the remedy above it.
func TestTheRawReportIsBounded(t *testing.T) {
	msg := witnessProblemMessage(witnessProblem{
		Kind: "denied", Code: 5, Server: "nas.example.local",
		Path: `\\nas.example.local\witness`, CNO: "Secondary", Node: "HVNEW02",
		Raw: strings.Repeat("noise ", 400),
	})
	if len(msg) > 1200 {
		t.Fatalf("message ran to %d characters; the raw report should be clipped", len(msg))
	}
	if !strings.Contains(msg, "…") {
		t.Fatalf("a clipped report should show that it was clipped:\n%s", msg)
	}
}

// Missing fields must not produce a sentence with a hole in it.
func TestAnEmptyClassificationStillReads(t *testing.T) {
	msg := witnessProblemMessage(witnessProblem{Kind: "denied"})
	for _, forbidden := range []string{"  ", "account $", "on  itself"} {
		if strings.Contains(msg, forbidden) {
			t.Fatalf("message has a hole in it (%q):\n%s", forbidden, msg)
		}
	}
	if !strings.Contains(msg, "the cluster's computer account") {
		t.Fatalf("with no cluster name the account should still be named generically:\n%s", msg)
	}
}

// The script has to ASK before it decides. Test-Path could not tell the two
// causes apart, which is what made the wrong remedy possible.
func TestTheScriptEstablishesTheCauseBeforeReportingIt(t *testing.T) {
	for _, want := range []string{
		"function ProbeShare",
		"EnumerateFileSystemEntries",
		"$kind = 'noshare'",
		"$kind = 'denied'",
		"problem = $problem",
	} {
		if !strings.Contains(witnessScript, want) {
			t.Fatalf("witnessScript should contain %q", want)
		}
	}
	// The old sentence was thrown from the script; it is built in Go now, and a
	// throw would arrive wrapped in "powershell: exit status 1:".
	if strings.Contains(witnessScript, "answers on SMB but this node cannot read it") {
		t.Fatal("the script should no longer throw the operator's sentence")
	}
	if strings.Contains(witnessScript, "Test-Path -LiteralPath $desired") {
		t.Fatal("Test-Path cannot tell a missing share from an unreadable one; ProbeShare replaced it")
	}
}

// The cluster's verdict about its own identity outranks the node's probe. On
// the rig the node could not authenticate to the share at all (1326) while the
// cluster authenticated and was refused ON the share — classifying that from
// the probe would have sent someone to investigate the node.
func TestTheClustersOwnVerdictOutranksTheNodesProbe(t *testing.T) {
	msg := witnessProblemMessage(witnessProblem{
		Kind: "clusterdenied", Code: 1326,
		Server: "podman.ballast.local", Share: "witness2",
		Path: `\\podman.ballast.local\witness2`, CNO: "Secondary", Node: "HVNEW02",
		Raw: `Access was denied to share path'\\podman.ballast.local\witness2'.`,
	})
	if !strings.Contains(msg, "the cluster reached") {
		t.Fatalf("the cluster's own attempt should be what the message describes:\n%s", msg)
	}
	if !strings.Contains(msg, "different identity and is not what is blocking the witness") {
		t.Fatalf("the node's separate failure should be named and set aside:\n%s", msg)
	}
	if strings.Contains(msg, "does not have a share called") {
		t.Fatalf("a refusal on the share must not be reported as a missing share:\n%s", msg)
	}
}

// The script must reach that classification from the cmdlet's wording, not from
// the probe code, because the probe asks as the wrong identity.
func TestTheScriptClassifiesTheClustersRefusalFirst(t *testing.T) {
	i := strings.Index(witnessScript, "denied to share path")
	j := strings.Index(witnessScript, "$code -eq 0")
	if i < 0 {
		t.Fatal("the script should recognise the cluster's own access refusal")
	}
	if j >= 0 && i > j {
		t.Fatal("the cluster's refusal must be classified before the probe code, which asks as the node")
	}
}

/*
The OLD witness would not delete, so the new one could never be applied.

	Secondary, 2026-09-14: "An error occurred while attempting to delete the
	resource 'File Share Witness'." The classifier correctly refused to guess and
	said the cause could not be established — which was honest and useless. It is
	a known failure with a known remedy, and CLAUDE.md is explicit that those must
	name the cause and offer the action.

	It is also the operator's own first instinct being right: they asked whether
	this was "the inability to remove old and add new", and for this error it is
	exactly that.
*/
func TestAStuckWitnessDeleteIsNamedAsTheBlocker(t *testing.T) {
	msg := witnessProblemMessage(witnessProblem{
		Kind: "deleteblocked", Server: "podman.ballast.local", Share: "witness2",
		Path: `\podman.ballast.local\witness2`, CNO: "Secondary", Node: "HVNEW04",
		WitnessState: "Failed",
		Raw:          "An error occurred while attempting to delete the resource 'File Share Witness'.",
	})
	if !strings.Contains(msg, "blocks every witness change") {
		t.Fatalf("it should say the stuck resource blocks all witness changes, not just this one:\n%s", msg)
	}
	if !strings.Contains(msg, "Remove-ClusterResource -Force") {
		t.Fatalf("a known remedy must be named:\n%s", msg)
	}
	// And Ballast must own the gap rather than presenting a host session as a
	// normal step — the boundary rule in CLAUDE.md.
	if !strings.Contains(msg, "gap in Ballast") {
		t.Fatalf("needing a session on a host is a defect and should say so:\n%s", msg)
	}
	if !strings.Contains(msg, "Failed") {
		t.Fatalf("the stuck resource's state is the reason it is stuck; name it:\n%s", msg)
	}
}

/*
Cleared, then the apply failed anyway. This is a DIFFERENT situation and the

	difference matters a great deal: the old witness is gone, so the cluster is
	running with no witness at all rather than with a broken one. An operator who
	reads the same sentence for both would not know that.
*/
func TestAFailedRetryAfterClearingSaysTheClusterNowHasNoWitness(t *testing.T) {
	msg := witnessProblemMessage(witnessProblem{
		Kind: "deleteblocked", AfterClear: true, WitnessState: "Failed",
		Path: `\podman.ballast.local\witness2`, Server: "podman.ballast.local", CNO: "Secondary",
		Raw: "Access was denied to share path.",
	})
	if !strings.Contains(msg, "NO witness configured") {
		t.Fatalf("clearing succeeded and applying did not — the cluster has no witness and must be told:\n%s", msg)
	}
	if strings.Contains(msg, "Remove-ClusterResource") {
		t.Fatalf("the resource is already gone; naming its removal again is wrong:\n%s", msg)
	}
}

// The script has to try clearing it before reporting, and only when the stuck
// resource is not casting a vote.
func TestTheScriptClearsAStuckWitnessOnlyWhenItIsNotVoting(t *testing.T) {
	for _, want := range []string{
		"delete the resource",
		"Set-ClusterQuorum -NodeMajority",
		"Remove-ClusterResource",
		// -not $anyOnline, not $wrState: the state of the FIRST resource says
		// nothing about the other eight when a cluster has collected duplicates.
		"-not $anyOnline",
	} {
		if !strings.Contains(witnessScript, want) {
			t.Fatalf("witnessScript should contain %q", want)
		}
	}
	/* A witness that IS online is casting a vote. Tearing it down to apply a new
	   one would remove a working vote on the strength of a guess.

	   Scoped to the RECOVERY block. The first Set-ClusterQuorum -NodeMajority in
	   the script belongs to the legitimate "witness declared None" branch, and an
	   unscoped search finds that one and compares the wrong two positions — which
	   is what this test did on its first run. */
	rec := witnessScript[strings.Index(witnessScript, "delete the resource"):]
	i := strings.Index(rec, "-not $anyOnline")
	j := strings.Index(rec, "Set-ClusterQuorum -NodeMajority -ErrorAction Stop")
	if i < 0 || j < 0 || i > j {
		t.Fatal("the not-online guard must come before the quorum change, or a healthy witness gets torn down")
	}
	// And the retry of the declared witness comes after the clear, not before it.
	if k := strings.Index(rec, "Set-ClusterQuorum -FileShareWitness $desired"); k < 0 || k < j {
		t.Fatal("the declared witness must be re-applied after the stuck one is cleared")
	}
}

/* The create loop, and the duplicates it left.

   Shipped 0.4.270 and measured within the hour: nine File Share Witness
   resources on Primary1 and five on Secondary, from one declaration each, the
   old one correctly gone. Two faults compounding.

   FIRST, a configured-but-FAILED witness was read as not configured. $curPath
   came from Get-ClusterQuorum alone, which did not give it up for a failed
   resource, so the comparison decided the declared witness was missing and
   applied it again — every pass, for ever. Whether a witness is HEALTHY is a
   different question from whether it is CONFIGURED, and only the second belongs
   in that comparison; the first is what the core-group condition reports.
   Re-creating a witness because it is unhealthy is not reconciliation, it is a
   loop, and it breaks the idempotency rule outright.

   SECOND, the clear took [0] of a collection that can hold several, so one was
   removed per pass while the apply added one and the set could never shrink. */

func TestAConfiguredWitnessIsNotReAppliedJustBecauseItIsFailed(t *testing.T) {
	// The path has to be read from the RESOURCES too, not only from the quorum
	// object, or a failed witness reads as absent and is created again.
	if !strings.Contains(witnessScript, "if (-not $curPath -and $fsw.Count -gt 0)") {
		t.Fatal("the configured path must also be read from the witness resources; the quorum object does not always give it up for a failed one")
	}
	i := strings.Index(witnessScript, "if (-not $curPath -and $fsw.Count -gt 0)")
	j := strings.Index(witnessScript, "(Norm $curPath) -ne (Norm $desired)")
	if j < 0 || i > j {
		t.Fatal("the resource-side read must happen before the comparison that decides whether to apply")
	}
}

func TestEveryStuckWitnessResourceIsRemovedNotJustTheFirst(t *testing.T) {
	// [0] on a collection that can hold several is how a set of duplicates
	// survives every pass: one removed, one added.
	if strings.Contains(witnessScript, "ResourceType -eq 'File Share Witness' })[0]") {
		t.Fatal("taking [0] removes one duplicate per pass while the apply adds one; the set never shrinks")
	}
	if !strings.Contains(witnessScript, "foreach ($r in $still)") {
		t.Fatal("the clear must iterate every stuck witness resource")
	}
}

/*
The duplicates are cleaned up by identity, not by name.

	Simulated against the rig's nine before shipping: they all carry the same
	resource NAME, so matching the keeper on its name matched every one of them
	and the loop removed nothing at all. Id is what distinguishes two resources;
	the name is a label they can share.
*/
func TestDuplicateWitnessesAreMatchedByIdNotName(t *testing.T) {
	if !strings.Contains(witnessScript, "$keepId") {
		t.Fatal("the keeper must be identified by Id; nine resources shared one name on the rig")
	}
	i := strings.Index(witnessScript, "if ($keepId -ne '' -and $rid -ne '')")
	if i < 0 {
		t.Fatal("the Id comparison must be the primary match")
	}
	// And with nothing to match on, it must keep one rather than empty the set.
	if !strings.Contains(witnessScript, "if (-not $kept) { $dupes = 0 }") {
		t.Fatal("if no keeper was identified the set must be left alone, not emptied")
	}
}

// A witness that is ONLINE is casting a vote. Nothing here may tear one down.
func TestAnOnlineWitnessIsNeverTornDown(t *testing.T) {
	if !strings.Contains(witnessScript, "$anyOnline") {
		t.Fatal("the clear must check that none of the witness resources is online before removing any")
	}
}
