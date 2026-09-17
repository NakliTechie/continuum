package journal

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func TestFeatureObjectsCommitBoundsAndLifecycle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	put := func(value string) func(json.RawMessage, uint64) (json.RawMessage, error) {
		return func(_ json.RawMessage, seq uint64) (json.RawMessage, error) {
			return json.Marshal(map[string]any{"value": value, "seq": seq})
		}
	}
	if err := s.UpdateObject("waits", "one", 1, put("first")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateObject("waits", "two", 1, put("second")); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	before, _ := s.Object("waits", "one")
	if err := s.UpdateObject("waits", "one", 1, func(json.RawMessage, uint64) (json.RawMessage, error) { return nil, errors.New("disk simulation") }); err == nil {
		t.Fatal("accepted failed mutation")
	}
	after, _ := s.Object("waits", "one")
	if string(before) != string(after) {
		t.Fatal("failed transaction changed object")
	}
	if err := s.AddBlock(Block{ID: "one", State: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append("one", "done", map[string]any{"turn": uint64(7)}); err != nil {
		t.Fatal(err)
	}
	o, err := s.Observe("one")
	if err != nil || o.State != "done" || o.Turn != 7 || o.Cursor == 0 {
		t.Fatal(o, err)
	}
	if err := s.Append("one", "idle", map[string]any{"turn": uint64(7)}); err != nil {
		t.Fatal(err)
	}
	p, err := s.Read(o.Cursor, "one")
	if err != nil || len(p.Events) != 1 || p.Events[0].Type != "idle" {
		t.Fatal(p, err)
	}
}
