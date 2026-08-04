package hyperv

import "strings"

import "testing"

// The template scripts are built as strings and only ever run against a real
// host, so their content is what these tests can pin. Match the exact construct,
// never wording that also appears in a comment.

func TestCaptureTemplateScriptGeneralise(t *testing.T) {
	plain := captureTemplateScript(false, false)
	gen := captureTemplateScript(true, false)

	// Sysprep must appear only when generalising was actually asked for. A
	// capture that generalises when it was not asked to destroys the source VM's
	// identity, which is not recoverable.
	if strings.Contains(plain, "sysprep.exe") {
		t.Fatal("a non-generalising capture must not run sysprep")
	}
	if !strings.Contains(gen, "'/generalize','/oobe','/shutdown','/quiet'") {
		t.Fatal("generalising capture must invoke sysprep with /generalize /oobe /shutdown")
	}
	// Sysprep shuts the guest down, killing the PowerShell Direct session, so it
	// must be started detached and waited on from the host.
	if strings.Contains(gen, "Start-Process") && strings.Contains(gen, "-Wait") {
		t.Fatal("sysprep must be started detached: it shuts the guest down and would break the session it was invoked over")
	}
	if !strings.Contains(gen, "Start-Sleep -Seconds 10") || !strings.Contains(gen, "AddMinutes(60)") {
		t.Fatal("generalising capture must wait, bounded, for the guest to shut itself down")
	}

	for _, s := range []string{plain, gen} {
		// The VM must be Off before the copy: a running VM holds its VHDX open.
		if !strings.Contains(s, "$state -ne 'Off'") {
			t.Fatal("capture must refuse a VM that is not Off")
		}
		// Saved is neither Running nor Off, and it is where a clustered VM lands
		// when its role goes offline (AutomaticStopAction defaults to Save). Saying
		// "a running VM holds its VHDX open" about it is untrue and sends the
		// operator looking for something to stop that is already stopped.
		if !strings.Contains(s, "$state -eq 'Saved'") {
			t.Fatal("Saved must be diagnosed on its own terms, not as a running VM")
		}
		if !strings.Contains(s, "Remove-VMSavedState") {
			t.Fatal("the Saved refusal must name the way out of it")
		}
		// Copy via a temp name so an interrupted capture never leaves something
		// at the template's path that looks like a finished image.
		if !strings.Contains(s, `$tmp = $dest + '.copying'`) {
			t.Fatal("capture must copy through a temp name")
		}
		if !strings.Contains(s, "Move-Item -LiteralPath $tmp -Destination $dest") {
			t.Fatal("capture must move the temp file into place")
		}
		if !strings.Contains(s, "'RESULT=OK'") {
			t.Fatal("capture must end with a result marker")
		}
		if !strings.Contains(s, "'BYTES=' + [string]((Get-Item -LiteralPath $dest).Length)") {
			t.Fatal("capture must report the captured image's size")
		}
		// Pipeline stops after a storage/cluster cmdlet abort the whole script
		// with exit 0 and no marker — the silent-truncation class this repository
		// has already been bitten by.
		if strings.Contains(s, "Select-Object -First 1") {
			t.Fatal("Select-Object -First 1 must not be used: its pipeline stop aborts the script silently")
		}
	}
}

func TestDeployFromTemplateScriptUnattend(t *testing.T) {
	plain := deployFromTemplateScript(false)
	withUA := deployFromTemplateScript(true)

	if strings.Contains(plain, "Mount-VHD") {
		t.Fatal("a deploy with no guest profile must not mount the image")
	}
	if !strings.Contains(withUA, "Mount-VHD -Path $tmp") {
		t.Fatal("unattend injection must mount the TEMP copy, not the destination")
	}
	if strings.Contains(withUA, "Mount-VHD -Path $dest") {
		t.Fatal("mounting the destination would leave a half-configured image where the reconciler attaches it")
	}
	if !strings.Contains(withUA, "Dismount-VHD") || !strings.Contains(withUA, "} finally {") {
		t.Fatal("the mount must be dismounted in a finally block, or a failure strands it mounted")
	}
	if !strings.Contains(withUA, `$panther = $target + ':\Windows\Panther'`) || !strings.Contains(withUA, `Join-Path $panther 'Unattend.xml'`) {
		t.Fatal(`the unattend must be written to <windows volume>:\Windows\Panther\Unattend.xml`)
	}
	// A BOM ahead of the XML declaration makes Windows Setup reject the file.
	if !strings.Contains(withUA, "UTF8Encoding($false)") {
		t.Fatal("the unattend must be written as UTF-8 without a BOM")
	}
	// The Windows volume has to be found, not assumed to be the first partition.
	if !strings.Contains(withUA, `':\Windows\System32'`) {
		t.Fatal("the Windows volume must be identified by probing for a Windows installation")
	}

	for _, s := range []string{plain, withUA} {
		if !strings.Contains(s, `$tmp = $dest + '.deploying'`) {
			t.Fatal("deploy must copy through a temp name")
		}
		if !strings.Contains(s, "Move-Item -LiteralPath $tmp -Destination $dest") {
			t.Fatal("deploy must move the temp file into place")
		}
		// The deploy must not create the VM — the centre authors desired state and
		// the reconciler builds it, which is what keeps a deployed VM ordinary.
		if strings.Contains(s, "New-VM") {
			t.Fatal("deploy must not create the VM; the reconciler does that from desired state")
		}
		if !strings.Contains(s, "'RESULT=OK'") {
			t.Fatal("deploy must end with a result marker")
		}
		if strings.Contains(s, "Select-Object -First 1") {
			t.Fatal("Select-Object -First 1 must not be used: its pipeline stop aborts the script silently")
		}
	}
}

