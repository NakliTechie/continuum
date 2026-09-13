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

var (
	ErrLimit    = errors.New("record limit reached")
	ErrNotFound = errors.New("block not found")
	ErrActive   = errors.New("active block cannot be retired")
	ErrBusy     = errors.New("state database is in use")
)

const (
	RecordingNone    = "none"
	RecordingVisible = "visible"
	RecordingLines   = "lines"
	RecordingFull    = "full"
)

// NormalizeRecording validates the per-block recording selector. Empty is the
// legacy spelling of full, so old clients and existing schema-1 records retain
// their behavior. Lines is deliberately bounded independently of byte
// retention; the host-wide event/byte limits remain the final ceiling.
func NormalizeRecording(mode string, lines int) (string, int, error) {
	if mode == "" {
		mode = RecordingFull
	}
	switch mode {
	case RecordingNone, RecordingVisible, RecordingFull:
		if lines != 0 {
			return "", 0, errors.New("recording_lines is only valid with recording=lines")
		}
	case RecordingLines:
		if lines < 1 || lines > 10000 {
			return "", 0, errors.New("recording_lines must be between 1 and 10000")
		}
	default:
		return "", 0, errors.New("recording must be none, visible, lines, or full")
	}
	return mode, lines, nil
}

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
	Recording         string `json:"recording"`
	RecordingLines    int    `json:"recording_lines,omitempty"`
	RecordingPurged   bool   `json:"recording_purged,omitempty"`
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
	Incomplete      bool    `json:"incomplete"`
	RecordingPurged bool    `json:"recording_purged,omitempty"`
	Recording       string  `json:"recording,omitempty"`
	RecordingLines  int     `json:"recording_lines,omitempty"`
	Events          []Event `json:"events"`
	Next            uint64  `json:"next_cursor"`
	First           uint64  `json:"first_available"`
	Last            uint64  `json:"last_available"`
	Gap             bool    `json:"gap"`
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
		if v := meta.Get([]byte("schema")); v != nil && !bytes.Equal(v, []byte("1")) && !bytes.Equal(v, []byte("2")) {
			return errors.New("unsupported state schema; restore with its matching binary")
		}
		for _, name := range []string{"blocks", "events", "operations", "operation_order", "output_bytes", "recording_screens", "recording_visible_event", "recording_visible_dirty", "recording_line_state"} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		// Schema 2 is an additive, transactional transition that makes rollback
		// refusal explicit: schema-1 binaries would otherwise ignore a block's
		// privacy-sensitive recording policy and resume full capture.
		if err := meta.Put([]byte("schema"), []byte("2")); err != nil {
			return err
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
			changed := false
			if block.Recording == "" {
				block.Recording = RecordingFull
				changed = true
			}
			if unclean {
				block.HistoryIncomplete = true
				changed = true
			}
			if block.State == "active" {
				block.State = "interrupted"
				block.HistoryIncomplete = true
				changed = true
			}
			if changed {
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
	mode, lines, err := NormalizeRecording(b.Recording, b.RecordingLines)
	if err != nil {
		return err
	}
	b.Recording, b.RecordingLines = mode, lines
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
	for _, name := range []string{"recording_screens", "recording_visible_event", "recording_visible_dirty", "recording_line_state"} {
		if b := tx.Bucket([]byte(name)); b != nil {
			if err := b.Delete([]byte(id)); err != nil {
				return err
			}
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
			b.Recording, b.RecordingLines, _ = NormalizeRecording(b.Recording, b.RecordingLines)
			out = append(out, b)
			return nil
		})
	})
	return out, err
}

// Block returns one normalized block record.
func (s *Store) Block(id string) (Block, error) {
	var out Block
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte("blocks")).Get([]byte(id))
		if v == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(v, &out); err != nil {
			return err
		}
		out.Recording, out.RecordingLines, _ = NormalizeRecording(out.Recording, out.RecordingLines)
		return nil
	})
	return out, err
}

// Retire removes an exited block and all state scoped to it. Active or
// interrupted work is never killed, orphaned, or forgotten as a side effect
// of storage management.
func (s *Store) Retire(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte("blocks")).Get([]byte(id))
		if v == nil {
			return ErrNotFound
		}
		var block Block
		if err := json.Unmarshal(v, &block); err != nil {
			return err
		}
		if block.State != "exited" {
			return ErrActive
		}
		return retireBlock(tx, id)
	})
}

