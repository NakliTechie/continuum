// Package journal owns Continuum's single-writer durable metadata and event log.
package journal

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/NakliTechie/continuum/internal/jsonwire"
	bolt "go.etcd.io/bbolt"
)

var ErrLimit = errors.New("record limit reached")

type Store struct {
	db                  *bolt.DB
	Host                string
	MaxEvents, MaxBytes int
	// MaxOperations bounds the duplicate-request window: the most recent
	// resolved intents are retained; older identities are forgotten.
	MaxOperations int
}
type Block struct {
	HistoryIncomplete bool   `json:"history_incomplete"`
	ID                string `json:"id"`
	Agent             string `json:"agent"`
	PID               int    `json:"pid"`
	State             string `json:"state"`
	Started           string `json:"started_at"`
}
type Event struct {
	Version int             `json:"v"`
	Host    string          `json:"host_id"`
	Block   string          `json:"block_id"`
	Seq     uint64          `json:"seq"`
	Time    string          `json:"time"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}
type Page struct {
	Incomplete bool    `json:"incomplete"`
	Events     []Event `json:"events"`
	Next       uint64  `json:"next_cursor"`
	First      uint64  `json:"first_available"`
	Last       uint64  `json:"last_available"`
	Gap        bool    `json:"gap"`
}
type Intent struct {
	Digest string          `json:"digest"`
	Result json.RawMessage `json:"result,omitempty"`
}

func key(n uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, n); return b }
func num(b []byte) uint64 {
	if len(b) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}
func ID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func Open(dir string) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("state directory must be absolute")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("state directory must be a private directory (chmod 700)")
	}
	path := filepath.Join(dir, "state.db")
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("state.db must be a private regular file")
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 250 * time.Millisecond})
	if err != nil {
		return nil, fmt.Errorf("open state (another daemon may own it): %w", err)
	}
	s := &Store{db: db, MaxEvents: 4096, MaxBytes: 16 << 20, MaxOperations: 4096}
	err = db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists([]byte("meta"))
		if err != nil {
			return err
		}
		if v := meta.Get([]byte("schema")); v != nil && !bytes.Equal(v, []byte("1")) {
			return errors.New("unsupported state schema; restore with its matching binary")
		}
		if err := meta.Put([]byte("schema"), []byte("1")); err != nil {
			return err
		}
		for _, name := range []string{"blocks", "events", "operations", "operation_order", "output_bytes"} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		// Intents recorded before the order index existed get one entry each,
		// so the retention window can retire them too.
		if order := tx.Bucket([]byte("operation_order")); order.Stats().KeyN == 0 {
			if err := tx.Bucket([]byte("operations")).ForEach(func(k, _ []byte) error {
				seq, err := order.NextSequence()
				if err != nil {
					return err
				}
				return order.Put(key(seq), append([]byte(nil), k...))
			}); err != nil {
				return err
			}
		}
		s.Host = string(meta.Get([]byte("host")))
		if s.Host == "" {
			s.Host = ID()
			if err := meta.Put([]byte("host"), []byte(s.Host)); err != nil {
				return err
			}
		}
		unclean := bytes.Equal(meta.Get([]byte("running")), []byte("1"))
		if err := meta.Put([]byte("running"), []byte("1")); err != nil {
			return err
		}
		b := tx.Bucket([]byte("blocks"))
		return b.ForEach(func(k, v []byte) error {
			var block Block
			if err := json.Unmarshal(v, &block); err != nil {
				return err
			}
			if unclean {
				block.HistoryIncomplete = true
			}
			if block.State == "active" {
				block.State = "interrupted"
				block.HistoryIncomplete = true
				v, _ = json.Marshal(block)
				return b.Put(k, v)
			}
			if unclean {
				v, _ = json.Marshal(block)
				return b.Put(k, v)
			}
			return nil
		})
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }

// MaxBlocks bounds recorded blocks per host. At the cap the oldest block that
// is no longer active is retired with its events; only a host whose blocks are
// all active refuses a new one.
const MaxBlocks = 1024

func (s *Store) AddBlock(b Block) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("blocks"))
		if bucket.Get([]byte(b.ID)) == nil && bucket.Stats().KeyN >= MaxBlocks {
			oldest := ""
			var started string
			if err := bucket.ForEach(func(k, v []byte) error {
				var old Block
				if err := json.Unmarshal(v, &old); err != nil {
					return err
				}
				if old.State != "active" && (oldest == "" || old.Started < started) {
					oldest, started = string(k), old.Started
				}
				return nil
			}); err != nil {
				return err
			}
			if oldest == "" {
				return ErrLimit
			}
			if err := retireBlock(tx, oldest); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(b.ID), raw)
	})
}

// retireBlock removes a block record and its retained events, keeping the
// retention counters exact. Sequence numbers are never reused; readers skip
// the holes, and a cursor past the last surviving event moves to the
// high-water mark (see Read).
func retireBlock(tx *bolt.Tx, id string) error {
	if err := tx.Bucket([]byte("blocks")).Delete([]byte(id)); err != nil {
		return err
	}
	if ob := tx.Bucket([]byte("output_bytes")); ob != nil {
		if err := ob.Delete([]byte(id)); err != nil {
			return err
		}
	}
	events := tx.Bucket([]byte("events"))
	meta := tx.Bucket([]byte("meta"))
	size := num(meta.Get([]byte("event_bytes")))
	count := num(meta.Get([]byte("event_count")))
	var keys [][]byte
	if err := events.ForEach(func(k, v []byte) error {
		var e Event
		if err := json.Unmarshal(v, &e); err != nil {
			return err
		}
		if e.Block == id {
			keys = append(keys, append([]byte(nil), k...))
			size -= min(size, uint64(len(v)))
			count -= min(count, 1)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, k := range keys {
		if err := events.Delete(k); err != nil {
			return err
		}
	}
	if err := meta.Put([]byte("event_count"), key(count)); err != nil {
		return err
	}
	return meta.Put([]byte("event_bytes"), key(size))
}
func (s *Store) Blocks() ([]Block, error) {
	out := []Block{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("blocks")).ForEach(func(_, v []byte) error {
			var b Block
			if err := json.Unmarshal(v, &b); err != nil {
				return err
			}
			out = append(out, b)
			return nil
		})
	})
	return out, err
}

// MaxStructuredPayload accommodates an accepted ACP line plus its routing envelope.
const MaxStructuredPayload = (8 << 20) + (64 << 10)

func (s *Store) Append(id, kind string, payload any) error {
	return s.appendBounded(id, kind, payload, 256<<10, 0)
}

// AppendOutput records a PTY output chunk and advances the block's durable
// lifetime output-byte counter. That counter — never decremented by event
// retention — is the resume offset a holder streams from after a daemon
// restart, so replay stays contiguous and non-duplicated even once the oldest
// output events have been retired.
func (s *Store) AppendOutput(id string, b []byte, unobserved bool) error {
	payload := map[string]any{"encoding": "base64", "data": base64.StdEncoding.EncodeToString(b)}
	if unobserved {
		payload["unobserved"] = true
	}
	return s.appendBounded(id, "output", payload, 256<<10, len(b))
}

// OutputBytes returns the block's lifetime committed output-byte count.
func (s *Store) OutputBytes(block string) (int, error) {
	var n uint64
	err := s.db.View(func(tx *bolt.Tx) error {
		n = num(tx.Bucket([]byte("output_bytes")).Get([]byte(block)))
		return nil
	})
	return int(n), err
}

// AppendStructured retains a complete accepted frame. Ordinary event payloads
// keep their smaller admission limit; only the two ACP routes use this bound.
func (s *Store) AppendStructured(id, kind string, payload any) error {
	if kind != "session_update" && kind != "permission_request" {
		return fmt.Errorf("not a structured frame kind: %s", kind)
	}
	return s.appendBounded(id, kind, payload, MaxStructuredPayload, 0)
}

func (s *Store) appendBounded(id, kind string, payload any, limit, outLen int) error {
	raw, err := jsonwire.Marshal(payload)
	if err != nil {
		return err
	}
	if len(raw) > limit {
		return ErrLimit
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("events"))
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		e := Event{1, s.Host, id, seq, time.Now().UTC().Format(time.RFC3339Nano), kind, raw}
		data, err := jsonwire.Marshal(e)
		if err != nil {
			return err
		}
		if err = b.Put(key(seq), data); err != nil {
			return err
		}
		meta := tx.Bucket([]byte("meta"))
		size := num(meta.Get([]byte("event_bytes"))) + uint64(len(data))
		count := int(num(meta.Get([]byte("event_count")))) + 1
		for count > s.MaxEvents || size > uint64(s.MaxBytes) {
			c := b.Cursor()
			_, v := c.First()
			if v == nil {
				break
			}
			size -= uint64(len(v))
			if err := c.Delete(); err != nil {
				return err
			}
			count--
		}
		if err := meta.Put([]byte("event_count"), key(uint64(count))); err != nil {
			return err
		}
		if err := meta.Put([]byte("event_bytes"), key(size)); err != nil {
			return err
		}
		if outLen > 0 {
			ob := tx.Bucket([]byte("output_bytes"))
			if err := ob.Put([]byte(id), key(num(ob.Get([]byte(id)))+uint64(outLen))); err != nil {
				return err
			}
		}
		if kind == "exited" {
			blocks := tx.Bucket([]byte("blocks"))
			if v := blocks.Get([]byte(id)); v != nil {
				var block Block
				if err := json.Unmarshal(v, &block); err != nil {
					return err
				}
				block.State = "exited"
				v, _ = json.Marshal(block)
				return blocks.Put([]byte(id), v)
			}
		}
		return nil
	})
}
func (s *Store) Read(after uint64, block string) (Page, error) {
	p := Page{Events: []Event{}, Next: after}
	err := s.db.View(func(tx *bolt.Tx) error {
		blocks := tx.Bucket([]byte("blocks"))
		if err := blocks.ForEach(func(k, v []byte) error {
			if block != "" && string(k) != block {
				return nil
			}
			var b Block
			if err := json.Unmarshal(v, &b); err != nil {
				return err
			}
			if b.HistoryIncomplete {
				p.Incomplete = true
			}
			return nil
		}); err != nil {
			return err
		}
		b := tx.Bucket([]byte("events"))
		c := b.Cursor()
		first, _ := c.First()
		p.First = num(first)
		p.Last = b.Sequence()
		p.Gap = (p.First > 0 && after < p.First-1) || after > p.Last
		if p.Gap {
			return nil
		}
		if after == ^uint64(0) {
			return nil
		}
		size := 0
		scanned := 0
		// A retired block leaves holes in the sequence, possibly at its end;
		// a cursor with nothing left to read moves to the high-water mark so
		// readers wait for new work instead of re-reading an empty page.
		if k, _ := c.Seek(key(after + 1)); k == nil {
			p.Next = p.Last
			return nil
		}
		for k, v := c.Seek(key(after + 1)); k != nil; k, v = c.Next() {
			// A page normally stays below 512 KiB. One accepted larger frame
			// is returned whole, alone, so its cursor can always advance.
			if scanned >= 256 || (len(p.Events) > 0 && size+len(v) > 512<<10) {
				break
			}
			scanned++
			p.Next = num(k)
			var e Event
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			if block != "" && block != e.Block {
				continue
			}
			p.Events = append(p.Events, e)
			size += len(v)
		}
		return nil
	})
	return p, err
}

// Lookup returns a recorded intent for id, or nil when the identity is unknown
// or has left the retention window.
func (s *Store) Lookup(id string) (*Intent, error) {
	var old *Intent
	err := s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket([]byte("operations")).Get([]byte(id)); v != nil {
			old = &Intent{}
			return json.Unmarshal(v, old)
		}
		return nil
	})
	return old, err
}

// Begin atomically records an intent before a host effect. Existing unresolved
// intents are returned to the caller for reconciliation, never re-executed.
// The ledger keeps at most MaxOperations identities: the oldest resolved ones
// are retired to make room, so a long-lived daemon never refuses mutations;
// unresolved intents are never retired, and only they can exhaust the ledger.
func (s *Store) Begin(id, digest string) (*Intent, error) {
	var old *Intent
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("operations"))
		if v := b.Get([]byte(id)); v != nil {
			old = &Intent{}
			return json.Unmarshal(v, old)
		}
		order := tx.Bucket([]byte("operation_order"))
		// Select retirements with a read-only walk, then delete: a bbolt cursor
		// must not be advanced across its own deletions.
		// Stats() reads committed pages, so the live count is tracked by hand
		// across this transaction's own deletions.
		live := b.Stats().KeyN
		var retire [][2][]byte
		if live >= s.MaxOperations {
			c := order.Cursor()
			for k, v := c.First(); k != nil && live >= s.MaxOperations; k, v = c.Next() {
				if raw := b.Get(v); raw != nil {
					var it Intent
					if err := json.Unmarshal(raw, &it); err != nil {
						return err
					}
					if len(it.Result) == 0 {
						continue // unresolved: keep for reconciliation
					}
					live--
				}
				retire = append(retire, [2][]byte{append([]byte(nil), k...), append([]byte(nil), v...)})
			}
		}
		for _, r := range retire {
			if err := order.Delete(r[0]); err != nil {
				return err
			}
			if err := b.Delete(r[1]); err != nil {
				return err
			}
		}
		if live >= s.MaxOperations {
			return ErrLimit
		}
		seq, err := order.NextSequence()
		if err != nil {
			return err
		}
		if err := order.Put(key(seq), []byte(id)); err != nil {
			return err
		}
		raw, _ := json.Marshal(Intent{Digest: digest})
		return b.Put([]byte(id), raw)
	})
	return old, err
}
func (s *Store) Finish(id, digest string, result []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		raw, _ := json.Marshal(Intent{digest, result})
		return tx.Bucket([]byte("operations")).Put([]byte(id), raw)
	})
}

// CleanShutdown clears the durable running marker only after every process has
// drained and no capture failure occurred. An unclean epoch is conservatively
// marked incomplete on the next startup, including its previously exited blocks.
func (s *Store) CleanShutdown() error {
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("meta")).Put([]byte("running"), []byte("0")) })
}
