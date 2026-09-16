package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/halvantic/hyperv-control-plane/api/types"
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

// A Windows service has 30 seconds to report Running before the SCM kills it,
// and Open runs inside that budget.
//
// Pruning and compacting on the way in blew it. HVNEW04 arrived with 186,000
// entries in a 1 GiB store, spent 20 seconds and 700 MB unmarshalling them, and
// was killed and restarted every 30 seconds — the host had no agent at all until
// someone noticed. The maintenance was worth doing; doing it during startup was
// not.
//
// One second is far below the real budget on purpose: the margin is what stops
// a slower disk or a bigger journal turning a passing test into a crash-looping
// host.
func TestOpenIsFastOnAHugeJournal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a six-figure journal")
	}
	path := filepath.Join(t.TempDir(), "agent.db")
	seedJournal(t, path, 186_000) // what HVNEW04 actually had

	start := time.Now()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	took := time.Since(start)
	defer s.Close()

	t.Logf("Open() over 186,000 entries took %v", took)
	if took > time.Second {
		t.Fatalf("Open must not do the journal's work: took %v, and a Windows service has 30s total", took)
	}
}

// The prune itself must not hold one enormous transaction either — that is where
// the 700 MB went. Chunking is what makes it survivable; this checks it still
// completes and leaves the journal bounded.
func TestPruningAHugeJournalCompletesInChunks(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a six-figure journal")
	}
	path := filepath.Join(t.TempDir(), "agent.db")
	seedJournal(t, path, 186_000)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PruneJournal(); err != nil {
		t.Fatalf("prune: %v", err)
	}

	all, err := s.Recent(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) > journalKeep {
		t.Fatalf("a completed prune must leave the tail: %d entries, want <= %d", len(all), journalKeep)
	}
	if !s.journalPruned() {
		t.Fatal("a completed prune must mark the store, or the next open never compacts")
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

	// After: the agent opens, comes up, and prunes in the background.
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PruneJournal(); err != nil {
		t.Fatal(err)
	}
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
