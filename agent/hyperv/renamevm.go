package hyperv

import (
	"context"
	"fmt"
	"strings"
)

/* Renaming a VM, and the cluster group that carries its name.

   A rename is not a spec edit. Desired state is keyed on the VM's name and the
   agent matches VMs by name, so changing the name in desired state alone leaves
   the host holding a machine called the old thing while the reconciler creates a
   brand new one under the new name — on the same VHDX files. The attach fails
   with "being used by another process", which the VM reconcile deliberately
   tolerates as transient, so what an operator gets is a phantom empty VM and no
   error at all.

   So the machine is renamed first, by this job, and the centre re-keys desired
   state only once it has succeeded.

   THE CLUSTER GROUP MUST FOLLOW. Add-ClusterVirtualMachineRole names the group
   after the VM, and the reconciler's own do-not-create guard looks that group up
   BY VM NAME (see clusterGuard in powershell_vm.go). Leave the group on the old
   name and the guard stops matching, so another node stops recognising that the
   VM lives elsewhere and runs New-VM for it. A half-renamed clustered VM is
   therefore worse than one that was never renamed, which is why the VM name is
   put back if the group will not follow. */

// RenameVM renames a VM and, when it is clustered, its cluster group with it.
//
// cluster is the VM's cluster name, empty for a standalone VM.
func (p *PowerShell) RenameVM(ctx context.Context, vm, newName, cluster string) error {
	if strings.TrimSpace(newName) == "" {
		return fmt.Errorf("a new name is required to rename %q", vm)
	}
	if strings.EqualFold(strings.TrimSpace(vm), strings.TrimSpace(newName)) {
		// Not an error: asking for the name it already has is a no-op, and the
		// centre's re-key is idempotent on the same grounds.
		return nil
	}
	if err := p.run2(ctx, renameVMScript(vm, newName, cluster)); err != nil {
		return fmt.Errorf("rename VM %q to %q: %w", vm, newName, err)
	}
	return nil
}

/*
renameVMScript builds it, split out so every refusal is testable without a host.

	Idempotent by construction: a VM already carrying the new name, with nothing
	under the old one, is this job's own previous run and reports NOOP rather than
	failing. That matters because the centre re-keys desired state only after the
	job succeeds, so a job that ran and lost its result must be safe to repeat.
*/
func renameVMScript(vm, newName, cluster string) string {
	old, want := psQuote(vm), psQuote(newName)
	script := "$ErrorActionPreference='Stop'; " +
		"$old = " + old + "; $new = " + want + "; " +
		"$vm = Get-VM -Name $old -ErrorAction SilentlyContinue; " +
		"$already = Get-VM -Name $new -ErrorAction SilentlyContinue; " +
		// Its own previous run: the new name is here and the old one is gone.
		"if (-not $vm -and $already) { 'RESULT=NOOP already renamed'; return }; " +
		"if (-not $vm) { throw ('there is no VM named ' + $old + ' on this host') }; " +
		/* Both names present is a different machine wearing the target name, and
		   renaming onto it is refused by Hyper-V anyway — but the refusal is
		   clearer said here, where the two can be named. */
		"if ($already) { throw ('a different VM is already named ' + $new + ' on this host, so the rename would " +
		"collide with it') }; " +
		"Rename-VM -VM $vm -NewName $new; "

	if strings.TrimSpace(cluster) != "" {
		script += "Import-Module FailoverClusters -ErrorAction SilentlyContinue; " +
			"$grp = Get-ClusterGroup -Name $old -ErrorAction SilentlyContinue; " +
			"if ($grp) { " +
			"  try { $grp.Name = $new } " +
			"  catch { " +
			/* Put the machine's name back. A VM whose group still carries the old
			   name is not half-done, it is broken: the reconciler's guard looks the
			   group up by VM name, misses, and another node creates a second VM. */
			"    try { Rename-VM -VM $vm -NewName $old } catch {}; " +
			"    throw ('the VM was renamed and its cluster group could not be: ' + [string]$_.Exception.Message + " +
			"'. The VM name has been put back, so the VM and its cluster group still agree and nothing is half-renamed') " +
			"  } " +
			"} else { " +
			// No group under either name is a VM the cluster does not own. Said,
			// not failed: it is renamed, and there is nothing left to rename.
			"  if (-not (Get-ClusterGroup -Name $new -ErrorAction SilentlyContinue)) { " +
			"    Write-Warning ('renamed, and no cluster group named ' + $old + ' was found to rename with it') } " +
			"}; "
	}
	return script + "'RESULT=RENAMED ' + $old + ' -> ' + $new"
}
