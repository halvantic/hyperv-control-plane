package vmware

import (
	"testing"

	"github.com/joshua-fourie/ballast/api/types"
)

// SnapshotName is a pure forward to the shared schema helper (see its own
// doc comment) so the centre and the agent can never spell the migration
// snapshot's name differently. This used to be asserted from
// centre/controllers, importing this package as a test-only fixture — moved
// here ahead of the repo split, since agent/vmware forwarding correctly to
// api/types is this side's half of the invariant, and the centre's half is
// covered by api/types' own tests of MigrationSnapshotName.
func TestSnapshotNameForwardsToTheSharedSchemaHelper(t *testing.T) {
	if got, want := SnapshotName("mig-bsl"), types.MigrationSnapshotName("mig-bsl"); got != want {
		t.Errorf("SnapshotName() = %q, want %q (types.MigrationSnapshotName)", got, want)
	}
}
