package ballastpb

import (
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

/* A field the proto does not carry round-trips perfectly as its zero value, so
   the failure is silent: the centre stores the witness LUN, the agent receives a
   witness with no disk, and the only symptom is a disk witness that never gets
   configured with nothing anywhere reporting a problem. Hence CLAUDE.md's rule
   that a new field lands in the schema, the proto and both converters together,
   with a round-trip test. */

func TestTheWitnessDiskSurvivesTheProto(t *testing.T) {
	lun := 3
	in := types.ClusterSpec{
		Members: []string{"HVNEW04", "HVNEW05"},
		Witness: types.WitnessSpec{
			Type: types.WitnessDisk,
			Disk: &types.CSVSourceSpec{
				SerialNumber: "6001405ab12cd34ef56",
				TargetIQN:    "iqn.2000-01.com.synology:Xpenology.default-target.aa584d33bc2",
				LUN:          &lun,
			},
		},
	}

	out := clusterSpecFromProto(clusterSpecToProto(in))

	if out.Witness.Disk == nil {
		t.Fatal("the witness disk did not survive the proto; a disk witness would silently never be configured")
	}
	if out.Witness.Disk.SerialNumber != in.Witness.Disk.SerialNumber {
		t.Errorf("serial: got %q want %q — the serial is the only identifier that names the same disk on every member",
			out.Witness.Disk.SerialNumber, in.Witness.Disk.SerialNumber)
	}
	if out.Witness.Disk.TargetIQN != in.Witness.Disk.TargetIQN {
		t.Errorf("target IQN: got %q want %q", out.Witness.Disk.TargetIQN, in.Witness.Disk.TargetIQN)
	}
	if out.Witness.Disk.LUN == nil || *out.Witness.Disk.LUN != lun {
		t.Errorf("LUN: got %v want %d", out.Witness.Disk.LUN, lun)
	}
	if out.Witness.Type != types.WitnessDisk {
		t.Errorf("witness type: got %q want %q", out.Witness.Type, types.WitnessDisk)
	}
}

// A file share witness must not grow a disk out of nowhere: nil has to stay nil,
// or the disk-witness path would fire for a cluster that never asked for one.
func TestAWitnessWithNoDiskStaysThatWay(t *testing.T) {
	in := types.ClusterSpec{
		Witness: types.WitnessSpec{Type: types.WitnessFileShare, FileSharePath: `\\xpenology.ballast.local\dr`},
	}

	out := clusterSpecFromProto(clusterSpecToProto(in))

	if out.Witness.Disk != nil {
		t.Fatalf("a file share witness must carry no disk, got %+v", out.Witness.Disk)
	}
	if out.Witness.FileSharePath != in.Witness.FileSharePath {
		t.Errorf("share path: got %q want %q", out.Witness.FileSharePath, in.Witness.FileSharePath)
	}
}
