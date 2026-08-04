package store

import (
	"os"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/joshua-fourie/ballast/api/types"
)

/* The journal was append-only in the literal sense — MarkDelivered set a flag
   and nothing ever deleted anything. One full HostStatus per cycle, kept for
   ever: sequence ~136,000 and a 512 MB–1 GB agent.db per rig host.

   The size was the lesser problem. Undelivered() walks the whole bucket and
   unmarshals every entry, so each delivery attempt got slower as the agent aged,
   and status reports had begun failing with DeadlineExceeded on healthy hosts. */

func countJournal(t *testing.T, s *Store) (delivered, undelivered int) {
	t.Helper()
	all, err := s.Recent(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range all {
		if e.Delivered {
			delivered++
		} else {
			undelivered++
		}
	}
	return delivered, undelivered
}

func TestDeliveredEntriesArePrunedToATail(t *testing.T) {
	s := openTemp(t)

	// Far more than the tail, all delivered as they go — the steady state of a
	// healthy agent talking to a reachable centre.
	for i := 0; i < journalKeep*3; i++ {
		e, err := s.AppendStatus(types.HostStatus{ObservedGeneration: int64(i)}, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.MarkDelivered(e.Seq); err != nil {
			t.Fatal(err)
		}
	}

	// Delivery no longer prunes — that walk belongs off the status-delivery
	// path, which a six-figure journal had already made slow enough to time out.
	// Maintenance runs on its own schedule.
	if err := s.PruneJournal(); err != nil {
		t.Fatal(err)
	}

	delivered, undelivered := countJournal(t, s)
	if undelivered != 0 {
		t.Fatalf("everything was delivered; got %d queued", undelivered)
	}
	if delivered > journalKeep {
		t.Fatalf("delivered entries must be pruned to the tail: kept %d, want <= %d", delivered, journalKeep)
	}
	// And the tail must be the NEWEST entries — old status is what is worthless.
	all, err := s.Recent(0)
	if err != nil {
		t.Fatal(err)
	}
	last := all[len(all)-1]
	if last.Status.ObservedGeneration != int64(journalKeep*3-1) {
		t.Fatalf("the newest entry must survive; newest kept is generation %d", last.Status.ObservedGeneration)
	}
}

// The load-bearing guarantee: queued status is the autonomy story. A delivered
// entry is a convenience the centre already holds, and must never displace one
// the centre has not seen.
func TestPruningNeverDropsUndeliveredStatus(t *testing.T) {
	s := openTemp(t)

	// One undelivered entry from the start, then a long run of delivered ones
	// on top of it — an agent that missed a single report and kept going.
	first, err := s.AppendStatus(types.HostStatus{Phase: types.PhaseDegraded}, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < journalKeep*3; i++ {
		e, err := s.AppendStatus(types.HostStatus{ObservedGeneration: int64(i)}, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.MarkDelivered(e.Seq); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.PruneJournal(); err != nil {
		t.Fatal(err)
	}

	queued, err := s.Undelivered()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Seq != first.Seq {
		t.Fatalf("the undelivered entry must survive any amount of pruning; queued=%d", len(queued))
	}
	if queued[0].Status.Phase != types.PhaseDegraded {
		t.Fatalf("the queued entry must survive intact, got phase %q", queued[0].Status.Phase)
	}
}

// A centre unreachable for long enough would otherwise queue without limit,
// which is not autonomy — it is a second outage waiting for the disk to fill.
func TestTheOfflineQueueIsBounded(t *testing.T) {
	s := openTemp(t)

	for i := 0; i < journalMaxUndelivered+250; i++ {
		if _, err := s.AppendStatus(types.HostStatus{ObservedGeneration: int64(i)}, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PruneJournal(); err != nil {
		t.Fatal(err)
	}

	queued, err := s.Undelivered()
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) > journalMaxUndelivered {
		t.Fatalf("queue must be bounded: %d entries, want <= %d", len(queued), journalMaxUndelivered)
	}
	// The OLDEST go. The centre overwrites host status on each report, so the
	// newest is what restores an accurate view — dropping from the new end would
	// leave the centre permanently behind.
	newest := queued[len(queued)-1]
	if newest.Status.ObservedGeneration != int64(journalMaxUndelivered+249) {
		t.Fatalf("the newest queued status must survive; newest is generation %d", newest.Status.ObservedGeneration)
	}
}

// Pruning frees pages but bbolt never shrinks the file, so an agent upgrading
// from the unbounded version would sit on its full 512 MB for ever.
func TestOpenCompactsAFileLeftMostlyEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")

	// Build a store big enough to cross the compaction threshold, the way the
	// old code did: lots of delivered entries that nothing ever removed.
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 64<<10)
	for i := range blob {
		blob[i] = 'x'
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketJournal)
		if err != nil {
			return err
		}
		for i := 0; i < 700; i++ {
			if err := b.Put(itob(uint64(i)), blob); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() <= compactAboveBytes {
		t.Skipf("fixture did not exceed the compaction threshold (%d bytes)", before.Size())
	}

	// First open must NOT compact: nothing has pruned yet, so bolt.Compact would
	// copy every live entry — the walk that has to stay off the startup path.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mid, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mid.Size() != before.Size() {
		t.Fatalf("an unpruned store must not be compacted on open: %d -> %d bytes", before.Size(), mid.Size())
	}

	// Pruning is what the agent does in the background once it is up. It marks
	// the store, so the NEXT open knows compaction is now cheap.
	if err := s.PruneJournal(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() >= before.Size() {
		t.Fatalf("the open after a prune must reclaim the space: %d -> %d bytes", before.Size(), after.Size())
	}
	// And the store still works afterwards — a smaller file that lost the
	// desired state would be a far worse bug than a large one.
	if err := s.SaveDesiredHost(types.Host{Meta: types.ObjectMeta{Name: "h1"}}); err != nil {
		t.Fatal(err)
	}
	if h, ok, err := s.LoadDesiredHost(); err != nil || !ok || h.Meta.Name != "h1" {
		t.Fatalf("store unusable after compaction: ok=%v err=%v", ok, err)
	}
	// No temp file left behind.
	if _, err := os.Stat(path + ".compact"); !os.IsNotExist(err) {
		t.Fatal("compaction must not leave its temp file behind")
	}
}

// Desired state is what the agent enforces when the centre is unreachable.
// Pruning the journal must not touch it.
func TestPruningLeavesDesiredStateAlone(t *testing.T) {
	s := openTemp(t)
	want := types.Host{Meta: types.ObjectMeta{Name: "host01", Generation: 9}}
	if err := s.SaveDesiredHost(want); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < journalKeep*2; i++ {
		e, _ := s.AppendStatus(types.HostStatus{ObservedGeneration: int64(i)}, false)
		if err := s.MarkDelivered(e.Seq); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := s.LoadDesiredHost()
	if err != nil || !ok {
		t.Fatalf("desired state must survive pruning: ok=%v err=%v", ok, err)
	}
	if got.Meta.Generation != 9 {
		t.Fatalf("desired state changed under pruning: %+v", got.Meta)
	}
}
