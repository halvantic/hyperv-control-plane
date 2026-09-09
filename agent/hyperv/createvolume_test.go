package hyperv

import (
	"context"
	"strings"
	"testing"
)

/*
Carving a volume out of the space an OS volume does not use.

	The whole safety argument for letting this run on the OS disk is what the
	script does NOT contain, so that is what the test asserts hardest: no
	Clear-Disk, no Initialize-Disk, nothing that touches a partition that already
	exists. If one of those ever appears here, this job stops being a way to use
	spare capacity and becomes a way to erase Windows.
*/
func TestCreateVolumeInFreeSpaceScript(t *testing.T) {
	cases := []struct {
		name      string
		device    string
		letter    string
		label     string
		sizeBytes uint64
		want      []string
		absent    []string
	}{
		{
			name:   "a sized volume names both numbers when it will not fit",
			device: "1", letter: "D", label: "Hyper-V", sizeBytes: 214_748_364_800,
			want: []string{
				"$want = [uint64]214748364800",
				"New-Partition -DiskNumber $disk.Number -Size $want -DriveLetter 'D'",
				"Format-Volume -FileSystem NTFS -Confirm:$false -NewFileSystemLabel 'Hyper-V'",
				// Refused with the size that WOULD fit, not just "too big".
				"if ($want -gt $free)",
				"the largest",
				// Its own previous run, checked before anything is measured.
				"$_.DriveLetter -eq 'D'",
				"'RESULT=NOOP'",
			},
		},
		{
			name:   "no size takes the whole free extent",
			device: "1", letter: "D",
			want: []string{
				"New-Partition -DiskNumber $disk.Number -UseMaximumSize -DriveLetter 'D'",
				"Format-Volume -FileSystem NTFS",
			},
			absent: []string{
				"-Size $want",
				"-NewFileSystemLabel", // none asked for, none invented
			},
		},
		{
			name:   "a raw disk is sent to the other action rather than failed at",
			device: "3", letter: "E",
			want: []string{
				"if ($disk.PartitionStyle -eq 'RAW')",
				"Format the whole disk instead",
			},
		},
		{
			name:   "an MBR disk over 2TB is told why its free space is unreachable",
			device: "1", letter: "D",
			want: []string{
				"$disk.PartitionStyle -eq 'MBR' -and [uint64]$disk.Size -gt 2199023255552",
				"converted to GPT",
			},
		},
		{
			name:   "a pooled disk is named as pooled, not as unreadable",
			device: "4", letter: "D",
			want: []string{"Storage Spaces owns it"},
		},
	}

	// Present in every script, whatever the parameters: this job may run on the
	// OS disk, and these are the reason it is safe to.
	const (
		clear      = "Clear-Disk"
		initialise = "Initialize-Disk"
		remove     = "Remove-Partition"
		reset      = "Reset-PhysicalDisk"
	)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := createVolumeInFreeSpaceScript(c.device, c.letter, c.label, c.sizeBytes)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, a := range c.absent {
				if strings.Contains(got, a) {
					t.Errorf("unexpectedly contains %q in:\n%s", a, got)
				}
			}
			for _, destructive := range []string{clear, initialise, remove, reset} {
				if strings.Contains(got, destructive) {
					t.Fatalf("script contains %s. This job runs on the OS disk; it may only allocate space "+
						"nothing has claimed:\n%s", destructive, got)
				}
			}
			// The boot/system refusal that guards every other disk job must NOT
			// be here — it would refuse the exact host this exists for.
			if strings.Contains(got, "refusing to format the OS/boot disk") {
				t.Error("this job carries the whole-disk refusal, which would refuse the shrunk OS disk it was written for")
			}
		})
	}
}

/*
A volume with no drive letter is refused, and the reason is idempotency.

	Elsewhere a letter-less volume is a legitimate arrangement. Here it would
	leave the job nothing to recognise its own previous run by, so running it
	twice would carve a SECOND volume out of what remained rather than doing
	nothing — an operation that is not idempotent, on a host where the space it
	consumes is the space somebody was saving.
*/
func TestCreateVolumeInFreeSpaceNeedsALetter(t *testing.T) {
	p := &PowerShell{}
	err := p.CreateVolumeInFreeSpace(context.Background(), "1", "  ", "", 0)
	if err == nil {
		t.Fatal("accepted a volume with no drive letter, so a second run would carve a second volume")
	}
	if !strings.Contains(err.Error(), "drive letter is required") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}
