package hyperv

import (
	"context"
	"strings"
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

/* Moving a VM by copying it.

   The path for two hosts that were not built to know about each other, which is
   what a NEW environment is. It asks for SMB and a stopped guest instead of live
   migration, Kerberos delegation and a compatible destination. */

func copyScript(t *testing.T) string {
	t.Helper()
	return copyVMScript("HVNew01", "HVNEW06", `I:\`, "Secondary", "",
		[]types.EvacuationNIC{{SourceSwitch: "ConvergedSwitch2", TargetSwitch: "Converged"}})
}

/*
The original is removed ONLY after the copy is confirmed on the other side.

	Until then the export is a backup, and deleting the source first would turn a
	failed evacuation into a lost VM.
*/
func TestTheSourceIsRemovedOnlyAfterTheImportIsConfirmed(t *testing.T) {
	s := copyScript(t)

	remove := strings.Index(s, "Remove-VM -Name $vm -Force")
	guard := strings.Index(s, "if (-not $imported)")
	if remove < 0 || guard < 0 {
		t.Fatalf("no removal or no guard before it: %s", s)
	}
	if guard > remove {
		t.Fatalf("the source is removed before the import is confirmed: %s", s)
	}
	if !strings.Contains(s, "has been left here untouched") {
		t.Errorf("an unconfirmed import does not say the VM is safe: %s", s)
	}
}

/*
A running guest cannot be exported, and that is the price of the strategy

	rather than a cmdlet's refusal. The message names the alternative.
*/
func TestARunningGuestIsRefusedWithTheAlternative(t *testing.T) {
	s := copyScript(t)
	if !strings.Contains(s, "if ($state -ne 'Off')") {
		t.Fatalf("a running guest is not checked for: %s", s)
	}
	if !strings.Contains(s, "evacuate with Move instead") {
		t.Errorf("the refusal does not name the alternative: %s", s)
	}
}

/*
The import runs ON THE DESTINATION, which is the whole point.

	Compare-VM's report is resolvable on the host holding the files — that is how
	Ballast already imports a VM whose switch does not exist locally. A report
	from Compare-VM -DestinationHost is not, which is what three failed attempts
	at fixing a live move established.
*/
func TestTheImportRunsWhereTheFilesAre(t *testing.T) {
	s := copyScript(t)

	if !strings.Contains(s, "Invoke-Command -ComputerName $dest") {
		t.Fatalf("the import does not run on the destination: %s", s)
	}
	if !strings.Contains(s, "Compare-VM -Path $cfg") {
		t.Errorf("the import does not compare against the files it is importing: %s", s)
	}
	if !strings.Contains(s, "Import-VM -CompatibilityReport $report") {
		t.Errorf("the import does not use the report it just resolved: %s", s)
	}
	// And the switch it should end up on is remembered BEFORE the disconnect
	// clears it, or there is nothing left to reconnect against.
	if !strings.Contains(s, "$wanted[[string]$src.Id] = [string]$src.SwitchName") {
		t.Errorf("the original switch is not remembered before disconnecting: %s", s)
	}
	if !strings.Contains(s, "Connect-VMNetworkAdapter -VMNetworkAdapter $ad -SwitchName $to") {
		t.Errorf("the adapters are never put on the mapped switch: %s", s)
	}
}

// An unmapped network arrives disconnected here too. A VM quietly on the wrong
// network is worse than one obviously on none.
func TestAnUnmappedNetworkStaysDisconnectedOnImport(t *testing.T) {
	s := copyVMScript("v", "d", `I:\`, "", "", nil)
	if !strings.Contains(s, "if (-not $to) { continue }") {
		t.Fatalf("an unmapped adapter would be attached to something: %s", s)
	}
}

/*
A leftover from an attempt that failed part way is exactly what would be in

	the way, and Export-VM will not write over it. Said plainly rather than
	passed through.
*/
func TestALeftoverExportIsNamedRatherThanOverwritten(t *testing.T) {
	s := copyScript(t)
	// The wording moved on: it now says what the leftover IS and names the
	// action that clears it. See TestTheRefusalNamesWhatIsThereAndWhatToDo.
	if !strings.Contains(s, "already exists under ") || !strings.Contains(s, "left by an earlier copy") {
		t.Fatalf("a leftover export is not detected: %s", s)
	}
}

// The destination's firewall is opened by Ballast here too — the copy depends on
// exactly the same SMB the live move does.
func TestTheCopyOpensTheDestinationFirewallToo(t *testing.T) {
	s := copyScript(t)
	if !strings.Contains(s, "Enable-NetFirewallRule -CimSession $fwSession -DisplayGroup 'File and Printer Sharing'") {
		t.Fatalf("the copy does not open the destination firewall: %s", s)
	}
	if !strings.Contains(s, "admin$") || !strings.Contains(s, "over SMB") {
		t.Errorf("the copy does not check SMB before writing: %s", s)
	}
}

/*
PowerShell 5.1, not 7. The agent runs powershell.exe because the Hyper-V

	modules target it, so a ternary would not parse on any host this ever ran on.
*/
func TestTheScriptIsWindowsPowerShellCompatible(t *testing.T) {
	s := copyScript(t)
	if strings.Contains(s, " ? (") || strings.Contains(s, ") : ") {
		t.Errorf("the script uses a PowerShell 7 ternary: %s", s)
	}
	if strings.Contains(s, "??") {
		t.Errorf("the script uses a PowerShell 7 null-coalescing operator: %s", s)
	}
}

// A cluster destination adds the role as part of the import, on the host that
// is joining it.
func TestACopyToAClusterAddsTheRoleAtTheDestination(t *testing.T) {
	s := copyVMScript("v", "d", `I:\`, "", "Primary1", nil)
	if !strings.Contains(s, "Add-ClusterVirtualMachineRole -Cluster $clusterName") {
		t.Fatalf("a cluster destination does not gain the role: %s", s)
	}
	if !strings.Contains(s, "'Primary1'") {
		t.Errorf("the target cluster is not passed in: %s", s)
	}
	// A standalone destination is sent no cluster cmdlets at all.
	if plain := copyVMScript("v", "d", `I:\`, "", "", nil); strings.Contains(plain, "'Primary1'") {
		t.Errorf("a standalone destination carries a cluster name: %s", plain)
	}
}

/*
Export-VM writes as the COMPUTER ACCOUNT, not as the agent.

	VMMS runs as LocalSystem and reaches the network as this host's machine
	account, which is not a local administrator on the destination — so the admin
	share refuses it:

	  Failed to copy file ... to '\HVNEW06\I$\...': Access is denied. (0x80070005)

	The same trap the ISO library carries a note about: a share has to grant the
	node, not the operator.
*/
func TestTheExportGoesThroughAShareGrantedToTheComputerAccount(t *testing.T) {
	s := copyScript(t)

	if !strings.Contains(s, "$srcAcct = $env:COMPUTERNAME + '$'") {
		t.Fatalf("the source computer account is never worked out: %s", s)
	}
	if !strings.Contains(s, "New-SmbShare -Name $name -Path $p -FullAccess $grantees -Temporary") {
		t.Fatalf("no share is made for the export: %s", s)
	}
	// The admin share is what failed; the export must not still be aimed at it.
	if strings.Contains(s, `$unc   = '\\' + $dest + '\' + $drive + '$'`) {
		t.Errorf("the export still writes to the admin share: %s", s)
	}
	if !strings.Contains(s, `$unc = '\\' + $dest + '\' + $shareName`) {
		t.Errorf("the export does not use the share it just made: %s", s)
	}
}

/*
Share permission AND NTFS. Granting one and not the other is the half of this

	that looks configured and still denies the write.
*/
func TestBothTheShareAndTheFilesystemAreGranted(t *testing.T) {
	s := copyScript(t)
	if !strings.Contains(s, "FileSystemAccessRule($g, 'Modify'") {
		t.Fatalf("NTFS is not granted, only the share: %s", s)
	}
	if !strings.Contains(s, "Set-Acl -LiteralPath $p") {
		t.Errorf("the filesystem ACL is never applied: %s", s)
	}
}

/*
The share comes down on every exit, including a failed one.

	A volume left shared to a computer account is a thing nobody would think to
	look for. Temporary as well, so a host that restarts mid-move is not left
	sharing a volume nobody meant to share.
*/
func TestTheShareIsRemovedOnEveryExit(t *testing.T) {
	s := copyScript(t)

	if !strings.Contains(s, "} finally {") {
		t.Fatalf("the share is not removed on a failed export: %s", s)
	}
	if !strings.Contains(s, "Remove-SmbShare -Force") {
		t.Errorf("the share is never removed: %s", s)
	}
	if !strings.Contains(s, "-Temporary") {
		t.Errorf("the share would survive a restart: %s", s)
	}
	// And the removal must come after the source is taken away, or a failure
	// there would leave the share behind.
	//
	// LastIndex, not Index: the export now has a finally of its own to clean up
	// the job it watches, and that one is nested INSIDE this block. The share's
	// is the outermost, so it is the last to open.
	fin, rm := strings.LastIndex(s, "} finally {"), strings.Index(s, "Remove-VM -Name $vm -Force")
	if rm < 0 || fin < rm {
		t.Errorf("the finally block does not wrap the whole move: %s", s)
	}
}

// The UNC is built the way the rest of this package builds one: '\' + host,
// then single separators. Getting that wrong produces a path that looks almost
// right and resolves to nothing.
func TestTheShareUNCIsWellFormed(t *testing.T) {
	s := copyScript(t)
	for _, bad := range []string{`'\\\\' + $dest`, `+ '\\' + $shareName`} {
		if strings.Contains(s, bad) {
			t.Errorf("malformed UNC (%s): %s", bad, s)
		}
	}
}

/*
The share grants BOTH accounts that touch it.

	Export-VM writes as the computer account, so that one must be granted or the
	copy is denied. But the agent reads the folder afterwards to find the
	exported configuration, and a share granted only to the machine account
	denies its own creator:

	  Test-Path : Access is denied

	against a share Ballast had just made. Two different identities do two
	different halves of this, and granting one of them is the version that looks
	configured and fails on the other half.
*/
func TestBothTheComputerAndTheAgentAccountsAreGranted(t *testing.T) {
	s := copyScript(t)

	if !strings.Contains(s, "$agentAcct = $env:USERNAME") {
		t.Fatalf("the agent's own account is never worked out: %s", s)
	}
	if !strings.Contains(s, "$grantees = @($acct, $agent)") {
		t.Fatalf("the share is granted to one account only: %s", s)
	}
	if !strings.Contains(s, "New-SmbShare -Name $name -Path $p -FullAccess $grantees") {
		t.Errorf("the share does not grant both: %s", s)
	}
	// NTFS for both as well — granting the share alone is the other half that
	// looks configured and still denies.
	if !strings.Contains(s, "foreach ($g in $grantees) {") {
		t.Errorf("the filesystem is granted to one account only: %s", s)
	}
}

/* The export has to report while it runs.

   Export-VM is synchronous and silent, so a copy of any real size looked
   exactly like a hung one: "exporting HVNew01 to HVNEW06", then nothing, then a
   failure naming a time limit and not a single byte. An operator could not tell
   a slow link from a stuck job — the one distinction that decides whether to
   wait or intervene. */
func TestTheExportReportsProgressWhileItRuns(t *testing.T) {
	s := copyVMScript("HVNew01", "HVNEW06", "D:\\VMs", "", "", nil)

	// Watched, not awaited. A synchronous Export-VM cannot report anything.
	if !strings.Contains(s, "Start-Job -ArgumentList $vm, $unc") {
		t.Fatalf("the export is not run as a child, so nothing can watch it:\n%s", s)
	}
	// The note is assembled first so a stalled figure can have "unchanged" added
	// to it, so the literal is the assembly, not the emission.
	if !strings.Contains(s, "$note = 'copied ' + [string]$doneGB + ' GB of '") {
		t.Errorf("no bytes are ever reported:\n%s", s)
	}
	if !strings.Contains(s, "Measure-Object -Property Length -Sum") {
		t.Errorf("progress is not measured from what has landed on the destination:\n%s", s)
	}

	/* A child job does not inherit $ErrorActionPreference. This package has
	   already shipped one bug where a job swallowed its own errors and the
	   caller reported success, so both halves are pinned: set inside the block,
	   and received afterwards. */
	if !strings.Contains(s, "param($v, $u)\n  $ErrorActionPreference = 'Stop'") {
		t.Errorf("the child job does not set its own error preference, so a failed export reports success:\n%s", s)
	}
	if !strings.Contains(s, "Receive-Job -Job $job -Wait -ErrorAction Stop") {
		t.Errorf("the child's failure is never re-thrown, so the copy imports nothing:\n%s", s)
	}
	if !strings.Contains(s, "Remove-Job -Job $job -Force") {
		t.Errorf("the job is not cleaned up:\n%s", s)
	}
}

/* The denominator is the file length, not the virtual size.

   A dynamic 500GB disk holding 40GB copies 40GB. Reported against 500 it would
   sit near eight percent for the whole run and read as stalled — a progress bar
   that lies is worse than none, because it is the thing being used to decide
   whether the job is stuck. */
func TestProgressIsMeasuredAgainstWhatIsActuallyCopied(t *testing.T) {
	s := copyVMScript("HVNew01", "HVNEW06", "D:\\VMs", "", "", nil)
	if !strings.Contains(s, "$totalBytes += [int64]$f.Length") {
		t.Fatalf("the total is not taken from the file length:\n%s", s)
	}
	// The call, not the word: the comment above the total names Get-VHD to say
	// what it deliberately does NOT use.
	if strings.Contains(s, "Get-VHD -") {
		t.Errorf("the total comes from the virtual size, so a dynamic disk reports as stalled:\n%s", s)
	}
	// A line repeating the same number makes a console look busy while nothing
	// is happening, which is the failure this whole change exists to end.
	if !strings.Contains(s, "$done -ne $lastBytes") {
		t.Errorf("progress is reported even when it has not moved:\n%s", s)
	}
}

/* The destination's path is text here, not a path.

   Join-Path resolves the drive qualifier against the machine RUNNING it, and
   this script runs on the source. Copying HVNew01 to HVNEW06, whose storage is
   I: and whose source host's is not, it threw:

     Join-Path : Cannot find drive. A drive with the name 'I' does not exist.

   The whole point of the copy strategy is that the two hosts share nothing, so
   assuming the destination's drives exist here is the one assumption it cannot
   make. */
func TestTheDestinationPathIsNeverResolvedLocally(t *testing.T) {
	s := copyVMScript("HVNew01", "HVNEW06", "I:\\", "", "", nil)

	if strings.Contains(s, "Join-Path $path") {
		t.Fatalf("the destination path is joined locally, so any drive this host lacks fails the copy:\n%s", s)
	}
	if !strings.Contains(s, "$cfgLocal = $path.TrimEnd('\\') + '\\' + $rel") {
		t.Errorf("the destination path is not built as a string:\n%s", s)
	}
	// A UNC has no drive to resolve, so the share side may keep using Join-Path.
	if !strings.Contains(s, "$target = Join-Path $unc $vm") {
		t.Errorf("the share path stopped using Join-Path, which was not the problem:\n%s", s)
	}
}

/* A compatibility report is a snapshot, so fixing it settles nothing until it
   is asked again.

   The import disconnected all four of HVNew01's adapters successfully and then
   read the ORIGINAL report, which still listed the four problems the disconnect
   had just solved:

     this host cannot take the VM even with its networks disconnected:
     Could not find Ethernet switch 'ConvergedSwitch2'. | ... (x4)

   Compare-VM -CompatibilityReport is the documented way to ask again. This is
   also the one real difference from the MOVE path, where fixing a report does
   not take at all -- see movecompat.go. */
func TestTheImportAsksTheReportAgainAfterFixingIt(t *testing.T) {
	s := copyVMScript("HVNew01", "HVNEW06", "I:\\", "", "",
		[]types.EvacuationNIC{{SourceSwitch: "ConvergedSwitch2", TargetSwitch: "Converged"}})

	if !strings.Contains(s, "$report = Compare-VM -CompatibilityReport $report") {
		t.Fatalf("the report is never refreshed, so every fixed problem still reads as a problem:\n%s", s)
	}
	// Order matters: refreshed AFTER the disconnects, and read BEFORE the throw.
	dis := strings.Index(s, "Disconnect-VMNetworkAdapter -VMNetworkAdapter $src")
	again := strings.Index(s, "$report = Compare-VM -CompatibilityReport $report")
	// The throw, not the phrase: the comment above the refresh quotes the
	// failure it explains, and that copy of the words comes first in the file.
	refuse := strings.Index(s, "throw ('this host cannot take the VM")
	if dis < 0 || again < dis {
		t.Errorf("the report is refreshed before the adapters are fixed, which asks the same question twice:\n%s", s)
	}
	if refuse < 0 || refuse < again {
		t.Errorf("the refusal is decided on the stale report:\n%s", s)
	}
}

/* Progress notes: a bare percentage is decorated, prose is not.

   The handler was written when the only payload was a number polled out of a
   Move-VM job, so it wrapped every line as "live migration N%". Three
   operations share it now and most emit prose, which reached the console as

     live migration copied 50 GB of 50 GB (100%)%

   The stray percent is the visible half. The damaging half is calling a COPY a
   live migration: they are different operations with different costs, and the
   copy says so itself by refusing to run unless the guest is off. Telling an
   operator their stopped guest is live-migrating is the console contradicting
   the thing it just did. */
func TestOnlyABarePercentageIsDecorated(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"42", "live migration 42%"},
		{"100", "live migration 100%"},
		{"copied 50 GB of 50 GB (100%)", "copied 50 GB of 50 GB (100%)"},
		{"exporting HVNew01 to HVNEW06 (50 GB)", "exporting HVNew01 to HVNEW06 (50 GB)"},
		{"made a temporary share on HVNEW06", "made a temporary share on HVNEW06"},
	} {
		if got := progressNote(c.in); got != c.want {
			t.Errorf("progressNote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

/* Clearing a leftover export.

   The copy refuses to write a second export over a first, because that is how a
   half-copy becomes an unreadable one. Until now the refusal ended at "remove
   it" — an instruction to open a session on the destination, which the brief
   calls a defect rather than a runbook step, and which lands at the worst
   moment: mid-evacuation, over a mess that is Ballast's own. */
func TestTheRefusalNamesWhatIsThereAndWhatToDo(t *testing.T) {
	s := copyVMScript("HVNew01", "HVNEW06", "I:", "", "", nil)

	// Complete or part-written decides whether discarding costs the whole copy
	// again, and that is the operator's call, not a detail to withhold.
	if !strings.Contains(s, "'a complete export'") || !strings.Contains(s, "'a part-written export'") {
		t.Errorf("the refusal does not say whether the leftover is finished:\n%s", s)
	}
	if !strings.Contains(s, "Clear leftover export") {
		t.Errorf("the refusal does not name the action that fixes it:\n%s", s)
	}
	// Named exactly as the console names it, because the message is the only
	// thing telling an operator where to look.
	if !strings.Contains(s, "left by an earlier copy") {
		t.Errorf("the console cannot recognise this failure from its message:\n%s", s)
	}
}

func TestClearingRefusesFilesAVMIsRegisteredAgainst(t *testing.T) {
	p := &PowerShell{}
	var script string
	p.run = func(_ context.Context, s string) ([]byte, error) { script = s; return []byte("removed 1 GB"), nil }

	if _, err := p.ClearVMExport(context.Background(), "HVNew01", "HVNEW06", "I:"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	/* A VM imported from this very export looks exactly like rubbish on disk and
	   is the one thing here that must never be deleted. Refusing costs a retry;
	   being wrong costs a VM. */
	if !strings.Contains(script, "foreach ($existing in @(Get-VM -ErrorAction SilentlyContinue))") {
		t.Fatalf("nothing checks whether a VM owns these files:\n%s", script)
	}
	if !strings.Contains(script, "is registered against the files under") {
		t.Errorf("the refusal does not say why it will not delete them:\n%s", script)
	}
	if !strings.Contains(script, "Remove-Item -LiteralPath $target -Recurse -Force") {
		t.Errorf("nothing is ever removed:\n%s", script)
	}
	/* The files are local to the destination, so the work runs there. A hop to
	   oneself needs WinRM, rights and a working loopback to do nothing, and that
	   is the arrangement most likely to be missing on a host just built. */
	if !strings.Contains(script, "$dest.ToLower() -ne $env:COMPUTERNAME.ToLower()") {
		t.Errorf("the agent hops to itself when it IS the destination:\n%s", script)
	}
}

/* Every path this script builds keeps its separator.

   Shipped once without one: "$root.TrimEnd('') + '' + $v" renders I:HVNew01,
   which resolves to nothing, so clearing a leftover would have reported there
   was nothing there. It is invisible in review — the line reads correctly at a
   glance and the missing character is the whole meaning. */
func TestBuiltPathsKeepTheirSeparator(t *testing.T) {
	sep := "\\"
	copyScript := copyVMScript("HVNew01", "HVNEW06", "I:"+sep, "", "", nil)
	if !strings.Contains(copyScript, "$cfgLocal = $path.TrimEnd('"+sep+"') + '"+sep+"' + $rel") {
		t.Errorf("the destination config path is built without a separator:\n%s", copyScript)
	}

	p := &PowerShell{}
	var clear string
	p.run = func(_ context.Context, s string) ([]byte, error) { clear = s; return []byte("ok"), nil }
	if _, err := p.ClearVMExport(context.Background(), "HVNew01", "HVNEW06", "I:"+sep); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !strings.Contains(clear, "$target = $root.TrimEnd('"+sep+"') + '"+sep+"' + $v") {
		t.Errorf("the target path is built without a separator, so it resolves to nothing:\n%s", clear)
	}
}

/* Silence and work look the same, so the copy says which it is.

   Reporting only movement was half right: it stops a console looking busy while
   nothing happens, and it also makes "finished the bytes, now doing something
   else" indistinguishable from "wedged". Observed: 50 GB of 50 GB eleven
   seconds in, then six and a half minutes of nothing, with the import running
   silently the whole time. */
func TestTheCopySaysSomethingWhileItIsQuiet(t *testing.T) {
	s := copyVMScript("HVNew01", "HVNEW06", "I:\\", "", "", nil)

	// A stalled figure is still reported, with how long it has been still.
	if !strings.Contains(s, "unchanged, still exporting") {
		t.Errorf("a copy that stops moving goes silent:\n%s", s)
	}
	if !strings.Contains(s, "(-not $moved -and $quiet -ge 60)") {
		t.Errorf("the quiet heartbeat is not on its own longer interval:\n%s", s)
	}
	// The end of the export is an event worth naming: it is the moment the
	// bytes stop and the silent phase begins.
	if !strings.Contains(s, "PROGRESS export finished, ") {
		t.Errorf("nothing marks the end of the copy:\n%s", s)
	}
	// And the import, which reports nothing of its own at all.
	if !strings.Contains(s, "PROGRESS still importing on ") {
		t.Errorf("the import phase is silent, which is where the copy was last seen sitting:\n%s", s)
	}
	if !strings.Contains(s, "$importJob = Start-Job") {
		t.Errorf("the import is awaited rather than watched, so it cannot report:\n%s", s)
	}
}