// PurgeRecording deletes an exited block's retained PTY output and durable
// visible screen while preserving its block and lifecycle/control events.
func (s *Store) PurgeRecording(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		blocks := tx.Bucket([]byte("blocks"))
		v := blocks.Get([]byte(id))
		if v == nil {
			return ErrNotFound
		}
		var block Block
		if err := json.Unmarshal(v, &block); err != nil {
			return err
		}
		if block.State != "exited" {
			return ErrActive
		}
		var keys [][]byte
		if err := tx.Bucket([]byte("events")).ForEach(func(k, raw []byte) error {
			var e Event
			if err := json.Unmarshal(raw, &e); err != nil {
				return err
			}
			if e.Block == id && e.Type == "output" {
				keys = append(keys, append([]byte(nil), k...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, k := range keys {
			if err := deleteEvent(tx, k); err != nil {
				return err
			}
		}
		for _, name := range []string{"recording_screens", "recording_visible_event", "recording_visible_dirty", "recording_line_state"} {
			if err := tx.Bucket([]byte(name)).Delete([]byte(id)); err != nil {
				return err
			}
		}
		block.RecordingPurged = true
		raw, err := json.Marshal(block)
		if err != nil {
			return err
		}
		if err := blocks.Put([]byte(id), raw); err != nil {
			return err
		}
		if _, err := appendEvent(tx, s.Host, id, "recording_purged", map[string]any{"recording": normalizedMode(block)}, 256<<10); err != nil {
			return err
		}
		return enforceRetention(tx, s.MaxEvents, s.MaxBytes)
	})
}

func normalizedMode(block Block) string {
	mode, _, _ := NormalizeRecording(block.Recording, block.RecordingLines)
	return mode
}

// RecordedScreen decodes the last committed frame for a visible recording.
func (s *Store) RecordedScreen(id string, dst any) (bool, error) {
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte("recording_screens")).Get([]byte(id))
		if raw == nil {
			return nil
		}
		found = true
		return json.Unmarshal(raw, dst)
	})
	return found, err
}

// ReplaceVisibleScreen commits a final or explicitly refreshed visible frame
// without advancing the lifetime byte offset.
func (s *Store) ReplaceVisibleScreen(id string, screen any, rendered []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte("blocks")).Get([]byte(id))
		if v == nil {
			return ErrNotFound
		}
		var block Block
		if err := json.Unmarshal(v, &block); err != nil {
			return err
		}
		if normalizedMode(block) != RecordingVisible {
			return nil
		}
		if tx.Bucket([]byte("recording_visible_dirty")).Get([]byte(id)) == nil {
			return nil // a late duplicate exit refresh must not resurrect a purge
		}
		raw, err := jsonwire.Marshal(screen)
		if err != nil {
			return err
		}
		if len(raw) > MaxStructuredPayload {
			return ErrLimit
		}
		if old := tx.Bucket([]byte("recording_visible_event")).Get([]byte(id)); old != nil {
			if err := deleteEvent(tx, old); err != nil {
				return err
			}
		}
		if err := tx.Bucket([]byte("recording_screens")).Put([]byte(id), raw); err != nil {
			return err
		}
		payload := map[string]any{"encoding": "base64", "data": base64.StdEncoding.EncodeToString(rendered), "snapshot": true}
		seq, err := appendEvent(tx, s.Host, id, "output", payload, MaxStructuredPayload)
		if err != nil {
			return err
		}
		if err := tx.Bucket([]byte("recording_visible_event")).Put([]byte(id), key(seq)); err != nil {
			return err
		}
		return enforceRetention(tx, s.MaxEvents, s.MaxBytes)
	})
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
	return s.AppendOutputScreen(id, b, nil, nil, unobserved)
}

