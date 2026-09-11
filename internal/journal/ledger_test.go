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
