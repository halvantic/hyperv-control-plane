package ballastpb

import (
	"testing"
	"reflect"
	"time"

	"github.com/joshua-fourie/ballast/api/types"
)

/* A field the proto does not carry round-trips perfectly as its ZERO VALUE, so
   the failure is silent: the centre stores the setting, the agent never
   receives it, and nothing anywhere reports a problem. The brief requires a
   round-trip test with any field added to a shared type, and this type is the
   worst case for a silent zero — RegisteredElsewhere and InUseElsewhere are
   what stop an import from giving two hosts a live claim on one set of VHDXs.

   Every field is set to a NON-ZERO value deliberately. A test that leaves a
   bool false cannot tell a carried field from a dropped one. */

func TestImportableVMRoundTrip(t *testing.T) {
	in := []types.ImportableVM{{
		Name:                "DC01",
		ID:                  "5C1E7B9A-1111-4E2B-9F3A-0A1B2C3D4E5F",
		ConfigPath:          `C:\ClusterStorage\DS1\DC01\Virtual Machines\5C1E7B9A.vmcx`,
		Volume:              `C:\ClusterStorage\DS1`,
		Generation:          2,
		ProcessorCount:      4,
		MemoryStartupBytes:  4294967296,
		SizeBytes:           137438953472,
		SavedState:          true,
		RegisteredElsewhere: true,
		RegisteredOn:        "HVNEW04",
		InUseElsewhere:      true,
		Compatible:          true,
		CompatKnown:         true,
		Error:               "the configuration could not be read",
		Incompatibilities: []types.VMIncompatibility{{
			Code:    33012,
			Message: `Could not find Ethernet switch 'ConvergedSwitch'.`,
			Kind:    "Switch",
			Remedy:  "connect the adapter to a switch this host has, or create the switch first",
			Fixable: true,
		}},
	}}

	got := importableVMsFromProto(importableVMsToProto(in))
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("did not survive the round trip:\n want %+v\n  got %+v", in, got)
	}
}

func TestImportScanRoundTrip(t *testing.T) {
	in := &types.ImportScanStatus{
		Roots:     []string{`C:\ClusterStorage\DS1`, `D:\`},
		ScannedAt: time.Date(2026, 8, 25, 14, 30, 0, 0, time.UTC),
		ElapsedMs: 4210,
		Truncated: true,
		Message:   "stopped at 200 configurations",
	}
	got := importScanFromProto(importScanToProto(in))
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("did not survive the round trip:\n want %+v\n  got %+v", in, got)
	}
}

/* Absent must stay absent. A scan that never ran and a scan that found nothing
   are the distinction ImportScanStatus exists to make, and a converter that
   turned nil into an empty struct would erase it. */
func TestImportScanAbsentStaysAbsent(t *testing.T) {
	if got := importScanFromProto(importScanToProto(nil)); got != nil {
		t.Fatalf("a scan that never ran came back as %+v, which reads as one that found nothing", got)
	}
	if got := importableVMsFromProto(importableVMsToProto(nil)); got != nil {
		t.Fatalf("nil importable VMs came back as %+v", got)
	}
}

/* The whole HostStatus path, not just the helper. The helper being correct and
   the field never being wired into HostStatusToProto is exactly the silent
   failure the brief describes. */
func TestHostStatusCarriesImportableVMs(t *testing.T) {
	in := types.HostStatus{
		ImportableVMs: []types.ImportableVM{{
			Name:                "DC01",
			ConfigPath:          `C:\ClusterStorage\DS1\DC01\Virtual Machines\x.vmcx`,
			RegisteredElsewhere: true,
			RegisteredOn:        "HVNEW04",
			InUseElsewhere:      true,
		}},
		ImportScan: &types.ImportScanStatus{Roots: []string{`C:\ClusterStorage\DS1`}, ElapsedMs: 12},
	}
	got := StatusFromProto(StatusToProto(in))
	if len(got.ImportableVMs) != 1 {
		t.Fatalf("HostStatus lost its importable VMs entirely: %+v", got.ImportableVMs)
	}
	v := got.ImportableVMs[0]
	if !v.RegisteredElsewhere || v.RegisteredOn != "HVNEW04" || !v.InUseElsewhere {
		t.Fatalf("the two guards that prevent corrupting a running VM did not survive: %+v", v)
	}
	if got.ImportScan == nil || got.ImportScan.ElapsedMs != 12 {
		t.Fatalf("scan record did not survive: %+v", got.ImportScan)
	}
}
