package asciicast

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, s string) (Header, [][]any) {
	t.Helper()
	lines := []string{}
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	var h Header
	if err := json.Unmarshal([]byte(lines[0]), &h); err != nil {
		t.Fatalf("header: %v", err)
	}
	var events [][]any
	for _, l := range lines[1:] {
		var e []any
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("event %q: %v", l, err)
		}
		if len(e) != 3 {
			t.Fatalf("event is not a 3-tuple: %q", l)
		}
		events = append(events, e)
	}
	return h, events
}

func TestV3StreamShape(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, Header{Term: Term{Cols: 120, Rows: 40, Type: "xterm-256color"}, Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1000, 0)
	if err := w.Output(base, []byte("first\r\n")); err != nil { // interval 0
		t.Fatal(err)
	}
	if err := w.Output(base.Add(250*time.Millisecond), []byte("\x1b[1mbold\x1b[0m")); err != nil {
		t.Fatal(err)
	}
	if err := w.Marker("gap"); err != nil {
		t.Fatal(err)
	}
	if err := w.Output(base.Add(100*time.Millisecond), []byte("back-in-time")); err != nil { // clamps to 0
		t.Fatal(err)
	}
	h, events := parse(t, buf.String())
	if h.Version != 3 || h.Term.Cols != 120 || h.Term.Rows != 40 {
		t.Fatalf("header %+v", h)
	}
	if len(events) != 4 {
		t.Fatalf("event count %d", len(events))
	}
	if events[0][0].(float64) != 0 || events[0][1] != "o" || events[0][2] != "first\r\n" {
		t.Fatalf("first event %v", events[0])
	}
	if d := events[1][0].(float64); d < 0.24 || d > 0.26 {
		t.Fatalf("interval %v", d)
	}
	if events[1][2] != "\x1b[1mbold\x1b[0m" {
		t.Fatalf("escape not preserved: %q", events[1][2])
	}
	if events[2][1] != "m" || events[2][2] != "gap" || events[2][0].(float64) != 0 {
		t.Fatalf("marker %v", events[2])
	}
	if events[3][0].(float64) != 0 {
		t.Fatalf("negative interval not clamped: %v", events[3])
	}
}

func TestHeaderDefaultsGeometry(t *testing.T) {
	var buf bytes.Buffer
	if _, err := NewWriter(&buf, Header{}); err != nil {
		t.Fatal(err)
	}
	var h Header
	_ = json.Unmarshal([]byte(strings.SplitN(buf.String(), "\n", 2)[0]), &h)
	if h.Term.Cols != 80 || h.Term.Rows != 24 {
		t.Fatalf("defaults %+v", h.Term)
	}
}
