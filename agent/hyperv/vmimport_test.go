package hyperv

import (
	"strings"
	"testing"
)

/* The scan and import scripts, checked against the code rather than against
   their own comments.

   This has gone wrong three times in this package — a test matched a comment
   naming -ChapSecret, another matched a comment naming Remove-IscsiTargetPortal
   — so every assertion here targets a construct that must EXECUTE, and each one
   is paired with a check that the wrong form is absent. */

func TestScanReadsIdentityRatherThanConstructingIt(t *testing.T) {
	s := scanImportableVMsScript([]string{`C:\ClusterStorage\DS1`}, 200)

	// The VM's name comes from the configuration Compare-VM read, never from the
	// folder. Hyper-V names a VM's folder at CREATION and never renames it, so
	// the directory is a stale label — importing under it would register a VM as
	// something it is not.
	if !strings.Contains(s, "$rec.name = [string]$vm.Name") {
		t.Error("the scan does not take the VM's name from the configuration it read")
	}
	if strings.Contains(s, "$rec.name = [string]$c.Directory") || strings.Contains(s, "Split-Path $c.DirectoryName -Leaf") {
		t.Error("the scan is naming a VM after its folder, which is a label from creation time and not an identity")
	}
}

func TestScanQuotesRootsSafely(t *testing.T) {
	s := scanImportableVMsScript([]string{`C:\Cluster's Storage`}, 200)
	if !strings.Contains(s, `'C:\Cluster''s Storage'`) {
		t.Errorf("an apostrophe in a path was not doubled, so the script would not parse:\n%s", firstLines(s, 6))
	}
}

