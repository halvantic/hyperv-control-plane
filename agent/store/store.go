// Package store is the agent's embedded, on-disk memory. It holds two things
// that must survive both agent restarts and the centre being unreachable:
//
//   - the last-honoured Host desired state, so the agent keeps enforcing the
//     same intent autonomously when it cannot reach the centre; and
//   - a status journal, an append-only record of reported status that queues
//     locally while the centre is offline and drains when it returns.
//
// The store is bbolt (a single-file, transactional key/value store) so the
// agent has no external dependency. Values are JSON-encoded api/types so the
// on-disk form tracks the schema directly.
package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/joshua-fourie/ballast/api/types"
)

// Journal retention.
//
// The journal used to be append-only in the literal sense: MarkDelivered set a
// flag and nothing ever deleted anything. One full HostStatus — inventory,
// metrics, every condition — was written per cycle and kept for ever. On the
// rig that reached sequence ~136,000 and agent.db grew to between 512 MB and
// 1 GB per host.
//
// Size was the lesser problem. Undelivered() and Recent() walk the whole bucket
// and JSON-unmarshal every entry, so the cost of each delivery attempt grew with
// the age of the agent, and status reports had started timing out with
// DeadlineExceeded on hosts that were otherwise healthy.
//
// The journal exists so status survives the centre being unreachable. That needs
// the undelivered queue and a little history for diagnosis; it never needed a
// permanent archive.
const (
	// journalKeep is how many DELIVERED entries are retained for diagnostics.
	// Anything older is redundant: the centre already has it.
	journalKeep = 200
	// journalMaxUndelivered bounds the offline queue. Reaching it means the
	// centre has been unreachable for a very long time, and the oldest entries
	// are dropped first — the centre overwrites host status on each report, so
	// the newest is what restores an accurate view. What is lost is some of the
	// transition history the centre derives events from, which is worth less
	// than an agent whose store grows without limit.
	journalMaxUndelivered = 5000
	// compactAboveBytes is when Open rebuilds the file. bbolt reuses freed pages
	// but never shrinks the file, so pruning alone leaves an agent upgrading from
	// the unbounded version sitting on its full 512 MB alone.
	compactAboveBytes = 32 << 20
	// pruneEvery is how often a delivery triggers a prune, rather than every one.
	//
	// Pruning walks the bucket, so doing it per delivery would reintroduce a
	// per-report scan — a smaller one than the bug being fixed, but the same
	// shape. Amortising it lets the journal drift this far past the tail between
	// sweeps, which costs a few hundred KB and takes the scan off the hot path.
	pruneEvery = 64
)

var (
	bucketDesired = []byte("desired")
	bucketJournal = []byte("journal")

	keyHost = []byte("host")
	keyVMs  = []byte("vms")
	// bucketResults holds terminal job results the centre has not accepted yet.
	bucketResults = []byte("results")
)

// Store is the agent's embedded store. Safe for concurrent use: bbolt
// serialises writes and allows concurrent reads.
type Store struct {
	db *bolt.DB
}

// JournalEntry is one recorded status report. Delivered tracks whether the
// centre has acknowledged it; undelivered entries are what the agent replays
// when the centre comes back.
type JournalEntry struct {
	Seq       uint64           `json:"seq"`
	Time      time.Time        `json:"time"`
	Status    types.HostStatus `json:"status"`
	Delivered bool             `json:"delivered"`
}

// Open opens or creates the store at path.
//
// It prunes the journal on the way in, and rebuilds the file when pruning has
// left it mostly empty space. Both matter most on the first start after an
// upgrade from the version that never deleted anything: that agent arrives with
// a 512 MB file and a six-figure journal, and would otherwise carry both for
// ever.
func Open(path string) (*Store, error) {
	s, err := openAt(path)
	if err != nil {
		return nil, err
	}
	if err := s.PruneJournal(); err != nil {
		// Not fatal. A store that cannot prune still works; it just stays large,
		// and the caller has no better option than carrying on.
		_ = s.Close()
		return nil, fmt.Errorf("prune journal: %w", err)
	}
	// Compaction has to happen with the database closed, so it is done here
	// rather than left to a caller who would have to know that.
	if fi, statErr := os.Stat(path); statErr == nil && fi.Size() > compactAboveBytes {
		if err := s.Close(); err != nil {
			return nil, fmt.Errorf("close before compact: %w", err)
		}
		if err := compactFile(path); err != nil {
			// A failed compaction must not stop the agent: the data is still
			// intact in the original file, it is merely bigger than it needs to
			// be. Reopen and carry on.
			return openAt(path)
		}
		return openAt(path)
	}
	return s, nil
}

func openAt(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bbolt at %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketDesired, bucketJournal, bucketResults} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init buckets: %w", err)
	}
	return &Store{db: db}, nil
}