// AppendOutputScreen applies a block's recording policy while always advancing
// its committed lifetime output offset. screen is the JSON-serializable
// terminal snapshot and rendered is a byte stream that can redraw that frame.
// They are required only by the visible policy.
func (s *Store) AppendOutputScreen(id string, b []byte, screen any, rendered []byte, unobserved bool) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		blocks := tx.Bucket([]byte("blocks"))
		v := blocks.Get([]byte(id))
		if v == nil {
			return ErrNotFound
		}
		var block Block
		if err := json.Unmarshal(v, &block); err != nil {
			return err
		}
		mode, lines, err := NormalizeRecording(block.Recording, block.RecordingLines)
		if err != nil {
			return err
		}
		if err := advanceOutputBytes(tx, id, len(b)); err != nil {
			return err
		}
		if mode == RecordingNone {
			return nil
		}
		data := b
		payload := map[string]any{"encoding": "base64", "data": base64.StdEncoding.EncodeToString(data)}
		if unobserved {
			payload["unobserved"] = true
		}
		if mode == RecordingVisible {
			if screen == nil || rendered == nil {
				return errors.New("visible recording requires a terminal snapshot")
			}
			raw, err := jsonwire.Marshal(screen)
			if err != nil {
				return err
			}
			if len(raw) > MaxStructuredPayload {
				return ErrLimit
			}
			payload["data"] = base64.StdEncoding.EncodeToString(rendered)
			payload["snapshot"] = true
			if old := tx.Bucket([]byte("recording_visible_event")).Get([]byte(id)); old != nil {
				if err := deleteEvent(tx, old); err != nil {
					return err
				}
			}
			if err := tx.Bucket([]byte("recording_screens")).Put([]byte(id), raw); err != nil {
				return err
			}
			if err := tx.Bucket([]byte("recording_visible_dirty")).Put([]byte(id), []byte{1}); err != nil {
				return err
			}
		}
		limit := 256 << 10
		if mode == RecordingVisible {
			limit = MaxStructuredPayload
		}
		seq, err := appendEvent(tx, s.Host, id, "output", payload, limit)
		if err != nil {
			return err
		}
		if mode == RecordingVisible {
			if err := tx.Bucket([]byte("recording_visible_event")).Put([]byte(id), key(seq)); err != nil {
				return err
			}
		}
		if mode == RecordingLines {
			if err := addLineEvent(tx, id, b); err != nil {
				return err
			}
			if err := trimOutputLines(tx, id, lines); err != nil {
				return err
			}
		}
		return enforceRetention(tx, s.MaxEvents, s.MaxBytes)
	})
}

func advanceOutputBytes(tx *bolt.Tx, id string, n int) error {
	if n == 0 {
		return nil
	}
	ob := tx.Bucket([]byte("output_bytes"))
	return ob.Put([]byte(id), key(num(ob.Get([]byte(id)))+uint64(n)))
}

type outputPayload struct {
	Encoding string `json:"encoding"`
	Data     string `json:"data"`
	Snapshot bool   `json:"snapshot,omitempty"`
}

type lineState struct {
	Breaks      int  `json:"breaks"`
	Events      int  `json:"events"`
	HasOutput   bool `json:"has_output"`
	LastNewline bool `json:"last_newline"`
}

func readLineState(tx *bolt.Tx, id string) (lineState, error) {
	var state lineState
	if raw := tx.Bucket([]byte("recording_line_state")).Get([]byte(id)); raw != nil {
		if err := json.Unmarshal(raw, &state); err != nil {
			return lineState{}, err
		}
	}
	return state, nil
}

