package journal

import (
	"encoding/json"
	"errors"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Objects are bounded, private feature metadata in the same durable transaction
// domain as the journal. The caller owns its schema and authorization.
func validSpace(space string) bool {
	switch space {
	case "waits", "peers", "grants", "sharing", "audit", "managed_workspaces":
		return true
	}
	return false
}

type AuditEntry struct {
	Order       uint64 `json:"order"`
	Time        string `json:"time"`
	Fingerprint string `json:"fingerprint"`
	Operation   string `json:"operation"`
	Decision    string `json:"decision"`
	Block       string `json:"block_id,omitempty"`
}

// AppendAudit keeps the newest 4096 bounded decisions. No token or request
// body is accepted into this type. Security-sensitive callers fail closed if
// this transaction does not commit.
func (s *Store) AppendAudit(entry AuditEntry) error {
	if len(entry.Fingerprint) > 32 || len(entry.Operation) > 64 || len(entry.Decision) > 32 || len(entry.Block) > 128 {
		return ErrLimit
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("feature_audit"))
		if err != nil {
			return err
		}
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		entry.Order, entry.Time = seq, time.Now().UTC().Format(time.RFC3339Nano)
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if err := b.Put(key(seq), raw); err != nil {
			return err
		}
		if b.Stats().KeyN > 4096 {
			first, _ := b.Cursor().First()
			if first != nil {
				return b.Delete(first)
			}
		}
		return nil
	})
}

func (s *Store) AuditPage(after uint64) ([]AuditEntry, uint64, uint64, bool, error) {
	out := make([]AuditEntry, 0, 100)
	next := after
	first := uint64(0)
	gap := false
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("feature_audit"))
		if b == nil {
			return nil
		}
		oldest, _ := b.Cursor().First()
		first = num(oldest)
		gap = first > 0 && after < first-1
		if gap {
			return nil
		}
		if after == ^uint64(0) {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Seek(key(after + 1)); k != nil && len(out) < 100; k, v = c.Next() {
			var entry AuditEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return err
			}
			out = append(out, entry)
			next = entry.Order
		}
		return nil
	})
	return out, next, first, gap, err
}

func (s *Store) Object(space, id string) (json.RawMessage, error) {
	if !validSpace(space) {
		return nil, errors.New("unknown metadata namespace")
	}
	var raw json.RawMessage
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("feature_" + space))
		if b == nil || b.Get([]byte(id)) == nil {
			return ErrNotFound
		}
		raw = append(raw, b.Get([]byte(id))...)
		return nil
	})
	return raw, err
}

func (s *Store) Objects(space string) (map[string]json.RawMessage, error) {
	if !validSpace(space) {
		return nil, errors.New("unknown metadata namespace")
	}
	all := map[string]json.RawMessage{}
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("feature_" + space))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error { all[string(k)] = append(json.RawMessage(nil), v...); return nil })
	})
	return all, err
}

// UpdateObject serializes read/modify/write and assigns a monotonic namespace
// sequence in the same commit. Returning an error rolls back both. nil deletes.
func (s *Store) UpdateObject(space, id string, limit int, f func(json.RawMessage, uint64) (json.RawMessage, error)) error {
	if !validSpace(space) || id == "" || len(id) > 256 || limit < 1 || limit > 4096 {
		return errors.New("invalid metadata key/limit")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("feature_" + space))
		if err != nil {
			return err
		}
		old := b.Get([]byte(id))
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		raw, err := f(append(json.RawMessage(nil), old...), seq)
		if err != nil {
			return err
		}
		if raw == nil {
			return b.Delete([]byte(id))
		}
		if len(raw) > 64<<10 || !json.Valid(raw) {
			return ErrLimit
		}
		if old == nil && b.Stats().KeyN >= limit {
			return ErrLimit
		}
		return b.Put([]byte(id), raw)
	})
}

type Observation struct {
	Host       string `json:"host_id"`
	Block      string `json:"block_id"`
	State      string `json:"state"`
	Turn       uint64 `json:"turn"`
	Cursor     uint64 `json:"cursor"`
	Incomplete bool   `json:"incomplete"`
}

func Lifecycle(kind string) bool {
	switch kind {
	case "running", "idle", "done", "needs_input", "stalled", "rate_limited", "exited", "unknown":
		return true
	}
	return false
}

// Observe snapshots committed lifecycle and its replay boundary atomically.
func (s *Store) Observe(id string) (Observation, error) {
	o := Observation{Host: s.Host, Block: id}
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte("blocks")).Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		var b Block
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		o.State, o.Turn, o.Incomplete = b.Lifecycle, b.Turn, b.HistoryIncomplete
		switch b.State {
		case "exited":
			o.State = "exited"
		case "interrupted":
			o.State = "unknown"
		}
		if o.State == "" {
			o.State = "running"
		}
		o.Cursor = tx.Bucket([]byte("events")).Sequence()
		return nil
	})
	return o, err
}
