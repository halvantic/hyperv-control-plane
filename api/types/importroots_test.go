package types

import (
	"reflect"
	"testing"
)

func vol(path string) StorageVolume { return StorageVolume{Path: path} }

func TestImportScanRoots(t *testing.T) {
	tests := []struct {
		name string
		in   []StorageVolume
		want []string
	}{
		{
			// A cluster member. The CSVs are what an operator adopted a LUN for,
			// and they mount under C: without being part of it.
			name: "cluster member keeps its CSVs and drops the system drive",
			in:   []StorageVolume{vol(`C:\`), vol(`C:\ClusterStorage\DS1`), vol(`C:\ClusterStorage\DS2`)},
			want: []string{`C:\ClusterStorage\DS1`, `C:\ClusterStorage\DS2`},
		},
		{
			// The standalone variant, which is the same code rather than a
			// separate one: "what VMs are on this storage" is the same question
			// on a data drive as on a CSV.
			name: "standalone host keeps its data drives",
			in:   []StorageVolume{vol(`C:\`), vol(`D:\`), vol(`E:\`)},
			want: []string{`D:\`, `E:\`},
		},
		{
			// Walking C:\ costs more than everything else combined — Windows, the
			// page file, the default VM path — and the VMs there are the ones
			// already registered, which is the opposite of what this looks for.
			name: "a host with nothing but its system drive has nowhere to look",
			in:   []StorageVolume{vol(`C:\`)},
			want: nil,
		},
		{
			// D:\ and D: are one volume. Two entries would mean walking it twice
			// and reporting every VM on it twice.
			name: "the same volume written two ways is one root",
			in:   []StorageVolume{vol(`D:\`), vol(`D:`), vol(`d:\`)},
			want: []string{`D:\`},
		},
		{
			// Two members of one cluster listing their volumes in different
			// orders is the same needless diffing that sortedUplinks exists for.
			name: "order is stable regardless of what the host reported",
			in:   []StorageVolume{vol(`E:\`), vol(`C:\ClusterStorage\DS1`), vol(`D:\`)},
			want: []string{`C:\ClusterStorage\DS1`, `D:\`, `E:\`},
		},
		{
			name: "empty paths are not roots",
			in:   []StorageVolume{vol(""), vol("   "), vol(`D:\`)},
			want: []string{`D:\`},
		},
		{
			name: "a host that has reported no volumes yet has no roots",
			in:   nil,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ImportScanRoots(HostResources{Volumes: tt.in})
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("roots\n want %q\n  got %q", tt.want, got)
			}
		})
	}
}

/*
The CSV test is the one that matters, because the naive rule — "skip anything

	on the system drive" — silently excludes exactly the storage this feature
	exists for. A cluster member would then scan nothing and report no importable
	VMs, which reads as "the volume is empty".
*/
func TestCSVsAreNotTheSystemDrive(t *testing.T) {
	for _, p := range []string{`C:\ClusterStorage\DS1`, `c:\clusterstorage\ds1`, `C:/ClusterStorage/DS1`} {
		if !isCSVPath(p) {
			t.Errorf("%q was not recognised as a CSV, so a cluster member would scan nothing", p)
		}
	}
	for _, p := range []string{`C:\`, `C:`, `D:\`} {
		if isCSVPath(p) {
			t.Errorf("%q was treated as a CSV", p)
		}
	}
}
