package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func o4Store(t *testing.T) *Store {
	t.Helper()
	s, e := Open(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func o4Payload(n int) json.RawMessage {
	return json.RawMessage(`"` + string(bytes.Repeat([]byte{'<'}, n-2)) + `"`)
}
func TestO4CheckerAdmissionBoundaries(t *testing.T) {
	s := o4Store(t)
	for _, c := range []struct {
		kind               string
		n                  int
		structured, accept bool
	}{{"output", (256 << 10) - 1, false, true}, {"output", 256 << 10, false, true}, {"output", (256 << 10) + 1, false, false}, {"session_update", 256 << 10, false, true}, {"session_update", (256 << 10) + 1, false, false}, {"session_update", MaxStructuredPayload - 1, true, true}, {"permission_request", MaxStructuredPayload, true, true}, {"session_update", MaxStructuredPayload + 1, true, false}, {"output", 10, true, false}} {
		t.Run(fmt.Sprintf("%s/%d/%v", c.kind, c.n, c.structured), func(t *testing.T) {
			var err error
			if c.structured {
				err = s.AppendStructured("a", c.kind, o4Payload(c.n))
			} else {
				err = s.Append("a", c.kind, o4Payload(c.n))
			}
			if (err == nil) != c.accept {
				t.Fatalf("accept=%v error=%v", c.accept, err)
			}
			if !c.accept && c.kind != "output" && !errors.Is(err, ErrLimit) {
				t.Fatalf("want ErrLimit got %v", err)
			}
		})
	}
}
func TestO4CheckerPaginationAndFilters(t *testing.T) {
	s := o4Store(t)
	want := map[uint64][]byte{}
	kinds := []struct {
		id string
		n  int
	}{{"chosen", 30}, {"other", 700 << 10}, {"chosen", 600 << 10}, {"other", (1 << 20) + 5}, {"chosen", 31}}
	for i, k := range kinds {
		p := o4Payload(k.n)
		if err := s.AppendStructured(k.id, "session_update", p); err != nil {
			t.Fatal(err)
		}
		if k.id == "chosen" {
			want[uint64(i+1)] = p
		}
	}
	for _, filter := range []string{"chosen", "other", "absent", ""} {
		after := uint64(0)
		seen := map[uint64]bool{}
		for count := 0; count < 10; count++ {
			p, err := s.Read(after, filter)
			if err != nil || p.Gap {
				t.Fatalf("read %v gap=%v", err, p.Gap)
			}
			if p.Next <= after && p.Next < p.Last {
				t.Fatalf("filter=%q stuck=%d", filter, after)
			}
			size := 0
			for _, e := range p.Events {
				b, _ := json.Marshal(e)
				size += len(b)
				if seen[e.Seq] {
					t.Fatal("duplicate")
				}
				seen[e.Seq] = true
				if filter != "" && filter != e.Block {
					t.Fatal("filter leak")
				}
				if expected, ok := want[e.Seq]; ok && !bytes.Equal(expected, e.Payload) {
					t.Fatal("payload changed")
				}
			}
			if size > 512<<10 && len(p.Events) != 1 {
				t.Fatalf("large page not alone: %d", len(p.Events))
			}
			after = p.Next
			if after == p.Last {
				break
			}
			if count == 9 {
				t.Fatal("cursor never finished")
			}
		}
		if filter == "chosen" && len(seen) != len(want) {
			t.Fatalf("missing filtered events %d", len(seen))
		}
		if filter == "absent" && len(seen) != 0 {
			t.Fatal("absent leak")
		}
		t.Logf("filter=%q final_cursor=%d selected=%d", filter, after, len(seen))
	}
	s2 := o4Store(t)
	for i := 0; i < 300; i++ {
		if err := s2.Append("other", "output", i); err != nil {
			t.Fatal(err)
		}
	}
	if err := s2.AppendStructured("chosen", "session_update", o4Payload(600<<10)); err != nil {
		t.Fatal(err)
	}
	p, err := s2.Read(0, "chosen")
	if err != nil || len(p.Events) != 0 || p.Next != 256 {
		t.Fatalf("scan limit events=%d next=%d err=%v", len(p.Events), p.Next, err)
	}
	p, err = s2.Read(p.Next, "chosen")
	if err != nil || len(p.Events) != 1 || p.Next != 301 {
		t.Fatalf("filtered continuation events=%d next=%d err=%v", len(p.Events), p.Next, err)
	}
	p, err = s2.Read(^uint64(0), "")
	if err != nil || !p.Gap {
		t.Fatal("cursor overflow did not report gap")
	}
}
func TestO4CheckerLogicalRetention(t *testing.T) {
	s := o4Store(t)
	if s.MaxBytes != 16<<20 || s.MaxEvents != 4096 {
		t.Fatalf("defaults bytes=%d events=%d", s.MaxBytes, s.MaxEvents)
	}
	for i := 0; i < 3; i++ {
		if err := s.AppendStructured("a", "session_update", o4Payload(8<<20)); err != nil {
			t.Fatal(err)
		}
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("events"))
		count, size := 0, 0
		if err := b.ForEach(func(_, v []byte) error { count++; size += len(v); return nil }); err != nil {
			return err
		}
		if size > s.MaxBytes || count > s.MaxEvents || count != 1 {
			t.Fatalf("retention count=%d bytes=%d", count, size)
		}
		meta := tx.Bucket([]byte("meta"))
		if num(meta.Get([]byte("event_bytes"))) != uint64(size) || num(meta.Get([]byte("event_count"))) != uint64(count) {
			t.Fatal("logical counters wrong")
		}
		t.Logf("logical retained bytes=%d count=%d", size, count)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Read(0, "")
	if err != nil || !p.Gap || p.First != 3 {
		t.Fatalf("gap=%v first=%d err=%v", p.Gap, p.First, err)
	}
	p, err = s.Read(p.First-1, "")
	if err != nil || len(p.Events) != 1 || len(p.Events[0].Payload) != 8<<20 {
		t.Fatal("retained large frame unavailable")
	}
	s2 := o4Store(t)
	for i := 0; i < 4097; i++ {
		if err := s2.Append("a", "output", 0); err != nil {
			t.Fatal(err)
		}
	}
	p, err = s2.Read(0, "")
	if err != nil || !p.Gap || p.First != 2 || p.Last != 4097 {
		t.Fatalf("count retention first=%d last=%d gap=%v err=%v", p.First, p.Last, p.Gap, err)
	}
}
