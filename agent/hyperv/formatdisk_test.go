package hyperv

import (
	"strings"
	"testing"
)

// Formatting with and without a drive letter.
//
// A volume with no letter is a deliberate arrangement — a disk destined for a
// folder mount point, or one about to be handed to a cluster — and assigning a
// letter nobody asked for is a change to the host that was not requested. The
// letter path must keep passing one; the no-letter path must pass none, because
// New-Partition assigns nothing unless it is told to.
func TestFormatDiskDriveScript(t *testing.T) {
	cases := []struct {
		name        string
		deviceID    string
		driveLetter string
		want        []string
		absent      []string
	}{
		{
			name:     "with a letter, the partition and the format both name it",
			deviceID: "2", driveLetter: "I",
			want: []string{
				"New-Partition -DiskNumber $disk.Number -UseMaximumSize -DriveLetter 'I'",
				"Format-Volume -DriveLetter 'I' -FileSystem NTFS",
				// Idempotent: a partition already holding the letter is the proof.
				"Where-Object { $_.DriveLetter -eq 'I' }",
				"'RESULT=NOOP'",
				"Initialize-Disk -Number $disk.Number -PartitionStyle GPT",
				"if ($disk.IsBoot -or $disk.IsSystem) { throw 'refusing to format the OS/boot disk' }",
			},
		},
		{
			name:     "with no letter, nothing assigns one",
			deviceID: "2", driveLetter: "",
			want: []string{
				"$part = New-Partition -DiskNumber $disk.Number -UseMaximumSize;",
				"$part | Format-Volume -FileSystem NTFS",
				// Idempotent on the evidence that exists here: a Basic partition
				// with a file system and no letter is this job's previous run.
				"$_.Type -eq 'Basic' -and -not $_.DriveLetter",
				"'RESULT=NOOP'",
				"Initialize-Disk -Number $disk.Number -PartitionStyle GPT",
				"if ($disk.IsBoot -or $disk.IsSystem) { throw 'refusing to format the OS/boot disk' }",
			},
			absent: []string{
				"-DriveLetter",       // never passed to New-Partition
				"-AssignDriveLetter", // nor asked for implicitly
				"Format-Volume -DriveLetter",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatDiskDriveScript(c.deviceID, c.driveLetter)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("script does not contain %q:\n%s", w, got)
				}
			}
			for _, a := range c.absent {
				if strings.Contains(got, a) {
					t.Errorf("script must not contain %q:\n%s", a, got)
				}
			}
			// The disk is always selected by the id it was given, and the OS disk
			// is always refused, whichever path ran.
			if !strings.Contains(got, "[string]$_.DeviceId -eq '"+c.deviceID+"'") {
				t.Errorf("script does not select disk %q:\n%s", c.deviceID, got)
			}
		})
	}
}

// A drive letter reaching the script is quoted, not interpolated raw.
func TestFormatDiskDriveScriptQuotesItsInputs(t *testing.T) {
	got := formatDiskDriveScript("2'; Remove-Item C:\\ -Recurse; '", "I")
	if strings.Contains(got, "Remove-Item C:\\ -Recurse;") && !strings.Contains(got, "''") {
		t.Fatalf("device id was not quoted:\n%s", got)
	}
}
