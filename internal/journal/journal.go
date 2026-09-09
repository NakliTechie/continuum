// Package journal owns Continuum's single-writer durable metadata and event log.
package journal

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var ErrLimit = errors.New("record limit reached")

type Store struct {
	db                  *bolt.DB
	Host                string
	MaxEvents, MaxBytes int
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
	s := &Store{db: db, MaxEvents: 4096, MaxBytes: 8 << 20}
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
		for _, name := range []string{"blocks", "events", "operations"} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
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
func (s *Store) AddBlock(b Block) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("blocks"))
		if bucket.Get([]byte(b.ID)) == nil && bucket.Stats().KeyN >= 1024 {
			return ErrLimit
		}
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(b.ID), raw)
	})
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
func (s *Store) Append(id, kind string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if len(raw) > 256<<10 {
		return ErrLimit
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("events"))
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		e := Event{1, s.Host, id, seq, time.Now().UTC().Format(time.RFC3339Nano), kind, raw}
		data, err := json.Marshal(e)
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
		for k, v := c.Seek(key(after + 1)); k != nil; k, v = c.Next() {
			if scanned >= 256 || size+len(v) > 512<<10 {
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

// Begin atomically records an intent before a host effect. Existing unresolved
// intents are returned to the caller for reconciliation, never re-executed.
func (s *Store) Begin(id, digest string) (*Intent, error) {
	var old *Intent
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("operations"))
		if v := b.Get([]byte(id)); v != nil {
			old = &Intent{}
			return json.Unmarshal(v, old)
		}
		if b.Stats().KeyN >= 4096 {
			return ErrLimit
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
