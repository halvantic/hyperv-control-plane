package hyperv

import (
	"strings"
	"testing"
)

/* A PowerShell error record rendered to stderr repeats its message up to three
   times and wraps it in positional noise about the script that threw. The
   carefully written sentence — the one naming the cause and the remedy — ends up
   buried in its own echo, which is the "raw error passed through" failure by
   another route: the remedy is there and nobody reads that far. */

const adoptRecord = `the LUN (serial f78ec498, 1000GB) already contains a ReFS volume labelled "iSCSI_DS1", 11.6GB used of 999.9GB. Adopting it formats it, so Ballast will not do that to a disk with contents. If this is the right LUN and its contents are finished with, use "Wipe and adopt"; otherwise correct the serial or present a different LUN.
At line:12 char:21
+ function Fail($m) { throw $m }
+                     ~~~~~~~~
    + CategoryInfo          : OperationStopped: (the LUN (serial... different LUN.:String) [], RuntimeException
    + FullyQualifiedErrorId : the LUN (serial f78ec498, 1000GB) already contains a ReFS volume labelled "iSCSI_DS1"`

func TestTheMessageSurvivesAndTheNoiseDoesNot(t *testing.T) {
	got := tidyPSError(adoptRecord)

	if !strings.HasPrefix(got, "the LUN (serial f78ec498, 1000GB) already contains") {
		t.Fatalf("the message itself must lead: %q", got)
	}
	if !strings.Contains(got, `use "Wipe and adopt"`) {
		t.Error("the remedy is the point of the message and must survive")
	}
	if strings.Contains(got, "At line:") {
		t.Error("the position in a generated script means nothing to an operator")
	}
	if strings.Contains(got, "FullyQualifiedErrorId") || strings.Contains(got, "CategoryInfo") {
		t.Error("the boilerplate must go")
	}
	// The source line names the helper that threw, which says nothing about the host.
	if strings.Contains(got, "function Fail") {
		t.Error("the offending source line must go")
	}
	// And the message must appear once, not twice.
	if strings.Count(got, "already contains") != 1 {
		t.Errorf("the message is repeated: %q", got)
	}
}

// An unrecognised error losing its detail is far worse than a tidy one keeping
// some noise, so anything not matching the known boilerplate is kept.
func TestAnUnfamiliarErrorIsLeftAlone(t *testing.T) {
	in := "Set-ClusterQuorum : There was an error configuring the file share witness"
	if got := tidyPSError(in); got != in {
		t.Fatalf("an ordinary message must pass through unchanged, got %q", got)
	}
}

func TestEmptyStaysEmpty(t *testing.T) {
	if got := tidyPSError("   \r\n  \n"); got != "" {
		t.Fatalf("whitespace is not a message, got %q", got)
	}
}

// PowerShell wraps long messages mid-line; rejoining them must not leave the
// doubled spaces that read as typos.
func TestWrappedLinesRejoinCleanly(t *testing.T) {
	got := tidyPSError("the share answers on SMB but this node\r\n  cannot read it.")
	if strings.Contains(got, "  ") {
		t.Fatalf("doubled spaces left after rejoining: %q", got)
	}
	if got != "the share answers on SMB but this node cannot read it." {
		t.Fatalf("got %q", got)
	}
}