// compactFile rebuilds the database into a fresh file and swaps it in, which is
// the only way to actually reclaim space from bbolt.
//
// Written to a sibling temp file and renamed, so an interruption leaves either
// the old complete file or the new one — never a half-written store. The temp
// file is removed first in case a previous attempt died mid-way.
func compactFile(path string) error {
	tmp := path + ".compact"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear stale compaction file: %w", err)
	}
	src, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("open source for compaction: %w", err)
	}
	defer src.Close()
	dst, err := bolt.Open(tmp, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("create compacted store: %w", err)
	}
	if err := bolt.Compact(dst, src, 0); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("compact: %w", err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close compacted store: %w", err)
	}
	if err := src.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close source: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("swap in compacted store: %w", err)
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// SaveDesiredHost records the host as the last-honoured desired state,
// replacing any previous copy. This is the state the agent falls back to when
// the centre is unreachable.
func (s *Store) SaveDesiredHost(h types.Host) error {
	data, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("marshal desired host: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketDesired).Put(keyHost, data)
	})
}

// LoadDesiredHost returns the last-honoured desired state. ok is false when the
// agent has never persisted any desired state (first boot before first pull).
func (s *Store) LoadDesiredHost() (host types.Host, ok bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketDesired).Get(keyHost)
		if data == nil {
			return nil
		}
		if err := json.Unmarshal(data, &host); err != nil {
			return fmt.Errorf("unmarshal desired host: %w", err)
		}
		ok = true
		return nil
	})
	return host, ok, err
}

// SaveDesiredVMs records the VMs placed on this host as last-honoured desired
// state, replacing any previous copy. Like the host spec, this is what the agent
// keeps enforcing when the centre is unreachable. An empty slice is stored
// faithfully (the centre having removed every VM is itself intent to honour).
func (s *Store) SaveDesiredVMs(vms []types.VM) error {
	data, err := json.Marshal(vms)
	if err != nil {
		return fmt.Errorf("marshal desired vms: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketDesired).Put(keyVMs, data)
	})
}

// LoadDesiredVMs returns the last-honoured VMs. ok is false when the agent has
// never persisted a VM set (distinct from an empty set, which the centre may
// legitimately have authored).
func (s *Store) LoadDesiredVMs() (vms []types.VM, ok bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketDesired).Get(keyVMs)
		if data == nil {
			return nil
		}
		if err := json.Unmarshal(data, &vms); err != nil {
			return fmt.Errorf("unmarshal desired vms: %w", err)
		}
		ok = true
		return nil
	})
	return vms, ok, err
}

// AppendStatus records a status report in the journal, assigning it the next
// sequence number, and returns the stored entry. Newly appended entries are
// undelivered until MarkDelivered records that the centre accepted them.
func (s *Store) AppendStatus(status types.HostStatus, delivered bool) (JournalEntry, error) {
	var entry JournalEntry
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketJournal)
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		entry = JournalEntry{
			Seq:       seq,
			Time:      time.Now().UTC(),
			Status:    status,
			Delivered: delivered,
		}
		data, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("marshal journal entry: %w", err)
		}
		return b.Put(itob(seq), data)
	})
	if err != nil {
		return JournalEntry{}, err
	}
	return entry, nil
}

// Undelivered returns journal entries not yet acknowledged by the centre, in
// sequence order. These are what the agent replays on reconnect.
func (s *Store) Undelivered() ([]JournalEntry, error) {
	var out []JournalEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketJournal).ForEach(func(_, v []byte) error {
			var e JournalEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return fmt.Errorf("unmarshal journal entry: %w", err)
			}
			if !e.Delivered {
				out = append(out, e)
			}
			return nil
		})
	})
	return out, err
}

// MarkDelivered flags the journal entry with the given sequence as delivered.
// It is a no-op if the entry does not exist.
func (s *Store) MarkDelivered(seq uint64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketJournal)
		data := b.Get(itob(seq))
		if data == nil {
			return nil
		}
		var e JournalEntry
		if err := json.Unmarshal(data, &e); err != nil {
			return fmt.Errorf("unmarshal journal entry: %w", err)
		}
		if e.Delivered {
			return nil
		}
		e.Delivered = true
		updated, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("marshal journal entry: %w", err)
		}
		if err := b.Put(itob(seq), updated); err != nil {
			return err
		}
		// Prune in the same transaction that delivered it, so the journal is
		// bounded by construction: it can only grow while the centre is
		// unreachable, which is the case it exists for. Amortised over
		// pruneEvery deliveries to keep the scan off the per-report path.
		if seq%pruneEvery != 0 {
			return nil
		}
		return pruneJournalTx(b)
	})
}

