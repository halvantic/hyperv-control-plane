package hyperv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A single-item lookup must never be written "| Select-Object -First 1".
//
// PowerShell's -First sends a stop signal upstream, and after a FailoverClusters
// cmdlet that aborts the ENTIRE script: the assignment never completes, the
// remaining lines never run, and the exit code is 0. The Go side sees a clean
// run with no output. That is how the green-but-absent replica survived for
// days (b5fae76) — Enable-VMReplication was never reached, and every pass
// reported AlreadyConfigured.
//
// @(...)[0] is the replacement: it drains the pipeline without the stop signal
// and yields $null on empty, exactly as the old form did.
//
// This guard exists because the construct kept coming back. It was 26 sites,
// then 46, then 47 across eleven files, each added by someone reaching for the
// obvious idiom. A test that names the whole agent tree is the only thing that
// holds; the previous guard covered two template scripts and watched the rest
// grow around it.
func TestNoPipelineStopInAgentScripts(t *testing.T) {
	// The construct is only a hazard where it truncates a lookup. -First N over
	// a large or remote stream is a deliberate early exit and is left alone;
	// where one is kept, the line above it says why.
	const banned = "Select-Object -First 1"

	root := ".." // the agent tree
	var offenders []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, banned) {
				continue
			}
			// The comments warning against it name it, and must keep doing so.
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
				continue
			}
			offenders = append(offenders, filepath.ToSlash(path)+":"+itoa(i+1)+"  "+trimmed)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the agent tree: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("%d single-item lookup(s) still use the pipeline stop, which aborts the script with exit 0 and no output.\n"+
			"Rewrite each as @(...)[0]:\n  %s", len(offenders), strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
