package terminal

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestCheckerOversizeFeedFaults(t *testing.T) {
	e := newEngine(t, 20, 4)
	feed(t, e, "before")
	rev := e.Snapshot().Revision
	_, err := e.Feed([]byte(strings.Repeat("x", MaxChunk+1)))
	s := e.Snapshot()
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("error=%v", err)
	}
	if s.Fault == "" || s.Revision <= rev {
		t.Errorf("rejected output left snapshot unfaulted: error=%v fault=%q revision=%d prior=%d", err, s.Fault, s.Revision, rev)
	}
	if _, err = e.Feed([]byte("later")); !errors.Is(err, ErrLimit) {
		t.Errorf("later Feed accepted: %v", err)
	}
}

func TestCheckerNumericLimitWithEmbeddedControl(t *testing.T) {
	for _, control := range []string{"\x00", "\x07", "\x0d"} {
		t.Run(strings.ReplaceAll(control, "\x00", "NUL"), func(t *testing.T) {
			e := newEngine(t, 20, 4)
			input := "x\x1b[4096" + control + "1b"
			_, err := e.Feed([]byte(input))
			s := e.Snapshot()
			if !errors.Is(err, ErrLimit) || s.Fault == "" {
				t.Errorf("accepted REP parameter 40961 with control %q: error=%v fault=%q scrollback=%d cursor=%+v", control, err, s.Fault, s.Scrollback, s.Cursor)
			}
		})
	}
}

func TestCheckerStringLimitBypasses(t *testing.T) {
	for name, input := range map[string]string{
		"DCS_BEL":        "\x1bPq\x07" + strings.Repeat("x", MaxSequence+1) + "\x1b\\",
		"APC_BEL":        "\x1b_\x07" + strings.Repeat("x", MaxSequence+1) + "\x1b\\",
		"OSC_C1_payload": "\x1b]2;" + strings.Repeat("x", 3000) + "\x9d" + strings.Repeat("x", 3000) + "\x07",
	} {
		t.Run(name, func(t *testing.T) {
			e := newEngine(t, 20, 4)
			_, err := e.Feed([]byte(input))
			s := e.Snapshot()
			if !errors.Is(err, ErrLimit) || s.Fault == "" {
				t.Errorf("accepted %d-byte sequence: error=%v fault=%q", len(input), err, s.Fault)
			}
		})
	}
}

func TestCheckerReplyOverflowFaults(t *testing.T) {
	e := newEngine(t, 20, 4)
	rev := e.Snapshot().Revision
	replies, err := e.Feed([]byte(strings.Repeat("\x1b[c", MaxChunk/3)))
	s := e.Snapshot()
	if !errors.Is(err, ErrLimit) || s.Fault == "" || len(replies) > MaxReplies || s.Revision <= rev {
		t.Fatalf("overflow: replies=%d error=%v fault=%q revision=%d", len(replies), err, s.Fault, s.Revision)
	}
	if _, err = e.Resize(21, 4); !errors.Is(err, ErrLimit) {
		t.Fatalf("resize after overflow: %v", err)
	}
}

func TestCheckerConcurrentActualResizeAndClose(t *testing.T) {
	for round := 0; round < 20; round++ {
		e := newEngine(t, 20, 4)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			for i := 0; i < 30; i++ {
				_, err := e.Feed([]byte("text\x1b[6n"))
				if err != nil && !errors.Is(err, io.ErrClosedPipe) {
					t.Error(err)
				}
			}
		})
		wg.Go(func() {
			<-start
			for i := 0; i < 30; i++ {
				_, err := e.Resize(20+i%3, 4+i%2)
				if err != nil && !errors.Is(err, io.ErrClosedPipe) {
					t.Error(err)
				}
			}
		})
		wg.Go(func() {
			<-start
			for i := 0; i < 30; i++ {
				s := e.Snapshot()
				if len(s.Lines) != s.Rows || len(s.ANSI) != s.Rows || s.Cursor.X < 0 || s.Cursor.X >= s.Cols || s.Cursor.Y < 0 || s.Cursor.Y >= s.Rows {
					t.Errorf("inconsistent frame: %+v", s)
				}
			}
		})
		wg.Go(func() {
			<-start
			if err := e.Close(); err != nil {
				t.Error(err)
			}
		})
		close(start)
		wg.Wait()
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	}
}

func TestCheckerRevisionAndImmutability(t *testing.T) {
	e := newEngine(t, 20, 4)
	feed(t, e, "\x1b[31mred\x1b[0m")
	a := e.Snapshot()
	b := e.Snapshot()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("observations changed state")
	}
	_, err := e.Resize(20, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, e.Snapshot()) {
		t.Fatal("no-op resize changed state")
	}
	a.ANSI[0] = "mutated"
	a.Lines[0] = "mutated"
	if !reflect.DeepEqual(b, e.Snapshot()) {
		t.Fatal("returned slices alias state")
	}
	reply := feed(t, e, "\x1b[6n")
	copy(reply, []byte("xxx"))
	if got := string(feed(t, e, "\x1b[6n")); got != "\x1b[1;4R" {
		t.Fatalf("reply alias %q", got)
	}
}
