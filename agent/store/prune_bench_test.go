package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/joshua-fourie/ballast/api/types"
)

// seedJournal writes n delivered entries the way the unbounded version did,
// straight into the bucket so the fixture does not pay a transaction each.
func seedJournal(tb testing.TB, path string, n int) {
	tb.Helper()
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		tb.Fatal(err)
	}
	defer db.Close()
	// A realistic entry: status carries inventory and conditions, which is why
	// they ran to kilobytes each.
	st := types.HostStatus{
		Phase: types.PhaseReady, AgentVersion: "0.4.9-slice",
		Inventory: types.HostInventory{
			LogicalCPUs: 48,
			PhysicalAdapters: []types.PhysicalAdapter{
				{Name: "Ethernet 1", MAC: "00-50-56-96-00-01", LinkSpeedBps: 10_000_000_000, Up: true},
				{Name: "Ethernet 2", MAC: "00-50-56-96-00-02", LinkSpeedBps: 10_000_000_000, Up: true},
			},
		},
	}
	for i := 0; i < 12; i++ {
		st.Conditions = append(st.Conditions, types.Condition{
			Type: fmt.Sprintf("ManagementVNIC/Net%d", i), Status: true,
			Reason: "AlreadyConfigured", Message: "already matches desired state",
			LastTransitionTime: time.Now().UTC(),
		})
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketJournal)
		if err != nil {
			return err
		}
		for i := 1; i <= n; i++ {
			data, err := json.Marshal(JournalEntry{Seq: uint64(i), Time: time.Now().UTC(), Status: st, Delivered: true})
			if err != nil {
				return err
			}
			if err := b.Put(itob(uint64(i)), data); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		tb.Fatal(err)
	}
}

// The cost of finding what to send should not grow with how long the agent has
// been running. On the rig it did: sequence ~136,000, and every delivery attempt
// unmarshalled all of them looking for the few that were undelivered.
func TestUndeliveredCostDoesNotGrowWithAgentAge(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a six-figure journal")
	}
	const aged = 136_000 // what the rig had reached

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.db")
	seedJournal(t, path, aged)

	// Before: measure the scan the old code did on every single report.
	raw, err := openAt(path)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := raw.Undelivered(); err != nil {
		t.Fatal(err)
	}
	unpruned := time.Since(start)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// After: Open prunes on the way in, which is what an upgrading agent gets.
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start = time.Now()
	if _, err := s.Undelivered(); err != nil {
		t.Fatal(err)
	}
	pruned := time.Since(start)

	t.Logf("Undelivered() over %d entries: %v unpruned, %v after Open pruned", aged, unpruned, pruned)
	if pruned*10 > unpruned {
		t.Fatalf("pruning must make the scan cheap: %v unpruned vs %v pruned", unpruned, pruned)
	}
}
