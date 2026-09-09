package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestRecoveryRetentionAndIntent(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	host := s.Host
	if _, err := Open(dir); err == nil {
		t.Fatal("two writers opened state")
	}
	if err := s.AddBlock(Block{ID: "a", State: "active"}); err != nil {
		t.Fatal(err)
	}
	s.MaxEvents = 2
	for i := 0; i < 3; i++ {
		if err := s.Append("a", "output", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.Read(0, "")
	if err != nil || !p.Gap || p.First != 2 {
		t.Fatalf("retention must signal gap: %+v %v", p, err)
	}
	p, err = s.Read(1, "a")
	if err != nil || len(p.Events) != 2 || p.Next != 3 {
		t.Fatalf("replay %+v %v", p, err)
	}
	if old, err := s.Begin("req", "digest"); err != nil || old != nil {
		t.Fatal(old, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Host != host {
		t.Fatal("host identity changed")
	}
	blocks, err := s.Blocks()
	if err != nil || blocks[0].State != "interrupted" {
		t.Fatal(blocks, err)
	}
	old, err := s.Begin("req", "digest")
	if err != nil || old == nil || old.Digest != "digest" || len(old.Result) != 0 {
		t.Fatal(old, err)
	}
	if err := s.Finish("req", "digest", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	old, err = s.Begin("req", "other")
	if err != nil || old.Digest != "digest" || string(old.Result) != `{"ok":true}` {
		t.Fatal(old, err)
	}
}
func TestSchemaRefusesFutureVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(dir, "state.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		b, e := tx.CreateBucket([]byte("meta"))
		if e != nil {
			return e
		}
		return b.Put([]byte("schema"), []byte("999"))
	})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if s, err := Open(dir); err == nil {
		s.Close()
		t.Fatal("future schema accepted")
	}
}
func TestFilteredReplayAdvancesAndByteBudget(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.MaxBytes = 400
	for i := 0; i < 5; i++ {
		if err := s.Append("a", "output", map[string]string{"data": "some output"}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.Read(0, "")
	if err != nil || !p.Gap {
		t.Fatal(p, err)
	}
	p, err = s.Read(p.First-1, "other")
	if err != nil || p.Next != p.Last || len(p.Events) != 0 {
		t.Fatal(p, err)
	}
}

func TestUncleanEpochPersistsIncompleteHistoryEvenForExitedBlocks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddBlock(Block{ID: "old", State: "exited"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Append("old", "output", map[string]string{"data": "captured"}); err != nil {
		t.Fatal(err)
	}
	// An oversized/faulted append is not captured. There is no clean marker.
	if err = s.Append("old", "output", string(make([]byte, 300<<10))); err == nil {
		t.Fatal("expected capture failure")
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.Read(0, "old")
	if err != nil || !p.Incomplete {
		t.Fatal("restart concealed uncertain history", p, err)
	}
}
