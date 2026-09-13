package journal

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func outputText(t *testing.T, s *Store, block string) (string, int) {
	t.Helper()
	p, err := s.Read(0, block)
	if err != nil {
		t.Fatal(err)
	}
	if p.Gap && p.First > 0 {
		p, err = s.Read(p.First-1, block)
		if err != nil {
			t.Fatal(err)
		}
	}
	var out strings.Builder
	n := 0
	for _, e := range p.Events {
		if e.Type != "output" {
			continue
		}
		var payload outputPayload
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		b, err := base64.StdEncoding.DecodeString(payload.Data)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(b)
		n++
	}
	return out.String(), n
}

func TestRecordingPoliciesPreserveResumeOffsets(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, block := range []Block{
		{ID: "none", State: "active", Recording: RecordingNone},
		{ID: "lines", State: "active", Recording: RecordingLines, RecordingLines: 2},
		{ID: "visible", State: "active", Recording: RecordingVisible},
	} {
		if err := s.AddBlock(block); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendOutput("none", []byte("secret"), false); err != nil {
		t.Fatal(err)
	}
	if got, n := outputText(t, s, "none"); got != "" || n != 0 {
		t.Fatalf("none retained output %q (%d events)", got, n)
	}
	if n, _ := s.OutputBytes("none"); n != len("secret") {
		t.Fatalf("none lost holder resume offset: %d", n)
	}

	for _, chunk := range []string{"one\n", "two\n", "three"} {
		if err := s.AppendOutput("lines", []byte(chunk), false); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := outputText(t, s, "lines"); got != "two\nthree" {
		t.Fatalf("lines retained %q", got)
	}
	if n, _ := s.OutputBytes("lines"); n != len("one\ntwo\nthree") {
		t.Fatalf("lines lost holder resume offset: %d", n)
	}

	if err := s.AppendOutputScreen("visible", []byte("raw-one"), map[string]any{"revision": 1}, []byte("screen-one"), false); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendOutputScreen("visible", []byte("raw-two"), map[string]any{"revision": 2}, []byte("screen-two"), false); err != nil {
		t.Fatal(err)
	}
	if got, n := outputText(t, s, "visible"); got != "screen-two" || n != 1 {
		t.Fatalf("visible retained %q (%d events)", got, n)
	}
	var frame struct {
		Revision int `json:"revision"`
	}
	if found, err := s.RecordedScreen("visible", &frame); err != nil || !found || frame.Revision != 2 {
		t.Fatalf("recorded screen: found=%v frame=%+v err=%v", found, frame, err)
	}
	if n, _ := s.OutputBytes("visible"); n != len("raw-oneraw-two") {
		t.Fatalf("visible lost holder resume offset: %d", n)
	}
	if err := s.Append("visible", "exited", map[string]int{"exit_code": 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeRecording("visible"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceVisibleScreen("visible", map[string]any{"revision": 3}, []byte("must-not-return")); err != nil {
		t.Fatal(err)
	}
	if found, err := s.RecordedScreen("visible", &frame); err != nil || found {
		t.Fatalf("exit refresh resurrected a purged screen: found=%v err=%v", found, err)
	}
}

func TestPurgeAndRetireBoundaries(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.AddBlock(Block{ID: "blk", State: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append("blk", "checkpoint", map[string]bool{"kept": true}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendOutput("blk", []byte("discard me"), false); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeRecording("blk"); !errors.Is(err, ErrActive) {
		t.Fatalf("active purge: %v", err)
	}
	if err := s.Retire("blk"); !errors.Is(err, ErrActive) {
		t.Fatalf("active retire: %v", err)
	}
	if err := s.AddBlock(Block{ID: "uncertain", State: "interrupted"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeRecording("uncertain"); !errors.Is(err, ErrActive) {
		t.Fatalf("interrupted purge: %v", err)
	}
	if err := s.Retire("uncertain"); !errors.Is(err, ErrActive) {
		t.Fatalf("interrupted retire: %v", err)
	}
	if err := s.Append("blk", "exited", map[string]int{"exit_code": 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeRecording("blk"); err != nil {
		t.Fatal(err)
	}
	p, err := s.Read(0, "blk")
	if err != nil || !p.RecordingPurged {
		t.Fatalf("purge marker: %+v %v", p, err)
	}
	kinds := map[string]bool{}
	for _, e := range p.Events {
		kinds[e.Type] = true
	}
	if kinds["output"] || !kinds["checkpoint"] || !kinds["recording_purged"] {
		t.Fatalf("purge event set: %v", kinds)
	}
	if n, _ := s.OutputBytes("blk"); n != len("discard me") {
		t.Fatalf("purge reset resume offset: %d", n)
	}
	if err := s.Retire("blk"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Block("blk"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retired block remains: %v", err)
	}
}

func TestLinePolicyStateTracksGlobalEviction(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.MaxEvents = 1
	if err := s.AddBlock(Block{ID: "lines", State: "active", Recording: RecordingLines, RecordingLines: 100}); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []string{"one\n", "two\n", "three\n"} {
		if err := s.AppendOutput("lines", []byte(chunk), false); err != nil {
			t.Fatal(err)
		}
	}
	if got, n := outputText(t, s, "lines"); got != "three\n" || n != 1 {
		t.Fatalf("global eviction left stale line state: %q (%d)", got, n)
	}
}

func TestCompactRequiresOfflineStateAndPreservesData(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddBlock(Block{ID: "keep", State: "exited"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		if err := s.Append("keep", "note", strings.Repeat("x", 1024)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := Compact(dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("online compaction was not fenced: %v", err)
	}
	if err := s.CleanShutdown(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, after, err := Compact(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before <= 0 || after <= 0 || after > before {
		t.Fatalf("invalid sizes %d -> %d", before, after)
	}
	if info, err := os.Stat(filepath.Join(dir, "state.db")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("compacted mode: %v %v", info, err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if block, err := reopened.Block("keep"); err != nil || block.Recording != RecordingFull {
		t.Fatalf("compacted data: %+v %v", block, err)
	}
}
