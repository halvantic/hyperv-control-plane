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
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/joshua-fourie/ballast/api/types"
)

var (
	bucketDesired = []byte("desired")
	bucketJournal = []byte("journal")

	keyHost = []byte("host")
	keyVMs  = []byte("vms")
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
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bbolt at %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketDesired, bucketJournal} {
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
		return b.Put(itob(seq), updated)
	})
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
