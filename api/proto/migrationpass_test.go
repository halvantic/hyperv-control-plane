package ballastpb

import (
	"testing"
	"time"

	"github.com/joshua-fourie/ballast/api/types"
)

/*
The change markers have to survive the wire.

	A field the proto does not carry round-trips perfectly as its zero value, so
	the failure is silent: the agent copies a delta, reports the marker, the
	centre stores an empty one, and the next pass reads the whole disk again
	while the console says "delta". This is the test that stops that, and it
	asserts on the marker specifically because that is the field with no
	plausible zero value.
*/
func TestAPassResultSurvivesTheWire(t *testing.T) {
	started := time.Date(2026, 8, 27, 2, 15, 0, 0, time.UTC)
	in := types.HostStatus{
		MigrationPasses: []types.MigrationPassResult{{
			// In-flight progress crosses the same wire as a finished pass. A
			// field the proto drops round-trips as its zero value, so this one
			// would silently turn every live report into a COMPLETED one at the
			// centre — advancing the pass count on a copy still running.
			InProgress:       true,
			Migration:        "mig-web01",
			JobID:            "job-4491",
			Final:            true,
			SourcePoweredOff: true,
			CopiedBytes:      42 << 20,
			StartedAt:        started,
			FinishedAt:       started.Add(9 * time.Minute),
			Disks: []types.MigrationPassDisk{{
				Key:          2000,
				Label:        "Hard disk 1",
				SourcePath:   "[ds1] Web01/Web01.vmdk",
				DestPath:     `C:\ClusterStorage\DS1\Web01\Web01.vhdx`,
				SizeBytes:    500 << 30,
				CopiedBytes:  42 << 20,
				NextChangeID: "52 3f b1 9c-4d 2a/17",
			}, {
				Key:          2001,
				Label:        "Hard disk 2",
				NextChangeID: "52 3f b1 9c-4d 2a/18",
			}},
		}, {
			Migration: "mig-db02",
			Error:     "changed block tracking was reset",
			CBTReset:  true,
		}},
	}

	out := StatusFromProto(StatusToProto(in))

	if len(out.MigrationPasses) != 2 {
		t.Fatalf("%d passes came back, sent 2", len(out.MigrationPasses))
	}
	got := out.MigrationPasses[0]
	if len(got.Disks) != 2 {
		t.Fatalf("%d disks came back, sent 2", len(got.Disks))
	}
	// The markers, first and by name. Everything else can be re-read from the
	// source; these cannot be recovered once lost.
	if got.Disks[0].NextChangeID != "52 3f b1 9c-4d 2a/17" || got.Disks[1].NextChangeID != "52 3f b1 9c-4d 2a/18" {
		t.Errorf("the change markers did not survive: %q, %q", got.Disks[0].NextChangeID, got.Disks[1].NextChangeID)
	}
	if got.Migration != "mig-web01" || got.JobID != "job-4491" {
		t.Errorf("the pass lost its identity: %+v", got)
	}
	if !got.InProgress {
		t.Error("InProgress did not survive the round trip, so a live report would arrive as a completed pass")
	}
	if !got.Final || !got.SourcePoweredOff {
		t.Error("the cutover pass came back looking like an ordinary delta")
	}
	if got.CopiedBytes != 42<<20 || got.Disks[0].SizeBytes != 500<<30 {
		t.Errorf("the byte counts did not survive: %+v", got)
	}
	if !got.StartedAt.Equal(started) || !got.FinishedAt.Equal(started.Add(9*time.Minute)) {
		t.Errorf("the times did not survive: %v .. %v", got.StartedAt, got.FinishedAt)
	}
	if got.Disks[0].DestPath != `C:\ClusterStorage\DS1\Web01\Web01.vhdx` {
		t.Errorf("the destination path did not survive: %q", got.Disks[0].DestPath)
	}

	// A failed pass carries its reason and its recoverability, or the centre
	// fails a migration that only needed a full read.
	if bad := out.MigrationPasses[1]; bad.Error == "" || !bad.CBTReset {
		t.Errorf("the failure did not survive: %+v", bad)
	}
}

// No passes must stay no passes, not an empty list that reads as "reported and
// there was nothing" when the agent is old enough not to send the field.
func TestNoPassesStaysAbsent(t *testing.T) {
	out := StatusFromProto(StatusToProto(types.HostStatus{}))
	if out.MigrationPasses != nil {
		t.Errorf("an empty status came back with %d passes", len(out.MigrationPasses))
	}
}