func writeLineState(tx *bolt.Tx, id string, state lineState) error {
	b := tx.Bucket([]byte("recording_line_state"))
	if state.Events == 0 {
		return b.Delete([]byte(id))
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return b.Put([]byte(id), raw)
}

func addLineEvent(tx *bolt.Tx, id string, data []byte) error {
	state, err := readLineState(tx, id)
	if err != nil {
		return err
	}
	state.Breaks += bytes.Count(data, []byte{'\n'})
	state.Events++
	if len(data) > 0 {
		state.HasOutput = true
		state.LastNewline = data[len(data)-1] == '\n'
	}
	return writeLineState(tx, id, state)
}

func decodedOutput(raw []byte) ([]byte, bool) {
	var e Event
	if json.Unmarshal(raw, &e) != nil || e.Type != "output" {
		return nil, false
	}
	var p outputPayload
	if json.Unmarshal(e.Payload, &p) != nil || p.Encoding != "base64" || p.Snapshot {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(p.Data)
	return b, err == nil
}

// trimOutputLines removes the oldest raw output through the excess newline
// boundary. The surviving first event is rewritten only when the boundary
// falls inside it; event sequence/timestamp stay stable.
func trimOutputLines(tx *bolt.Tx, id string, keep int) error {
	events := tx.Bucket([]byte("events"))
	state, err := readLineState(tx, id)
	if err != nil {
		return err
	}
	targetBreaks := keep - 1
	if state.HasOutput && state.LastNewline {
		targetBreaks = keep
	}
	excess := state.Breaks - targetBreaks
	if excess <= 0 {
		return nil
	}
	type candidate struct {
		key   []byte
		raw   []byte
		event Event
		data  []byte
	}
	var candidates []candidate
	covered := 0
	if err := events.ForEach(func(k, v []byte) error {
		if covered >= excess {
			return nil
		}
		var e Event
		if json.Unmarshal(v, &e) != nil || e.Block != id {
			return nil
		}
		data, ok := decodedOutput(v)
		if !ok {
			return nil
		}
		candidates = append(candidates, candidate{key: append([]byte(nil), k...), raw: append([]byte(nil), v...), event: e, data: data})
		covered += bytes.Count(data, []byte{'\n'})
		return nil
	}); err != nil {
		return err
	}
	for _, candidate := range candidates {
		k, v, e, data := candidate.key, candidate.raw, candidate.event, candidate.data
		breaks := bytes.Count(data, []byte{'\n'})
		if breaks <= excess {
			excess -= breaks
			if err := deleteEvent(tx, append([]byte(nil), k...)); err != nil {
				return err
			}
			continue
		}
		cut := 0
		for i := 0; i < excess; i++ {
			n := bytes.IndexByte(data[cut:], '\n')
			if n < 0 {
				break
			}
			cut += n + 1
		}
		var p map[string]any
		if json.Unmarshal(e.Payload, &p) != nil {
			return errors.New("invalid output payload")
		}
		p["data"] = base64.StdEncoding.EncodeToString(data[cut:])
		e.Payload, _ = jsonwire.Marshal(p)
		updated, err := jsonwire.Marshal(e)
		if err != nil {
			return err
		}
		meta := tx.Bucket([]byte("meta"))
		size := num(meta.Get([]byte("event_bytes")))
		size -= min(size, uint64(len(v)))
		size += uint64(len(updated))
		if err := events.Put(k, updated); err != nil {
			return err
		}
		if err := meta.Put([]byte("event_bytes"), key(size)); err != nil {
			return err
		}
		state, err := readLineState(tx, id)
		if err != nil {
			return err
		}
		state.Breaks -= min(state.Breaks, excess)
		if err := writeLineState(tx, id, state); err != nil {
			return err
		}
		excess = 0
	}
	return nil
}

func appendEvent(tx *bolt.Tx, host, id, kind string, payload any, limit int) (uint64, error) {
	raw, err := jsonwire.Marshal(payload)
	if err != nil {
		return 0, err
	}
	if len(raw) > limit {
		return 0, ErrLimit
	}
	events := tx.Bucket([]byte("events"))
	seq, err := events.NextSequence()
	if err != nil {
		return 0, err
	}
	e := Event{1, host, id, seq, time.Now().UTC().Format(time.RFC3339Nano), kind, raw}
	data, err := jsonwire.Marshal(e)
	if err != nil {
		return 0, err
	}
	if err := events.Put(key(seq), data); err != nil {
		return 0, err
	}
	meta := tx.Bucket([]byte("meta"))
	count := num(meta.Get([]byte("event_count"))) + 1
	size := num(meta.Get([]byte("event_bytes"))) + uint64(len(data))
	if err := meta.Put([]byte("event_count"), key(count)); err != nil {
		return 0, err
	}
	if err := meta.Put([]byte("event_bytes"), key(size)); err != nil {
		return 0, err
	}
	return seq, nil
}

func deleteEvent(tx *bolt.Tx, k []byte) error {
	events := tx.Bucket([]byte("events"))
	v := events.Get(k)
	if v == nil {
		return nil
	}
	meta := tx.Bucket([]byte("meta"))
	count := num(meta.Get([]byte("event_count")))
	size := num(meta.Get([]byte("event_bytes")))
	var event Event
	if json.Unmarshal(v, &event) == nil && tx.Bucket([]byte("recording_line_state")).Get([]byte(event.Block)) != nil {
		if data, ok := decodedOutput(v); ok {
			state, err := readLineState(tx, event.Block)
			if err != nil {
				return err
			}
			state.Breaks -= min(state.Breaks, bytes.Count(data, []byte{'\n'}))
			state.Events -= min(state.Events, 1)
			if state.Events == 0 {
				state = lineState{}
			}
			if err := writeLineState(tx, event.Block, state); err != nil {
				return err
			}
		}
	}
	if err := events.Delete(k); err != nil {
		return err
	}
	count -= min(count, 1)
	size -= min(size, uint64(len(v)))
	if err := meta.Put([]byte("event_count"), key(count)); err != nil {
		return err
	}
	return meta.Put([]byte("event_bytes"), key(size))
}

func enforceRetention(tx *bolt.Tx, maxEvents, maxBytes int) error {
	events := tx.Bucket([]byte("events"))
	meta := tx.Bucket([]byte("meta"))
	count := num(meta.Get([]byte("event_count")))
	size := num(meta.Get([]byte("event_bytes")))
	for count > uint64(maxEvents) || size > uint64(maxBytes) {
		k, _ := events.Cursor().First()
		if k == nil {
			break
		}
		if err := deleteEvent(tx, append([]byte(nil), k...)); err != nil {
			return err
		}
		count = num(meta.Get([]byte("event_count")))
		size = num(meta.Get([]byte("event_bytes")))
	}
	return nil
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

// AdvanceOutput commits bytes that cannot be represented by the selected
// recording policy (for example, a visible-screen block that exited while no
// daemon owned a terminal engine). Callers separately mark that block's
// history incomplete; holder resume still must not replay these bytes twice.
func (s *Store) AdvanceOutput(block string, n int) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("blocks")).Get([]byte(block)) == nil {
			return ErrNotFound
		}
		return advanceOutputBytes(tx, block, n)
	})
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
	return s.db.Update(func(tx *bolt.Tx) error {
		if _, err := appendEvent(tx, s.Host, id, kind, payload, limit); err != nil {
			return err
		}
		if err := advanceOutputBytes(tx, id, outLen); err != nil {
			return err
		}
		if err := enforceRetention(tx, s.MaxEvents, s.MaxBytes); err != nil {
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
			if b.RecordingPurged {
				p.RecordingPurged = true
			}
			if block != "" {
				p.Recording, p.RecordingLines, _ = NormalizeRecording(b.Recording, b.RecordingLines)
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

// Compact atomically replaces an offline state.db with a compacted copy. The
// source lock is held through copy and rename, so a running daemon makes this
// fail with bbolt's timeout instead of racing the replacement.
func Compact(dir string) (before, after int64, err error) {
	if !filepath.IsAbs(dir) {
		return 0, 0, errors.New("state directory must be absolute")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return 0, 0, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return 0, 0, errors.New("state directory must be a private directory (chmod 700)")
	}
	path := filepath.Join(dir, "state.db")
	info, err = os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return 0, 0, errors.New("state.db must be a private regular file")
	}
	before = info.Size()
	src, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 250 * time.Millisecond})
	if err != nil {
		if errors.Is(err, bolt.ErrTimeout) {
			return 0, 0, fmt.Errorf("%w: stop the daemon first", ErrBusy)
		}
		return 0, 0, fmt.Errorf("open state exclusively (stop the daemon first): %w", err)
	}
	defer src.Close()
	tmp, err := os.CreateTemp(dir, ".state-compact-")
	if err != nil {
		return 0, 0, err
	}
	tmpPath := tmp.Name()
	err = tmp.Close()
	if err != nil {
		return 0, 0, err
	}
	defer os.Remove(tmpPath)
	dst, err := bolt.Open(tmpPath, 0600, nil)
	if err == nil {
		err = bolt.Compact(dst, src, 64<<20)
	}
	if dst != nil {
		if closeErr := dst.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return 0, 0, err
	}
	if compacted, statErr := os.Lstat(tmpPath); statErr != nil {
		return 0, 0, statErr
	} else if !compacted.Mode().IsRegular() || compacted.Size() == 0 || compacted.Mode().Perm()&0077 != 0 {
		return 0, 0, errors.New("compacted state failed validation")
	} else {
		after = compacted.Size()
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return 0, 0, err
	}
	// Persist the directory entry replacement where the platform supports it.
	if d, openErr := os.Open(dir); openErr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return before, after, nil
}
