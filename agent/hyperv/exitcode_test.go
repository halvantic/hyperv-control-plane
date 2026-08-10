package hyperv

import (
	"strings"
	"testing"
)

/* powershell.exe -Command takes its exit code from $? at the end. A script whose
   LAST statement emitted a suppressed non-terminating error therefore exits 1
   having written nothing at all — no stderr, no stdout, no event log entry —
   which is indistinguishable from a real failure and impossible to diagnose.

   Reproduced exactly:

       powershell -NoProfile -Command "Get-Item 'C:\nope' -ErrorAction SilentlyContinue"
       exit code: 1, output: ''

   It is not a corner case. The natural way to verify a removal is to re-read and
   expect nothing:

       $still = Get-VM -Name $vm -ErrorAction SilentlyContinue
       if ($still) { throw ... }

   where the VM being GONE is the success condition, and Get-VM not finding it
   leaves $? false. Ballast reported Failed for VM and replica deletions that had
   completed — on the rig, repeatedly, over several days, while the VMs were in
   fact already deleted. */

func TestReachingTheEndOfAScriptMeansSuccess(t *testing.T) {
	got := withExplicitSuccess("Get-VM -Name 'x' -ErrorAction SilentlyContinue")

	if !strings.HasSuffix(got, "\nexit 0") {
		t.Fatalf("a script that runs to the end must exit 0 explicitly, got %q", got)
	}
	if !strings.HasPrefix(got, "Get-VM") {
		t.Fatal("the original script must be preserved ahead of it")
	}
}

// The exit must be on its own line. Appended to the last line it would attach to
// whatever that line was — inside an if, a foreach, or a comment, where it either
// changes the meaning or never runs.
func TestTheExitIsOnItsOwnLine(t *testing.T) {
	got := withExplicitSuccess("# a trailing comment")
	if !strings.Contains(got, "\nexit 0") {
		t.Fatal("appended to a comment line, the exit would be commented out")
	}
	lines := strings.Split(got, "\n")
	if strings.TrimSpace(lines[len(lines)-1]) != "exit 0" {
		t.Fatalf("the exit must be the final statement, got %q", lines[len(lines)-1])
	}
}

// A failing script must still fail. Every script here signals failure by
// throwing, and $ErrorActionPreference is Stop, so a throw terminates before the
// exit is ever reached — the guard is that the exit is LAST, never earlier.
func TestTheExitCannotPreEmptAThrow(t *testing.T) {
	script := "$ErrorActionPreference='Stop'\nthrow 'no'\nWrite-Output 'unreachable'"
	got := withExplicitSuccess(script)

	if strings.Index(got, "exit 0") < strings.Index(got, "throw 'no'") {
		t.Fatal("the exit must come after everything, or it would mask a failure")
	}
}