// A clustered VM that is Off is an OFFLINE ROLE, and Failover Clustering
// deregisters it from Hyper-V — so the state a disk copy requires is the state
// in which Get-VM cannot find it. Both scripts must register it for the duration
// and put it back, or clone and capture can never work on a clustered VM.
func TestDiskCopyScriptsHandleAnOfflineClusterRole(t *testing.T) {
	scripts := map[string]string{
		"capture":            captureTemplateScript(false, false),
		"capture generalise": captureTemplateScript(true, false),
		"clone":              cloneVMScript("Windows", "Windows-clone", `C:\ClusterStorage\DS1\Windows-clone`),
	}
	for name, s := range scripts {
		t.Run(name, func(t *testing.T) {
			// It must not hard-fail on Get-VM before trying the cluster.
			if strings.Contains(s, "Get-VM -Name $vm -ErrorAction Stop") || strings.Contains(s, "Get-VM -Name $src -ErrorAction Stop") {
				t.Fatal("Get-VM must not be -ErrorAction Stop: an offline cluster role is not a missing VM")
			}
			if !strings.Contains(s, "Ensure-BallastVMRegistered") {
				t.Fatal("the script must go through Ensure-BallastVMRegistered")
			}
			// Only the CONFIGURATION resource may be started. Starting the VM
			// resource would boot the guest, which is the opposite of what a copy
			// of its disk needs.
			if !strings.Contains(s, "'Virtual Machine Configuration'") {
				t.Fatal("only the Virtual Machine Configuration resource may be brought online")
			}
			if strings.Contains(s, "Start-ClusterGroup") {
				t.Fatal("starting the whole group would power the VM on")
			}
			// The repository's rule: cluster cmdlets take objects, not names.
			if strings.Contains(s, "Start-ClusterResource -Name") || strings.Contains(s, "Stop-ClusterResource -Name") {
				t.Fatal("cluster resource cmdlets must bind -InputObject, not -Name")
			}
			// The operator's role state must survive the operation.
			if !strings.Contains(s, "Stop-ClusterResource -InputObject $global:__broughtOnline") {
				t.Fatal("a role brought online for the copy must be put back")
			}
			if !strings.Contains(s, "} finally {") {
				t.Fatal("the restore must be in a finally block, or a failed copy leaves the role changed")
			}
		})
	}
}

// Sysprep shuts a clustered guest down, which takes its role offline and
// deregisters it. Waiting for State -eq 'Off' would then never be satisfied, so
// the VM vanishing has to count as the shutdown completing.
func TestGeneraliseWaitTreatsDeregistrationAsShutdown(t *testing.T) {
	s := captureTemplateScript(true, false)
	if !strings.Contains(s, "$g = Get-VM -Name $vm -ErrorAction SilentlyContinue") || !strings.Contains(s, "if (-not $g) { break }") {
		t.Fatal("the sysprep wait must break when the VM deregisters, not spin until the timeout")
	}
	// And it has to come back before its disks can be read.
	if !strings.Contains(s, "$v = Ensure-BallastVMRegistered $vm") {
		t.Fatal("the VM must be re-registered after sysprep so its disks can be read")
	}
}

func TestParseMarkerUint(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want uint64
	}{
		{"plain", "BYTES=1234\r\nRESULT=OK\r\n", 1234},
		{"no marker", "RESULT=OK\r\n", 0},
		{"not a number", "BYTES=lots\r\nRESULT=OK\r\n", 0},
		{"negative", "BYTES=-1\r\nRESULT=OK\r\n", 0},
		{"last wins", "BYTES=1\r\nBYTES=2\r\n", 2},
		{"trailing whitespace", "BYTES= 99 \r\n", 99},
		{"end of output", "BYTES=7", 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseMarkerUint(tt.out, "BYTES="); got != tt.want {
				t.Fatalf("parseMarkerUint(%q) = %d, want %d", tt.out, got, tt.want)
			}
		})
	}
}

