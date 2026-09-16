package store

import (
	"path/filepath"
	"testing"

	"github.com/halvantic/hyperv-control-plane/api/types"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDesiredHostSaveLoad(t *testing.T) {
	s := openTemp(t)

	if _, ok, err := s.LoadDesiredHost(); err != nil || ok {
		t.Fatalf("expected no desired host on first boot, ok=%v err=%v", ok, err)
	}

	h := types.Host{
		Meta: types.ObjectMeta{Name: "host01", UID: "u1", Generation: 4},
		Spec: types.HostSpec{FQDN: "host01.lab.local", RebootPolicy: types.RebootNever},
	}
	if err := s.SaveDesiredHost(h); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.LoadDesiredHost()
	if err != nil || !ok {
		t.Fatalf("load after save: ok=%v err=%v", ok, err)
	}
	if got.Meta.Name != "host01" || got.Meta.Generation != 4 {
		t.Fatalf("loaded host mismatch: %+v", got.Meta)
	}
}

func TestJournalQueueAndDeliver(t *testing.T) {
	s := openTemp(t)

	// Append three entries; the middle one already delivered.
	e1, err := s.AppendStatus(types.HostStatus{Phase: types.PhaseProgressing, Autonomous: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendStatus(types.HostStatus{Phase: types.PhaseProgressing}, true); err != nil {
		t.Fatal(err)
	}
	e3, err := s.AppendStatus(types.HostStatus{Phase: types.PhaseReady}, false)
	if err != nil {
		t.Fatal(err)
	}

	undelivered, err := s.Undelivered()
	if err != nil {
		t.Fatal(err)
	}
	if len(undelivered) != 2 {
		t.Fatalf("want 2 undelivered, got %d", len(undelivered))
	}
	if undelivered[0].Seq != e1.Seq || undelivered[1].Seq != e3.Seq {
		t.Fatalf("undelivered order/seq wrong: %d, %d", undelivered[0].Seq, undelivered[1].Seq)
	}

	// Deliver the first; only e3 should remain queued.
	if err := s.MarkDelivered(e1.Seq); err != nil {
		t.Fatal(err)
	}
	undelivered, err = s.Undelivered()
	if err != nil {
		t.Fatal(err)
	}
	if len(undelivered) != 1 || undelivered[0].Seq != e3.Seq {
		t.Fatalf("after delivery want only e3 queued, got %+v", undelivered)
	}

	// MarkDelivered is idempotent and safe on unknown sequences.
	if err := s.MarkDelivered(e1.Seq); err != nil {
		t.Fatalf("re-mark delivered: %v", err)
	}
	if err := s.MarkDelivered(9999); err != nil {
		t.Fatalf("mark unknown seq: %v", err)
	}
}

func TestRecentLimit(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < 5; i++ {
		if _, err := s.AppendStatus(types.HostStatus{ObservedGeneration: int64(i)}, false); err != nil {
			t.Fatal(err)
		}
	}
	recent, err := s.Recent(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 {
		t.Fatalf("want 2 recent, got %d", len(recent))
	}
	// Newest last: generations 3 then 4.
	if recent[0].Status.ObservedGeneration != 3 || recent[1].Status.ObservedGeneration != 4 {
		t.Fatalf("recent ordering wrong: %d, %d",
			recent[0].Status.ObservedGeneration, recent[1].Status.ObservedGeneration)
	}
}

// A job result the centre could not accept must survive to be replayed. Without
// this an agent that finished work while the centre was unreachable lost the
// outcome for good, and the job read Running in the console for ever — seen when
// the centre's machine slept through a storage migration.
func TestPendingJobResults(t *testing.T) {
	s := openTemp(t)

	if got, err := s.PendingResults(); err != nil || len(got) != 0 {
		t.Fatalf("a fresh store has nothing pending: %+v err=%v", got, err)
	}
	if err := s.AppendPendingResult("job-1", types.JobSucceeded, "moved storage"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendPendingResult("job-2", types.JobFailed, "no"); err != nil {
		t.Fatal(err)
	}

	got, err := s.PendingResults()
	if err != nil {
		t.Fatal(err)
	}
	// Order matters: a job's states are a sequence, and delivering them out of
	// order would report an older one last.
	if len(got) != 2 || got[0].JobID != "job-1" || got[1].JobID != "job-2" {
		t.Fatalf("results must replay in the order they happened: %+v", got)
	}
	if got[0].State != types.JobSucceeded || got[0].Message != "moved storage" {
		t.Fatalf("result not preserved: %+v", got[0])
	}
	if got[0].Time.IsZero() {
		t.Fatal("the queued-at time must be recorded, so a replay can say how old it is")
	}

	if err := s.DropPendingResult(got[0].Seq); err != nil {
		t.Fatal(err)
	}
	after, _ := s.PendingResults()
	if len(after) != 1 || after[0].JobID != "job-2" {
		t.Fatalf("only the delivered one may be dropped: %+v", after)
	}
}
