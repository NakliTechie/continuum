package terminal

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func newEngine(t *testing.T, c, r int) Engine {
	t.Helper()
	e, err := New(c, r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}
func feed(t *testing.T, e Engine, b string) []byte {
	t.Helper()
	r, err := e.Feed([]byte(b))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestDetachedQueriesAndModes(t *testing.T) {
	e := newEngine(t, 80, 24)
	reply := feed(t, e, "\x1b[4;7H\x1b[6n\x1b[5n\x1b[c\x1b[>c\x1b]11;?\x1b\\")
	for _, want := range []string{"\x1b[4;7R", "\x1b[0n", "\x1b[?62;1;6;22c", "\x1b[>1;10;0c", "\x1b]11;rgb:"} {
		if !strings.Contains(string(reply), want) {
			t.Fatalf("missing %q in %q", want, reply)
		}
	}
	feed(t, e, "\x1b[?1h\x1b[?2004h\x1b[?25l")
	s := e.Snapshot()
	if !s.Modes.CursorKeys || !s.Modes.BracketedPaste || s.Cursor.Visible {
		t.Fatalf("modes: %+v", s)
	}
	if got := string(feed(t, e, "\x1b[?2004$p")); got != "\x1b[?2004;1$y" {
		t.Fatalf("mode reply %q", got)
	}
}
func TestSplitSequencesAndUTF8(t *testing.T) {
	e := newEngine(t, 20, 4)
	whole := newEngine(t, 20, 4)
	transcript := "hi\x1b[2;3H\x1b[31m界\x1b[0m\x1b]2;title\x07!"
	feed(t, whole, transcript)
	for _, b := range []byte(transcript) {
		feed(t, e, string([]byte{b}))
	}
	a, b := e.Snapshot(), whole.Snapshot()
	if !reflect.DeepEqual(a.Lines, b.Lines) || !reflect.DeepEqual(a.ANSI, b.ANSI) || a.Cursor != b.Cursor {
		t.Fatalf("split mismatch: %+v / %+v", a, b)
	}
}
func TestAlternateRestoreResizeAndScrollback(t *testing.T) {
	e := newEngine(t, 20, 4)
	feed(t, e, "primary\x1b[?1049hALT")
	s := e.Snapshot()
	if !s.Alternate || !strings.HasPrefix(s.Lines[0], "ALT") {
		t.Fatal(s)
	}
	feed(t, e, "\x1b[?1049l")
	s = e.Snapshot()
	if s.Alternate || !strings.HasPrefix(s.Lines[0], "primary") {
		t.Fatal(s)
	}
	for i := 0; i < 500; i++ {
		feed(t, e, "line\r\n")
	}
	if n := e.Snapshot().Scrollback; n > ScrollbackLines || n == 0 {
		t.Fatal(n)
	}
	if _, err := e.Resize(40, 10); err != nil {
		t.Fatal(err)
	}
	s = e.Snapshot()
	if s.Cols != 40 || s.Rows != 10 || s.Cursor.X >= 40 || s.Cursor.Y >= 10 {
		t.Fatal(s)
	}
}
func TestFaultsAndBounds(t *testing.T) {
	for _, size := range [][2]int{{0, 1}, {241, 1}, {240, 100}, {-1, 4}} {
		if e, err := New(size[0], size[1]); err == nil {
			e.Close()
			t.Fatalf("accepted %v", size)
		}
	}
	for _, bad := range []string{"\x1b[999999999b", "\x9b999999999b", "e" + strings.Repeat("\u0301", 150), "\x1b]2;" + strings.Repeat("x", MaxSequence)} {
		e := newEngine(t, 20, 4)
		_, err := e.Feed([]byte(bad))
		if !errors.Is(err, ErrLimit) || e.Snapshot().Fault == "" {
			t.Fatalf("fault %v", err)
		}
		if _, err := e.Feed([]byte("later")); !errors.Is(err, ErrLimit) {
			t.Fatal(err)
		}
	}
	e := newEngine(t, 20, 4)
	before := e.Snapshot()
	if _, err := e.Resize(999, 999); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, e.Snapshot()) {
		t.Fatal("invalid resize mutated state")
	}
}
func TestSnapshotsDoNotReplyOrExposeApplicationEscapes(t *testing.T) {
	e := newEngine(t, 20, 4)
	feed(t, e, "\x1b]8;;javascript:evil\x1b\\hello\x1b]8;;\x1b\\\x1b]52;c;ignored\x07")
	a := e.Snapshot()
	a.Lines[0] = "changed"
	b := e.Snapshot()
	if strings.Contains(strings.Join(b.ANSI, ""), "javascript") || strings.Contains(strings.Join(b.ANSI, ""), "\x1b]") {
		t.Fatal(b)
	}
	if !strings.HasPrefix(b.Lines[0], "hello") {
		t.Fatal(b)
	}
	if replies := feed(t, e, ""); len(replies) != 0 {
		t.Fatalf("snapshot generated %q", replies)
	}
}
func TestConcurrentSnapshotsResizeAndClose(t *testing.T) {
	e := newEngine(t, 80, 24)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Go(func() {
			for j := 0; j < 80; j++ {
				_, _ = e.Feed([]byte("\x1b[6ntext"))
				_ = e.Snapshot()
				_, _ = e.Resize(80, 24)
			}
		})
	}
	wg.Wait()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Feed([]byte("x")); err == nil {
		t.Fatal("write after close")
	}
}

// A frame is rebuilt only when the revision moves: repeated snapshots of an
// idle screen return the same content, and any write invalidates it.
func TestSnapshotIsCachedPerRevision(t *testing.T) {
	e, err := New(20, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Feed([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	a, b := e.Snapshot(), e.Snapshot()
	if a.Revision != b.Revision || a.Lines[0] != b.Lines[0] || !strings.HasPrefix(a.Lines[0], "hello") {
		t.Fatalf("idle snapshots differ: %+v %+v", a.Lines, b.Lines)
	}
	if _, err := e.Feed([]byte(" world")); err != nil {
		t.Fatal(err)
	}
	c := e.Snapshot()
	if c.Revision == b.Revision || !strings.HasPrefix(c.Lines[0], "hello world") {
		t.Fatalf("write did not invalidate the cached frame: rev %d vs %d %q", c.Revision, b.Revision, c.Lines[0])
	}
}