func TestScanSkipsCheckpointConfigurations(t *testing.T) {
	s := scanImportableVMsScript([]string{`D:\`}, 200)
	// Checkpoint and planned-VM .vmcx files sit alongside the real one. Listing
	// them offers an operator the chance to import a checkpoint as a VM.
	if !strings.Contains(s, `-notmatch 'Virtual Machines$'`) {
		t.Error("the scan does not restrict itself to configurations in a Virtual Machines folder")
	}
}

func TestScanIsCapped(t *testing.T) {
	s := scanImportableVMsScript([]string{`D:\`}, 7)
	if !strings.Contains(s, "$cap = 7") {
		t.Error("the cap was not passed into the script")
	}
	// Reaching the cap must be REPORTED, not silently truncate the list — a
	// partial count presented as complete is the same class of lie as a stale
	// reading presented as current.
	if !strings.Contains(s, "$out.truncated = $true") {
		t.Error("hitting the cap does not set truncated, so a partial list would read as the whole volume")
	}
}

/* The two refusals that stop this feature corrupting somebody's VM. Both are
   enforced in the SCRIPT, because a guard that lives only in the console is not
   a guard: the job can be enqueued through the API, and the state that makes an
   import unsafe changes while the operator is reading the page. */

func TestImportRefusesAVMThisHostAlreadyHas(t *testing.T) {
	s := importVMScript(VMImport{ConfigPath: `C:\ClusterStorage\DS1\DC01\Virtual Machines\x.vmcx`})
	if !strings.Contains(s, "$existing = @(Get-VM") {
		t.Fatal("the import does not check whether this host already has the VM")
	}
	if !strings.Contains(s, "$existing.Count -gt 0") || !strings.Contains(s, "throw") {
		t.Fatal("finding the VM already registered does not refuse the import")
	}
	// Checked against the LIVE host by id, not against a name or a flag the
	// console sent — the console's view is minutes old by the time it is clicked.
	if !strings.Contains(s, "([string]$_.Id) -eq ([string]$vm.Id)") {
		t.Error("the already-registered check does not compare VM ids")
	}
}

func TestImportRefusesFilesAnotherHostHasOpen(t *testing.T) {
	s := importVMScript(VMImport{ConfigPath: `C:\ClusterStorage\DS1\DC01\Virtual Machines\x.vmcx`})
	if !strings.Contains(s, "[System.IO.File]::Open(") || !strings.Contains(s, "'Open', 'ReadWrite', 'None'") {
		t.Fatal("the import does not test whether the VM's disks are open elsewhere")
	}
	if !strings.Contains(s, `if ($busy -ne '' -and -not $doCopy)`) {
		t.Fatal("an open disk does not refuse a register-in-place import")
	}
}

func TestImportChecksCompatibilityBeforeActing(t *testing.T) {
	s := importVMScript(VMImport{ConfigPath: `D:\VM\Virtual Machines\x.vmcx`})
	/* Compare-VM must run before Import-VM, not as a fallback after it failed:
	   the whole point is naming the specific finding instead of "import failed".

	   Matched on the EXECUTABLE forms. The first draft of this test searched for
	   the bare string "Import-VM" and found it inside a comment above the
	   Compare-VM call, reporting the order as wrong when it was right — which is
	   this package's recurring test bug (a test matching a comment rather than
	   the code) arriving in the test written to guard against it. */
	ci := strings.Index(s, "$report = Compare-VM -Path $path")
	ii := strings.Index(s, "$new = Import-VM")
	if ci < 0 || ii < 0 || ci > ii {
		t.Fatalf("Compare-VM does not run before Import-VM (compare at %d, import at %d)", ci, ii)
	}
	if !strings.Contains(s, "$report.Incompatibilities.Count -gt 0") {
		t.Error("the report's findings are not checked")
	}
}

/* Register-in-place and copy fail in opposite, expensive directions. Registering
   when a copy was wanted gives two clusters one set of files; copying when
   registering was wanted rewrites a 500GB VM onto the volume it is already on. */

func TestRegisterInPlacePassesTheReportNotThePath(t *testing.T) {
	s := importVMScript(VMImport{ConfigPath: `D:\VM\Virtual Machines\x.vmcx`, Copy: false})
	if !strings.Contains(s, "Import-VM -CompatibilityReport $report") {
		t.Error("registering in place does not pass the compatibility report, so what is imported is not what was inspected")
	}
	if !strings.Contains(s, "$doCopy = $false") {
		t.Error("copy was not disabled in the script")
	}
}

func TestCopyGeneratesANewIdentity(t *testing.T) {
	s := importVMScript(VMImport{ConfigPath: `D:\VM\Virtual Machines\x.vmcx`, Copy: true})
	if !strings.Contains(s, "$doCopy = $true") {
		t.Error("copy was not enabled in the script")
	}
	// A copy that kept the original id would be the corruption this feature
	// refuses, arriving by the other route.
	if !strings.Contains(s, "-Copy -GenerateNewId") {
		t.Error("a copy does not generate a new identity")
	}
}

func TestClusterRoleIsAddedByIDNotName(t *testing.T) {
	s := importVMScript(VMImport{ConfigPath: `D:\VM\Virtual Machines\x.vmcx`, Cluster: true})
	// Two VMs can carry the same name on one cluster — importing somebody else's
	// DC01 next to your own is exactly what this feature makes possible — so a
	// name lookup would add the role to whichever Windows found first.
	if !strings.Contains(s, "Add-ClusterVirtualMachineRole -VMId $out.id") {
		t.Error("the clustered role is not added by VM id")
	}
	if strings.Contains(s, "Add-ClusterVirtualMachineRole -VMName") {
		t.Error("the clustered role is added by name, which is ambiguous after an import")
	}
}

/* A failed cluster-role add must not report the whole job as failed: the VM IS
   imported by then, and "failed" tells an operator to import again — straight
   into the already-registered refusal. */
func TestAFailedClusterRoleReportsThePartialOutcome(t *testing.T) {
	s := importVMScript(VMImport{ConfigPath: `D:\VM\Virtual Machines\x.vmcx`, Cluster: true})
	if !strings.Contains(s, "$out.note = 'imported, but adding the clustered role failed: '") {
		t.Error("a failed clustered-role add does not report that the import itself succeeded")
	}
}

func TestSavedStateIsSpentOnlyWhenAsked(t *testing.T) {
	no := importVMScript(VMImport{ConfigPath: `D:\VM\Virtual Machines\x.vmcx`})
	if !strings.Contains(no, "$discardSaved = $false") {
		t.Error("saved state would be discarded without being asked for")
	}
	yes := importVMScript(VMImport{ConfigPath: `D:\VM\Virtual Machines\x.vmcx`, DiscardSavedState: true})
	if !strings.Contains(yes, "$discardSaved = $true") || !strings.Contains(yes, "Remove-VMSavedState") {
		t.Error("discarding saved state was asked for and the script does not do it")
	}
}

/* Windows gives a code and a sentence. The brief refuses a raw error passed
   through to an operator when the failure has a known remedy. */
func TestIncompatibilitiesCarryARemedy(t *testing.T) {
	tests := []struct {
		name     string
		code     int32
		msg      string
		wantKind string
		wantIn   string
	}{
		{"missing switch", 33012, `Could not find Ethernet switch 'ConvergedSwitch'.`, "Switch", "Create the switch"},
		{"missing ISO", 40010, `The device at 'D:\iso\win.iso' could not be opened.`, "ISO", "imports and runs without it"},
		{"saved state", 0, `Cannot restore the saved state.`, "SavedState", "starts the VM cold"},
		{"processor", 0, `The processor does not provide a feature.`, "Processor", "processor compatibility"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := explainIncompatibility(tt.code, tt.msg, true)
			if got.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", got.Kind, tt.wantKind)
			}
			if !strings.Contains(got.Remedy, tt.wantIn) {
				t.Errorf("remedy did not say what to do:\n %s", got.Remedy)
			}
			// Windows' own words are kept alongside, never replaced — an operator
			// searching for the message must still find it.
			if got.Message != tt.msg {
				t.Errorf("the original message was lost: %q", got.Message)
			}
		})
	}
}

/* A finding nobody has seen keeps Windows' words and offers NO remedy. Inventing
   one would be worse than silence: a wrong remedy in an infra console gets
   followed. */
func TestAnUnknownFindingInventsNothing(t *testing.T) {
	got := explainIncompatibility(99999, "Something entirely new went wrong.", false)
	if got.Kind != "Other" {
		t.Errorf("kind = %q, want Other", got.Kind)
	}
	if got.Remedy != "" {
		t.Errorf("a remedy was invented for a finding nobody has seen: %q", got.Remedy)
	}
	if got.Message == "" {
		t.Error("Windows' own message was dropped, leaving nothing to search for")
	}
}

func firstLines(s string, n int) string {
	parts := strings.SplitN(s, "\n", n+1)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, "\n")
}