// PruneJournal drops journal entries that are no longer needed: delivered ones
// beyond the diagnostic tail, and the oldest undelivered ones once the offline
// queue exceeds its bound.
//
// Exposed so Open can run it once on the way in — an agent upgrading from the
// version that never pruned needs its backlog cleared before anything reads the
// bucket, not after the next successful delivery.
func (s *Store) PruneJournal() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return pruneJournalTx(tx.Bucket(bucketJournal))
	})
}

// pruneJournalTx does the work inside a caller's transaction.
//
// Keys are big-endian sequence numbers, so cursor order is oldest first and the
// scan walks exactly the entries eligible for deletion.
//
// Two rules, in order of importance:
//
//   - an UNDELIVERED entry is never dropped to make room for a delivered one.
//     Queued status is the autonomy guarantee; a delivered copy is a
//     convenience the centre already holds.
//   - once undelivered entries alone exceed journalMaxUndelivered, the oldest
//     go. An unbounded queue is not autonomy, it is a second outage waiting for
//     the disk to fill.
func pruneJournalTx(b *bolt.Bucket) error {
	if b == nil {
		return nil
	}
	var delivered, undelivered int
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		var e JournalEntry
		if err := json.Unmarshal(v, &e); err != nil {
			// An entry we cannot read is not one we can reason about, and it
			// would otherwise block the scan for ever. Count it as droppable.
			delivered++
			continue
		}
		if e.Delivered {
			delivered++
		} else {
			undelivered++
		}
	}
	dropDelivered := delivered - journalKeep
	dropUndelivered := undelivered - journalMaxUndelivered
	if dropDelivered <= 0 && dropUndelivered <= 0 {
		return nil
	}
	// Second pass deletes from the oldest end. Collect keys first: deleting
	// through a cursor while iterating it is only safe via c.Delete(), and the
	// two counters make the intent clearer than an in-place dance.
	var kill [][]byte
	c = b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if dropDelivered <= 0 && dropUndelivered <= 0 {
			break
		}
		var e JournalEntry
		bad := json.Unmarshal(v, &e) != nil
		switch {
		case (bad || e.Delivered) && dropDelivered > 0:
			dropDelivered--
		case !bad && !e.Delivered && dropUndelivered > 0:
			dropUndelivered--
		default:
			continue
		}
		key := make([]byte, len(k))
		copy(key, k)
		kill = append(kill, key)
	}
	for _, k := range kill {
		if err := b.Delete(k); err != nil {
			return fmt.Errorf("prune journal entry: %w", err)
		}
	}
	return nil
}

// Recent returns up to limit most-recent journal entries, newest last. A limit
// of 0 or less returns all entries.
func (s *Store) Recent(limit int) ([]JournalEntry, error) {
	var all []JournalEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketJournal).ForEach(func(_, v []byte) error {
			var e JournalEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return fmt.Errorf("unmarshal journal entry: %w", err)
			}
			all = append(all, e)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

func itob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// ---------------------------------------------------------------------------
// Pending job results
// ---------------------------------------------------------------------------

// PendingResult is a job outcome the agent produced but could not deliver.
//
// The status journal has always been replayed when the centre returns — that is
// the autonomy story — but a job's RESULT was fire-and-forget: reportJob logged a
// warning and moved on. So an agent that finished work while the centre was
// unreachable lost the outcome for ever, and the job sat Running in the console
// until somebody cancelled it.
//
// Seen on a storage migration: the centre's machine slept mid-move, the agent
// completed the migration, and hours later the files were in their new home
// while the job still read "live migration 20%". The work was never at risk. The
// record of it was.
//
// Only TERMINAL results are worth keeping. Progress notes describe a moment that
// has passed by the time the centre is back, and replaying "20%" for a job that
// finished would be worse than silence.
type PendingResult struct {
	Seq     uint64         `json:"seq"`
	Time    time.Time      `json:"time"`
	JobID   string         `json:"jobId"`
	State   types.JobState `json:"state"`
	Message string         `json:"message"`
}

// AppendPendingResult records an undelivered terminal job result for replay.
func (s *Store) AppendPendingResult(jobID string, state types.JobState, msg string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketResults)
		if err != nil {
			return err
		}
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		e := PendingResult{Seq: seq, Time: time.Now().UTC(), JobID: jobID, State: state, Message: msg}
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		return b.Put(itob(seq), raw)
	})
}

// PendingResults returns the undelivered job results in the order they happened,
// which is the order they must be replayed in: a job's states are a sequence, and
// delivering them out of order would report an older one last.
func (s *Store) PendingResults() ([]PendingResult, error) {
	var out []PendingResult
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketResults)
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var e PendingResult
			if err := json.Unmarshal(v, &e); err != nil {
				return nil // a corrupt entry must not block the rest
			}
			out = append(out, e)
			return nil
		})
	})
	return out, err
}

// DropPendingResult removes a result once the centre has accepted it.
func (s *Store) DropPendingResult(seq uint64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketResults)
		if b == nil {
			return nil
		}
		return b.Delete(itob(seq))
	})
}
