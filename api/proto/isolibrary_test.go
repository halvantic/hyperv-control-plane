package ballastpb

import (
	"reflect"
	"testing"
	"time"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

/* An ISO library is declared in two places and never in both for the same host:
   a standalone host declares its own, a cluster declares one for its members.
   Nothing is fanned between them, so each has to cross the wire on its own leg.

   HostSpec has no reflect-based every-field guard (TestHostRoundTripCarriesEveryField
   checks specific fields), so a spec field added without a converter would have
   round-tripped silently as nil — the exact failure that has now cost this
   codebase HostSpec.Maintenance, ClusterStatus.ReplicaBroker and
   VMStatus.MemoryDemandBytes. These tests are that guard for the library. */

func TestISOLibrarySurvivesTheWireOnAStandaloneHost(t *testing.T) {
	in := types.Host{
		Meta: types.ObjectMeta{Name: "hv04"},
		Spec: types.HostSpec{
			FQDN: "hv04.lab.local",
			ISOLibrary: &types.ISOLibrarySpec{
				Path:             `\\nas.lab.local\isos`,
				CredentialSecret: "nas-reader",
			},
		},
	}
	got := HostFromProto(HostToProto(in))
	if got.Spec.ISOLibrary == nil {
		t.Fatal("a standalone host's ISO library did not survive the round trip — the agent would never mount it")
	}
	if !reflect.DeepEqual(*in.Spec.ISOLibrary, *got.Spec.ISOLibrary) {
		t.Fatalf("mangled: in %+v got %+v", *in.Spec.ISOLibrary, *got.Spec.ISOLibrary)
	}
}

func TestISOLibrarySurvivesTheWireOnACluster(t *testing.T) {
	in := types.ClusterSpec{
		Members:    []string{"n1", "n2"},
		ISOLibrary: &types.ISOLibrarySpec{Path: `\\nas.lab.local\isos`},
	}
	got := clusterSpecFromProto(clusterSpecToProto(in))
	if got.ISOLibrary == nil || got.ISOLibrary.Path != `\\nas.lab.local\isos` {
		t.Fatalf("a cluster's ISO library did not survive the round trip: %+v", got.ISOLibrary)
	}
}

// No library declared is a real answer. It must arrive as nil, not as an empty
// library that the agent would then try to mount from "".
func TestNoISOLibraryStaysNil(t *testing.T) {
	got := HostFromProto(HostToProto(types.Host{Meta: types.ObjectMeta{Name: "hv04"}}))
	if got.Spec.ISOLibrary != nil {
		t.Fatalf("an undeclared library must stay nil, got %+v", got.Spec.ISOLibrary)
	}
	cl := clusterSpecFromProto(clusterSpecToProto(types.ClusterSpec{Members: []string{"n1"}}))
	if cl.ISOLibrary != nil {
		t.Fatalf("an undeclared cluster library must stay nil, got %+v", cl.ISOLibrary)
	}
}

// MachineReadable has THREE states and the third one is the interesting one.
// The agent reads the share as its service account; Hyper-V attaches media as
// the computer account. Unset means the computer-account check could not be
// established — not that it failed — and flattening it to false would report a
// perfectly good library as unbootable on every pass where the probe was skipped.
func TestISOLibraryStatusKeepsUnknownDistinctFromFalse(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name string
		in   *bool
	}{
		{"unknown", nil},
		{"reachable as the computer account", &yes},
		{"not reachable as the computer account", &no},
	} {
		st := types.HostStatus{ISOLibrary: &types.ISOLibraryStatus{
			Path: `\\nas.lab.local\isos`, Readable: true, MachineReadable: tc.in,
			Message: "m", ISOs: []string{"w2025.iso"},
			CheckedAt: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC),
		}}
		got := StatusFromProto(StatusToProto(st))
		if got.ISOLibrary == nil {
			t.Fatalf("%s: the library status did not survive", tc.name)
		}
		if tc.in == nil {
			if got.ISOLibrary.MachineReadable != nil {
				t.Errorf("%s: unknown must stay unknown, got %v", tc.name, *got.ISOLibrary.MachineReadable)
			}
			continue
		}
		if got.ISOLibrary.MachineReadable == nil {
			t.Errorf("%s: a determined result must not arrive as unknown", tc.name)
			continue
		}
		if *got.ISOLibrary.MachineReadable != *tc.in {
			t.Errorf("%s: want %v, got %v", tc.name, *tc.in, *got.ISOLibrary.MachineReadable)
		}
	}
}

// The rest of the status, so a field added later cannot ride along unnoticed.
func TestISOLibraryStatusCarriesEveryField(t *testing.T) {
	yes := true
	in := types.ISOLibraryStatus{
		Path:            `\\nas.lab.local\isos`,
		Readable:        true,
		MachineReadable: &yes,
		Message:         "granted to the node computer accounts",
		ISOs:            []string{"w2025.iso", "rocky10.iso"},
		CheckedAt:       time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC),
	}
	rv := reflect.ValueOf(in)
	for i := 0; i < rv.NumField(); i++ {
		if f := rv.Type().Field(i); f.IsExported() && rv.Field(i).IsZero() {
			t.Errorf("this test does not set ISOLibraryStatus.%s, so the round trip cannot vouch for it", f.Name)
		}
	}
	got := StatusFromProto(StatusToProto(types.HostStatus{ISOLibrary: &in}))
	if !reflect.DeepEqual(in, *got.ISOLibrary) {
		t.Fatalf("round trip mismatch:\n in:  %+v\n got: %+v", in, *got.ISOLibrary)
	}
}
