package journal

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// The duplicate-request ledger is a window, not a lifetime cap: once it is
// full the oldest resolved identities are retired, so a daemon that has handled
// thousands of keystrokes still accepts the next stop. Unresolved intents are
// never retired, and only they can exhaust it.
func TestLedgerRetiresOldestResolvedIntents(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.MaxOperations = 4
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("req-%02d", i)
		if old, err := s.Begin(id, "d"); err != nil || old != nil {
			t.Fatalf("begin %s: %v %v", id, old, err)
		}
		if err := s.Finish(id, "d", json.RawMessage(`{"ok":true}`)); err != nil {
			t.Fatal(err)
		}
	}
	if old, err := s.Lookup("req-09"); err != nil || old == nil {
		t.Fatalf("newest identity must survive: %v %v", old, err)
	}
	if old, err := s.Lookup("req-00"); err != nil || old != nil {
		t.Fatalf("oldest identity must be retired: %v %v", old, err)
	}
	if n := count(t, s, "operations"); n > 4 {
		t.Fatalf("ledger holds %d identities, window is 4", n)
	}
	if n := count(t, s, "operation_order"); n > 4 {
		t.Fatalf("order index holds %d entries, window is 4", n)
	}

	// Unresolved intents survive retirement and, alone, exhaust the window.
	s.MaxOperations = 2
	if _, err := s.Begin("hang-1", "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin("hang-2", "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin("hang-3", "d"); err != ErrLimit {
		t.Fatalf("two unresolved intents fill a window of two: got %v", err)
	}
	if old, err := s.Begin("hang-1", "other"); err != nil || old == nil || len(old.Result) != 0 {
		t.Fatalf("unresolved intent must still be returned for reconciliation: %v %v", old, err)
	}
	if err := s.Finish("hang-1", "d", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin("hang-3", "d"); err != nil {
		t.Fatalf("a resolved intent makes room: %v", err)
	}
}

// A state file written before the order index existed is indexed on open, so
// its old identities can be retired like any other.
func TestLedgerIndexesLegacyIntentsOnOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("old-%d", i)
		if _, err := s.Begin(id, "d"); err != nil {
			t.Fatal(err)
		}
		_ = s.Finish(id, "d", json.RawMessage(`1`))
	}
	if err := s.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte("operation_order")) }); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := count(t, s, "operation_order"); n != 3 {
		t.Fatalf("legacy intents indexed: %d", n)
	}
	s.MaxOperations = 3
	if _, err := s.Begin("new", "d"); err != nil {
		t.Fatal(err)
	}
	if old, _ := s.Lookup("old-0"); old != nil {
		t.Fatal("legacy identity was not retired")
	}
}

func count(t *testing.T, s *Store, bucket string) int {
	t.Helper()
	n := 0
	if err := s.db.View(func(tx *bolt.Tx) error { n = tx.Bucket([]byte(bucket)).Stats().KeyN; return nil }); err != nil {
		t.Fatal(err)
	}
	return n
}

// The block cap retires the oldest block that is no longer active, together
// with its retained events, so a long-lived host keeps accepting new work;
// only a host whose recorded blocks are all active refuses one.
func TestBlockCapRetiresOldestExitedBlock(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < MaxBlocks; i++ {
		id := fmt.Sprintf("blk-%04d", i)
		if err := s.AddBlock(Block{ID: id, State: "active", Started: fmt.Sprintf("2026-09-11T00:00:%02d.%06dZ", i/1000, i%1000)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddBlock(Block{ID: "one-more", State: "active", Started: "2026-09-12T00:00:00Z"}); err != ErrLimit {
		t.Fatalf("all-active host must refuse: %v", err)
	}
	// Exit two blocks out of start order; the earliest-started exited one goes first.
	for _, id := range []string{"blk-0007", "blk-0003"} {
		if err := s.Append(id, "output", map[string]any{"data": "x"}); err != nil {
			t.Fatal(err)
		}
		if err := s.Append(id, "exited", map[string]any{"exit_code": 0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddBlock(Block{ID: "one-more", State: "active", Started: "2026-09-12T00:00:00Z"}); err != nil {
		t.Fatalf("an exited block must make room: %v", err)
	}
	blocks, _ := s.Blocks()
	seen := map[string]bool{}
	for _, b := range blocks {
		seen[b.ID] = true
	}
	if seen["blk-0003"] || !seen["blk-0007"] || !seen["one-more"] || len(blocks) != MaxBlocks {
		t.Fatalf("retired the wrong block: has-0003=%v has-0007=%v has-new=%v n=%d", seen["blk-0003"], seen["blk-0007"], seen["one-more"], len(blocks))
	}
	page, err := s.Read(0, "blk-0003")
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("retired block's events must be gone: %+v %v", page, err)
	}
	if page, _ = s.Read(0, "blk-0007"); len(page.Events) != 2 {
		t.Fatalf("surviving block keeps its events: %+v", page)
	}
	var count, size uint64
	_ = s.db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		count, size = num(meta.Get([]byte("event_count"))), num(meta.Get([]byte("event_bytes")))
		var actual uint64
		_ = tx.Bucket([]byte("events")).ForEach(func(_, v []byte) error { actual += uint64(len(v)); return nil })
		if actual != size {
			t.Errorf("event_bytes %d, actual %d", size, actual)
		}
		return nil
	})
	if count != 2 {
		t.Fatalf("event_count after retirement: %d", count)
	}
}

// Retiring a block can remove the newest events; a cursor already past every
// surviving event must advance to the high-water mark rather than report an
// empty page that a follower would re-read forever.
func TestReadAdvancesPastRetiredTail(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"keep", "gone"} {
		if err := s.AddBlock(Block{ID: id, State: "active", Started: "2026-09-11T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Append("keep", "output", map[string]any{"data": "a"})    // seq 1
	_ = s.Append("gone", "output", map[string]any{"data": "b"})    // seq 2
	_ = s.Append("gone", "exited", map[string]any{"exit_code": 0}) // seq 3
	if err := s.db.Update(func(tx *bolt.Tx) error { return retireBlock(tx, "gone") }); err != nil {
		t.Fatal(err)
	}
	page, err := s.Read(1, "")
	if err != nil || page.Gap || len(page.Events) != 0 || page.Next != 3 || page.Last != 3 {
		t.Fatalf("cursor after the last surviving event must land on the high-water mark: %+v %v", page, err)
	}
	if page, _ = s.Read(0, ""); len(page.Events) != 1 || page.Next != 1 {
		t.Fatalf("surviving events still read: %+v", page)
	}
	_ = s.Append("keep", "output", map[string]any{"data": "c"}) // seq 4
	if page, _ = s.Read(3, ""); len(page.Events) != 1 || page.Next != 4 {
		t.Fatalf("follower resumes after retirement: %+v", page)
	}
}
