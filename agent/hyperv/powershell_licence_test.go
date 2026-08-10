package hyperv

import (
	"fmt"
	"strings"
	"testing"
)

/* These assert the shape of the generated PowerShell rather than its effect, which
   is the only thing testable off a Hyper-V host. They exist because the failure
   they guard against was silent in exactly that way: the conversion script called
   a cmdlet with a parameter it does not have, and nothing said so until it ran on
   a real host against a real evaluation edition. */

func editionScriptFor(target string) string {
	return readTargetEditionsFunc + fmt.Sprintf(editionScript, psQuote(target))
}

// Set-WindowsEdition services an offline IMAGE and takes -Path. It has no -Online
// parameter set, so an online conversion has to go through DISM.exe — and the call
// that was here passed no -Edition either, so it would have converted to nothing
// even on a build that accepted -Online.
func TestTheConversionDoesNotUseTheOfflineCmdlet(t *testing.T) {
	s := editionScriptFor("ServerDatacenter")

	// Comment lines are skipped: the script explains why it does not use the
	// cmdlet, and naming it there is the point.
	for _, line := range strings.Split(s, "\n") {
		code := strings.TrimSpace(line)
		if !strings.HasPrefix(code, "#") && strings.Contains(code, "Set-WindowsEdition") {
			t.Errorf("Set-WindowsEdition is offline-only; the online conversion must use DISM.exe: %q", code)
		}
	}
	if !strings.Contains(s, "/Set-Edition:") {
		t.Error("the conversion must call DISM with /Set-Edition:")
	}
	if !strings.Contains(s, "/AcceptEula") {
		t.Error("DISM refuses the conversion without /AcceptEula")
	}
	// The reboot is the reconciler's to schedule under RebootPolicy, not DISM's to
	// take on its own.
	if !strings.Contains(s, "/NoRestart") {
		t.Error("the conversion must not restart the host itself")
	}
}

// 3010 is ERROR_SUCCESS_REBOOT_REQUIRED: a success DISM reports with a non-zero
// code. Treating it as a failure reports nothing staged when the conversion is in
// fact waiting on the restart.
func TestARebootRequiredExitIsTreatedAsSuccess(t *testing.T) {
	s := editionScriptFor("ServerDatacenter")
	if !strings.Contains(s, "3010") {
		t.Fatal("exit 3010 must be accepted, or every successful conversion reports as failed")
	}
}

// The target must reach DISM. An earlier version interpolated it into the refusal
// message and nowhere else, which is the kind of gap that only shows up on a host.
func TestTheTargetEditionReachesDISM(t *testing.T) {
	s := editionScriptFor("ServerDatacenterCor")
	if !strings.Contains(s, "'ServerDatacenterCor'") {
		t.Fatalf("the target edition is not in the script at all")
	}
	if !strings.Contains(s, "('/Set-Edition:' + $target)") {
		t.Error("the target must be passed to DISM, not only named in messages")
	}
}

// The key is a product key: it must not be interpolated into the script text,
// which is what appears in an error message and in a log line. It arrives in the
// environment instead. (It does reach DISM's own command line, because DISM takes
// it no other way — but that is Windows' log, not Ballast's.)
func TestTheProductKeyIsNotWrittenIntoTheScript(t *testing.T) {
	s := editionScriptFor("ServerDatacenter")
	if !strings.Contains(s, "$env:BALLAST_PRODUCT_KEY") {
		t.Error("the key must come from the environment")
	}
}

// Both scripts call Read-TargetEditions, so both must carry its definition. A
// missing function in PowerShell is not a parse error — the call returns nothing
// and the script carries on, so the conversion would silently stop checking
// whether Windows allows it.
func TestBothScriptsCarryTheTargetEditionsHelper(t *testing.T) {
	for _, c := range []struct {
		name, script string
	}{
		{"conversion", editionScriptFor("ServerDatacenter")},
		{"observation", readTargetEditionsFunc + licenceScript},
	} {
		if !strings.Contains(c.script, "function Read-TargetEditions") {
			t.Errorf("%s script calls Read-TargetEditions without defining it", c.name)
		}
	}
}

// A read that fails must leave the list EMPTY, not assert that no conversion is
// possible. The console falls back to free text on an empty list; a refusal
// synthesised from a failed read would block a conversion Windows would allow.
func TestTheTargetEditionsReadSwallowsItsFailures(t *testing.T) {
	if !strings.Contains(readTargetEditionsFunc, "catch {}") {
		t.Error("a failed target-editions read must yield an empty list, not an error")
	}
	if !strings.Contains(editionScript, "$valid.Count -gt 0") {
		t.Error("an empty list must not be read as a refusal")
	}
}

// A PowerShell backtick escape cannot appear in a Go raw string, so a line break
// written that way silently becomes something else.
func TestNoScriptCarriesABacktickEscape(t *testing.T) {
	for name, s := range map[string]string{
		"edition":     editionScript,
		"licence":     licenceScript,
		"activation":  activationScript,
		"targetreads": readTargetEditionsFunc,
	} {
		if strings.Contains(s, "`") {
			t.Errorf("%s script contains a backtick, which a Go raw string cannot carry", name)
		}
	}
}
