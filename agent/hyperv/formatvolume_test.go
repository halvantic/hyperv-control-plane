package hyperv

import (
	"strings"
	"testing"
)

// The OS/boot volume must be refused, and the drive letter must be quoted
// rather than interpolated raw — the same two guarantees formatDiskScript and
// formatDiskDriveScript already carry, checked here for the third script.
func TestFormatVolumeScriptRefusesOSVolume(t *testing.T) {
	got := formatVolumeScriptForTest()
	for _, want := range []string{
		"Get-Partition -DriveLetter $letter",
		"if ($part.IsBoot -or $part.IsSystem) { throw 'refusing to format the OS/boot volume' }",
		"Format-Volume -DriveLetter $letter -FileSystem NTFS -Force -Confirm:$false",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script does not contain %q:\n%s", want, got)
		}
	}
	// Get-Disk would report the WHOLE disk's IsBoot/IsSystem, which is true for
	// every partition on a disk carrying the OS (D:, I: carved from the OS LUN
	// on some hosts) — checking it here would refuse a data partition that
	// Format-Volume never puts the boot partition anywhere near.
	if strings.Contains(got, "Get-Disk") {
		t.Errorf("script must not check the whole disk's IsBoot/IsSystem, only the partition's:\n%s", got)
	}
}

func TestFormatVolumeScriptQuotesItsInputs(t *testing.T) {
	got := formatVolumeScript("I'; Remove-Item C:\\ -Recurse; '")
	if strings.Contains(got, "Remove-Item C:\\ -Recurse;") && !strings.Contains(got, "''") {
		t.Fatalf("drive letter was not quoted:\n%s", got)
	}
}
