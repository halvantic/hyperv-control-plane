package types

import "testing"

/* The storage kind is a discriminator because the models do not overlap: S2D
   pools local disks and Ballast declares capacity and resiliency, while an iSCSI
   array owns all three and the cluster has no pool at all. Bolting iSCSI onto the
   S2D fields would make pool health, capacity and repair report on something that
   does not exist. */

// Clusters already exist that predate ClusterStorageSpec and set only EnableS2D.
// Every consumer reads StorageKind(), so an old cluster and a new one are
// indistinguishable to it — otherwise each caller would have to know which era
// its spec came from, and the ones that forgot would treat a live S2D cluster as
// having no storage at all.
func TestStorageKindFallsBackToTheDeprecatedFlag(t *testing.T) {
	if got := (ClusterSpec{EnableS2D: true}).StorageKind(); got != StorageKindS2D {
		t.Errorf("a pre-existing S2D cluster must still report S2D, got %q", got)
	}
	if got := (ClusterSpec{}).StorageKind(); got != "" {
		t.Errorf("no storage declared means no kind, got %q", got)
	}
}

func TestAnExplicitKindWins(t *testing.T) {
	s := ClusterSpec{
		// The flag is stale — this cluster was converted — and the explicit kind
		// is the operator's current intent.
		EnableS2D: true,
		Storage:   &ClusterStorageSpec{Kind: StorageKindISCSI},
	}
	if got := s.StorageKind(); got != StorageKindISCSI {
		t.Fatalf("the explicit kind must win over the deprecated flag, got %q", got)
	}
}

func TestAStorageSpecWithNoKindStillFallsBack(t *testing.T) {
	s := ClusterSpec{EnableS2D: true, Storage: &ClusterStorageSpec{}}
	if got := s.StorageKind(); got != StorageKindS2D {
		t.Fatalf("an empty kind is not a declaration; want the fallback, got %q", got)
	}
}

/* MPIO is not a preference.

   With several portals and no MPIO, Windows sees the SAME LUN once per path as
   separate disks. A cluster that then uses one path's disk on one node and
   another path's on a second is writing to the same blocks through two devices
   it believes are unrelated. That is data corruption, not a performance setting,
   so a request to disable it alongside multiple portals is refused rather than
   honoured — and the refusal is reported, not silent. */

func TestMPIOIsRequiredWheneverThereAreSeveralPaths(t *testing.T) {
	no := false
	s := ISCSIStorageSpec{
		Portals:    []string{"10.0.70.10", "10.0.71.10"},
		EnableMPIO: &no,
	}
	required, overridden := s.MPIORequired()
	if !required {
		t.Fatal("several portals without MPIO presents one LUN as several disks; it must be required")
	}
	if !overridden {
		t.Fatal("the operator asked for it off and was refused — that must be reported, not silent")
	}
}

func TestMPIODefaultsFromThePathCount(t *testing.T) {
	multi := ISCSIStorageSpec{Portals: []string{"a", "b"}}
	if required, overridden := multi.MPIORequired(); !required || overridden {
		t.Errorf("multiple portals default to MPIO on, unopinionated: required=%v overridden=%v", required, overridden)
	}
	single := ISCSIStorageSpec{Portals: []string{"a"}}
	if required, overridden := single.MPIORequired(); required || overridden {
		t.Errorf("a single path needs no MPIO by default: required=%v overridden=%v", required, overridden)
	}
}

// One path and an explicit request for MPIO is honoured. It is harmless, and it
// is what an operator about to add a second portal would sensibly set first.
func TestMPIOMayBeAskedForOnASinglePath(t *testing.T) {
	yes := true
	s := ISCSIStorageSpec{Portals: []string{"10.0.70.10"}, EnableMPIO: &yes}
	if required, overridden := s.MPIORequired(); !required || overridden {
		t.Fatalf("an explicit yes on one path is honoured: required=%v overridden=%v", required, overridden)
	}
}

// Explicitly off with one path is a real choice and stays off.
func TestMPIOMayBeDeclinedOnASinglePath(t *testing.T) {
	no := false
	s := ISCSIStorageSpec{Portals: []string{"10.0.70.10"}, EnableMPIO: &no}
	if required, overridden := s.MPIORequired(); required || overridden {
		t.Fatalf("one path may decline MPIO: required=%v overridden=%v", required, overridden)
	}
}