// A saved VM cannot be captured, and a clustered VM is Saved rather than Off
// whenever its role goes offline. Discarding the saved state has to be something
// the centre can do, or the only way out is PowerShell on a node — which is the
// one thing this product exists to remove.
func TestCaptureCanDiscardSavedStateItself(t *testing.T) {
	plain := captureTemplateScript(false, false)
	discard := captureTemplateScript(false, true)

	// Match the INVOCATION, not the name: the Saved refusal names the cmdlet in
	// its message text, so a bare substring is present in both scripts. This file
	// has been bitten by matching prose before.
	const call = "Remove-VMSavedState -VMName $vm -ErrorAction Stop"
	if strings.Contains(plain, call) {
		t.Fatal("a capture must not discard saved memory unless it was asked to")
	}
	if !strings.Contains(discard, call) {
		t.Fatal("the capture must be able to discard the saved state itself")
	}
	// Only Saved. An Off VM needs nothing, and a Running one is a different
	// refusal — discarding is not a way to stop a VM.
	if !strings.Contains(discard, "if ([string]$v.State -eq 'Saved') {") {
		t.Fatal("only a Saved VM may have its state discarded")
	}
	// It has to happen before the Off check, or the capture refuses the very
	// state it was told to clear.
	if strings.Index(discard, "Remove-VMSavedState") > strings.Index(discard, "$state -eq 'Saved'") {
		t.Fatal("the discard must precede the state checks")
	}
}

// The standalone job is the same operation for a VM that is merely stuck Saved.
func TestDiscardSavedStateScript(t *testing.T) {
	s := discardSavedStateScript()
	if !strings.Contains(s, "Ensure-BallastVMRegistered") {
		t.Fatal("a saved clustered VM is deregistered while its role is offline; it must be registered first")
	}
	if !strings.Contains(s, "'RESULT=NOOP'") {
		t.Fatal("an already-Off VM must be a no-op, so this is safe to run ahead of anything needing Off")
	}
	if !strings.Contains(s, "$state -ne 'Saved'") {
		t.Fatal("a VM that is neither Off nor Saved must be refused, not silently left alone")
	}
	if !strings.Contains(s, "$after -ne 'Off'") {
		t.Fatal("the result must be verified, not assumed")
	}
}

// The drain must not pass -Wait. On Suspend-ClusterNode it is a SWITCH, not a
// timeout, so "-Wait 0" binds 0 positionally to -Name — a StringCollection — the
// same trap as the cluster cmdlets fixed in d113ecc. Verified against the real
// cmdlet's syntax on the rig before it ever ran.
func TestMaintenanceScriptDoesNotMisuseWait(t *testing.T) {
	s := maintenanceScript("HVNEW03", MaintenanceEnter)
	if strings.Contains(s, "-Wait") {
		t.Fatal("Suspend-ClusterNode -Wait is a switch, not a timeout; passing it (or a value) blocks the cycle or misbinds -Name")
	}
	if !strings.Contains(s, "Suspend-ClusterNode -Name") || !strings.Contains(s, "-Drain") {
		t.Fatal("entering maintenance must drain, not merely pause")
	}
	// Observe must never act: a node paused outside Ballast stays paused.
	if !strings.Contains(s, "$intent -eq 'enter'") || !strings.Contains(s, "$intent -eq 'exit'") {
		t.Fatal("acting must be gated on an explicit intent, so an observe pass changes nothing")
	}
	// DrainStatus is what separates "paused" from "safe to reboot".
	if !strings.Contains(s, "DrainStatus") || !strings.Contains(s, "InProgress") {
		t.Fatal("the drain's progress must be observed; paused alone does not mean the roles have gone")
	}
}

