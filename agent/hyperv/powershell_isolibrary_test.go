package hyperv

import (
	"context"
	"strings"
	"testing"
)

/* The two reads answer different questions and routinely disagree.

   The agent runs as a service account; Hyper-V attaches media from VMMS, which
   runs as LocalSystem and therefore reaches the share as the node's COMPUTER
   ACCOUNT. A share granted to a user lists perfectly for the agent and cannot
   boot a VM — the ordinary result of "I gave my account access and it still
   doesn't work", and invisible unless both are tested.

   The witness cost a day teaching this: an ordinary remote probe reached the NAS
   anonymously because the credential could not double-hop, and false-failed a
   share that was fine. */

func TestISOLibraryProbeTestsTheComputerAccountNotJustTheAgent(t *testing.T) {
	s := isoLibraryScript(`\\nas.lab.local\isos`, "", "")

	if !strings.Contains(s, "Get-ChildItem -LiteralPath $share -Filter *.iso") {
		t.Error("the agent-context read must list the ISOs")
	}
	// The computer-account read has to actually run as LocalSystem. Anything
	// short of that answers a different question.
	for _, want := range []string{
		"Register-ScheduledTask",
		"-UserId 'SYSTEM'",
		"LogonType ServiceAccount",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the computer-account probe must run as LocalSystem; missing %q", want)
		}
	}
	// And it must clean up after itself, or a failed probe leaves a scheduled
	// task on every host it ran on.
	if !strings.Contains(s, "Unregister-ScheduledTask") || !strings.Contains(s, "finally {") {
		t.Error("the probe task must be removed in a finally, not only on the happy path")
	}
}

// The computer account is the identity that attaches the media, so it is also
// the right identity to list what is attachable.
//
// Listing only as the agent broke the configuration this feature itself asks
// for: a share granted to Domain Computers and not to the agent's service
// account showed no images and reported unreachable, while Hyper-V could boot
// from it perfectly. Found on live hardware within minutes of shipping.
func TestISOLibraryProbeListsAsTheComputerAccountToo(t *testing.T) {
	s := isoLibraryScript(`\\nas.lab.local\iso`, "", "")

	// The SYSTEM probe must return the file NAMES, not merely a count.
	if !strings.Contains(s, "-Filter *.iso -File -Force") {
		t.Error("the computer-account probe must enumerate the ISOs, not just prove the path resolves")
	}
	if !strings.Contains(s, "isos = $f") {
		t.Error("the computer-account probe must return the file names it found")
	}
	// And those names must be used when the agent could not read the share,
	// or the listing is empty for exactly the grant this feature recommends.
	if !strings.Contains(s, "if ((-not $out.readable) -or ($out.isos.Count -eq 0)) { $out.isos = $mi }") {
		t.Error("the machine's listing must be used when the agent's read failed")
	}
}

// Granted to the machines and not to the agent is the NORMAL outcome of
// following the advice this feature gives. It must read as working, with one
// line of explanation — not as a fault.
func TestISOLibraryProbeDoesNotTreatTheRecommendedGrantAsAFailure(t *testing.T) {
	s := isoLibraryScript(`\\nas\iso`, "", "")
	if !strings.Contains(s, "(-not $out.readable) -and $out.machineReadable -eq $true") {
		t.Fatal("the machine-yes/agent-no case must be recognised explicitly")
	}
	if !strings.Contains(s, "VMs boot from it normally") {
		t.Error("that case must say it works, since it does")
	}
}

// Unknown is a third state. A probe that could not run says nothing about the
// share, and reporting false would condemn a working library — the same
// absence-of-observation-is-not-observation-of-absence rule as everywhere else.
func TestISOLibraryProbeLeavesUnknownUnknown(t *testing.T) {
	s := isoLibraryScript(`\\nas\isos`, "", "")
	if !strings.Contains(s, "machineReadable = $null") {
		t.Fatal("machineReadable must start unknown, not false")
	}
	// Only a probe that actually reported may set it either way.
	okAt := strings.Index(s, "$out.machineReadable = $true")
	noAt := strings.Index(s, "$out.machineReadable = $false")
	testPathAt := strings.Index(s, "if (Test-Path -LiteralPath $tmp)")
	if okAt == -1 || noAt == -1 || testPathAt == -1 {
		t.Fatal("both verdicts must be gated on the probe having written a result")
	}
	if okAt < testPathAt || noAt < testPathAt {
		t.Error("a verdict is reachable without the probe having reported")
	}
	// A result file that cannot be parsed is not a verdict either: $res stays
	// null and neither branch runs.
	if !strings.Contains(s, "} elseif ($res) {") {
		t.Error("an unparseable result must leave the verdict unknown, not false")
	}
}

// The remedy is outside what Ballast administers — it cannot grant rights on
// someone's NAS — so it must name the step precisely rather than failing
// obscurely. "Grant a user access" is the wrong advice and the common mistake.
func TestISOLibraryProbeNamesTheGrantThatIsMissing(t *testing.T) {
	s := isoLibraryScript(`\\nas\isos`, "", "")
	for _, want := range []string{
		"computer account",
		"Grant the node computer accounts",
		"domain-joined",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the failure must name the actual remedy; missing %q", want)
		}
	}
}

func TestCheckISOLibraryParsesTheProbe(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{
		[]byte(`{"readable":true,"machineReadable":false,"message":"grant the computer accounts","isos":["w2025.iso","rocky10.iso"]}`),
	}}
	got, err := newTestPS(f).CheckISOLibrary(context.Background(), `\\nas\isos`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Readable {
		t.Error("readable was reported true")
	}
	if got.MachineReadable == nil || *got.MachineReadable {
		t.Errorf("machineReadable false must survive as false, got %v", got.MachineReadable)
	}
	if len(got.ISOs) != 2 || got.ISOs[0] != "w2025.iso" {
		t.Errorf("ISOs mangled: %v", got.ISOs)
	}
	if got.Path != `\\nas\isos` {
		t.Errorf("the path must be echoed back, got %q", got.Path)
	}
}

// An omitted machineReadable must decode as unknown, not false.
func TestCheckISOLibraryKeepsAnOmittedVerdictUnknown(t *testing.T) {
	f := &fakeRunner{responses: [][]byte{[]byte(`{"readable":true,"isos":[]}`)}}
	got, err := newTestPS(f).CheckISOLibrary(context.Background(), `\\nas\isos`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.MachineReadable != nil {
		t.Fatalf("an omitted verdict must stay unknown, got %v", *got.MachineReadable)
	}
}

// No library declared is not a probe worth running, and must not error.
func TestCheckISOLibraryWithNoPathDoesNothing(t *testing.T) {
	f := &fakeRunner{}
	got, err := newTestPS(f).CheckISOLibrary(context.Background(), "  ", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Readable || got.MachineReadable != nil || len(f.calls) != 0 {
		t.Fatalf("an undeclared library must not be probed, got %+v after %d calls", got, len(f.calls))
	}
}
