package hyperv

import (
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
	if !strings.Contains(s, "already exists under ") || !strings.Contains(s, "A previous copy was left there") {
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
	if !strings.Contains(s, "FileSystemAccessRule($acct, 'Modify'") {
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
	fin, rm := strings.Index(s, "} finally {"), strings.Index(s, "Remove-VM -Name $vm -Force")
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