// Maintenance mode must NEVER put a node's storage into maintenance mode.
//
// An earlier version did, and it wedged the rig. Enable-StorageMaintenanceMode
// on a scale unit is not atomic: it releases the disks one at a time, the
// spaces go Degraded part-way through, its own health validation then fails and
// it aborts without rolling back — stranding the disks it already took. Those
// hold every space degraded, and a degraded space makes Suspend-ClusterNode
// refuse outright, so the drain can never finish. Observed on HVNEW03: two of
// four disks stranded, all three virtual disks degraded, every later drain
// refused with "a clustered space is in a degraded condition".
//
// This test is the guard against reintroducing it, in either order — there is
// no ordering that makes it safe, which is what the two-way deadlock proved.
func TestMaintenanceNeverEnablesStorageMaintenance(t *testing.T) {
	for _, intent := range []MaintenanceIntent{MaintenanceObserve, MaintenanceEnter, MaintenanceExit} {
		// Against the executable script only. The comments explain at length what
		// must not be done and name the cmdlet and its flag to do it — matching
		// prose rather than code is a trap this repo has fallen into before.
		s := psCode(maintenanceScript("HVNEW03", intent))
		if strings.Contains(s, "Enable-StorageMaintenanceMode") {
			t.Fatalf("intent %q: enabling storage maintenance strands disks part-way and deadlocks the drain; the node stays up during maintenance and its disks keep serving", intentWord(intent))
		}
		if strings.Contains(s, "ValidateVirtualDisksHealthy") {
			t.Fatalf("intent %q: forcing past the health validation makes the abort less likely, not safe — it exists because releasing storage that holds the only copy loses it", intentWord(intent))
		}
	}
}

// psCode strips whole-line PowerShell comments so an assertion is made against
// what runs, not what the script says about itself.
func psCode(script string) string {
	var kept []string
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// psBranch returns the body of one `$intent -eq '<word>'` arm. Every branch is
// present in every script — only $intent differs — so an ordering assertion has
// to be made inside the arm it is about, or it silently reads the other one.
func psBranch(t *testing.T, script, word string) string {
	t.Helper()
	head := "($intent -eq '" + word + "') {"
	i := strings.Index(script, head)
	if i < 0 {
		t.Fatalf("no %q branch in the script", word)
	}
	rest := script[i+len(head):]
	// Arms are separated by `} elseif (` / closed by a line-initial `}`.
	if j := strings.Index(rest, "\n} else"); j >= 0 {
		return rest[:j]
	}
	if j := strings.Index(rest, "\n}"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// Clearing it is the other half: an agent upgrading from the version that
// stranded disks has to heal them, or the node stays permanently unable to pause.
func TestMaintenanceClearsStrandedStorageMaintenance(t *testing.T) {
	s := psCode(maintenanceScript("HVNEW03", MaintenanceEnter))

	// Disabling the scale unit is a no-op when the unit was never in maintenance
	// and only individual disks were stranded — which is exactly what a part-way
	// Enable leaves. Those have to be cleared by disk.
	if !strings.Contains(s, "Disable-StorageMaintenanceMode -InputObject $d") {
		t.Fatal("stranded disks must be cleared individually; the scale-unit disable is a no-op when the unit itself was never in maintenance")
	}
	if !strings.Contains(s, "'In Maintenance Mode'") {
		t.Fatal("stranded disks are found by their own operational status, not the scale unit's")
	}

	// Both arms clear it, so a node stranded by an older agent heals whichever
	// way it is being driven.
	enter, exit := psBranch(t, s, "enter"), psBranch(t, s, "exit")
	for word, arm := range map[string]string{"enter": enter, "exit": exit} {
		if !strings.Contains(arm, "Clear-BallastStorageMaintenance") {
			t.Fatalf("the %s arm must clear stranded storage, or the node can never be drained again", word)
		}
	}
	// Observe must not undo a flag somebody else set deliberately, so the clear
	// lives only inside the two acting arms.
	if strings.Contains(strings.Replace(strings.Replace(s, enter, "", 1), exit, "", 1), "Clear-BallastStorageMaintenance %[1]s") {
		t.Fatal("clearing must be gated on an explicit intent; an observe pass changes nothing")
	}
	// Coming back, the node returns to service before its storage is acted on.
	if strings.Index(exit, "Resume-ClusterNode") > strings.Index(exit, "Clear-BallastStorageMaintenance") {
		t.Fatal("the node must be resumed before its storage state is acted on")
	}
	// Going out, the clear happens first: a stranded disk holds the spaces
	// degraded, and a degraded space is precisely what makes the pause refuse.
	if strings.Index(enter, "Clear-BallastStorageMaintenance") > strings.Index(enter, "Suspend-ClusterNode") {
		t.Fatal("stranded storage must be cleared before the pause is attempted, because it is what makes the pause refuse")
	}

	// And a failure to clear has to reach the centre. Left unsaid, the operator
	// sees only a node that will not pause, with nothing to explain why.
	if !strings.Contains(s, "storageOut=$storageOut") || !strings.Contains(s, "storageError=$storageErr") {
		t.Fatal("stranded storage, and a failure to clear it, must be reported")
	}
	if !strings.Contains(s, "$intent -ne 'observe' -and $storageOut") {
		t.Fatal("the report must distinguish 'could not clear it' from 'did not try'")
	}
}
