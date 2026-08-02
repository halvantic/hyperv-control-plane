package hyperv

import "strings"

import "testing"

// The template scripts are built as strings and only ever run against a real
// host, so their content is what these tests can pin. Match the exact construct,
// never wording that also appears in a comment.

func TestCaptureTemplateScriptGeneralise(t *testing.T) {
	plain := captureTemplateScript(false)
	gen := captureTemplateScript(true)

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
		if !strings.Contains(s, "-ne 'Off'") {
			t.Fatal("capture must refuse a VM that is not Off")
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
