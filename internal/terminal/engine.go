// Package terminal owns an experimental, headless terminal screen. It never
// owns a process or writes to a user's terminal. Replies belong to the PTY host.
package terminal

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
	"github.com/charmbracelet/x/vt"
)

const (
	Name            = "charm-vt-experimental-1"
	MaxCols         = 240
	MaxRows         = 100
	MaxCells        = 19200
	ScrollbackLines = 200
	MaxChunk        = 32 << 10
	MaxReplies      = 64 << 10
	MaxSequence     = 4096
)

var ErrLimit = errors.New("experimental terminal limit exceeded")

// Engine hides the emulator dependency from process and protocol code.
// Feed and Resize return terminal-generated replies, never viewer input.
type Engine interface {
	Feed([]byte) ([]byte, error)
	Resize(int, int) ([]byte, error)
	Snapshot() Snapshot
	Close() error
}

type Cursor struct {
	X       int  `json:"x"`
	Y       int  `json:"y"`
	Visible bool `json:"visible"`
	Style   int  `json:"style"`
	Blink   bool `json:"blink"`
}
type Modes struct {
	CursorKeys     bool `json:"cursor_keys"`
	BracketedPaste bool `json:"bracketed_paste"`
	Mouse          bool `json:"mouse"`
	Focus          bool `json:"focus"`
}

// Snapshot is a complete visible frame at Revision, not serialized parser state
// or a raw-replay starting point. ANSI contains generated SGR and text only;
// rows are separate to avoid inheriting application margins or cursor modes.
type Snapshot struct {
	Engine     string   `json:"engine"`
	Revision   uint64   `json:"revision"`
	Cols       int      `json:"cols"`
	Rows       int      `json:"rows"`
	Alternate  bool     `json:"alternate"`
	Cursor     Cursor   `json:"cursor"`
	Modes      Modes    `json:"modes"`
	Lines      []string `json:"lines"`
	ANSI       []string `json:"ansi"`
	Scrollback int      `json:"scrollback_lines"`
	Fault      string   `json:"fault,omitempty"`
}

type emulator struct {
	mu       sync.Mutex
	vt       *vt.Emulator
	revision uint64
	// cached is the last frame built, reused while the revision holds: a
	// viewer polling an idle screen must not rebuild 19,200 cells under the
	// same lock the output pump needs.
	cached        *Snapshot
	cachedRev     uint64
	cursor        Cursor
	modes         Modes
	modeSet       map[ansi.Mode]bool
	guard         sequenceGuard
	fault         error
	closed        bool
	input         *io.PipeWriter
	done          chan struct{}
	replyMu       sync.Mutex
	replies       []byte
	replyOverflow bool
}

func ValidSize(cols, rows int) bool {
	return cols > 0 && cols <= MaxCols && rows > 0 && rows <= MaxRows && cols*rows <= MaxCells
}
func New(cols, rows int) (Engine, error) {
	if !ValidSize(cols, rows) {
		return nil, fmt.Errorf("%w: size must fit 240 columns, 100 rows and 19200 cells", ErrLimit)
	}
	e := &emulator{vt: vt.NewEmulator(cols, rows), cursor: Cursor{Visible: true, Blink: true}, modeSet: map[ansi.Mode]bool{}, done: make(chan struct{})}
	e.input = e.vt.InputPipe().(*io.PipeWriter)
	e.vt.SetScrollbackSize(ScrollbackLines)
	// Upstream uses a DEC-private status report for ANSI DSR 5. Reply with the
	// standard non-private CSI 0 n; let the engine handle cursor reports.
	e.vt.RegisterCsiHandler('n', func(params ansi.Params) bool {
		n, _, ok := params.Param(0, 0)
		if !ok || n != 5 {
			return false
		}
		_, _ = io.WriteString(e.input, "\x1b[0n")
		return true
	})
	e.vt.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(v bool) { e.cursor.Visible = v },
		CursorStyle:      func(s vt.CursorStyle, b bool) { e.cursor.Style = int(s); e.cursor.Blink = b },
		EnableMode:       func(m ansi.Mode) { e.modeSet[m] = true; e.updateModes() },
		DisableMode:      func(m ansi.Mode) { delete(e.modeSet, m); e.updateModes() },
	})
	// The library's pipe is synchronous. Drain it even with zero viewers. Closing
	// the pipe and joining this reader BEFORE vt.Close avoids its closed-flag race.
	go e.drain()
	return e, nil
}
func (e *emulator) updateModes() {
	e.modes = Modes{CursorKeys: e.modeSet[ansi.ModeCursorKeys], BracketedPaste: e.modeSet[ansi.ModeBracketedPaste], Focus: e.modeSet[ansi.ModeFocusEvent]}
	for _, m := range []ansi.Mode{ansi.ModeMouseX10, ansi.ModeMouseNormal, ansi.ModeMouseHighlight, ansi.ModeMouseButtonEvent, ansi.ModeMouseAnyEvent} {
		e.modes.Mouse = e.modes.Mouse || e.modeSet[m]
	}
}
func (e *emulator) drain() {
	defer close(e.done)
	b := make([]byte, 4096)
	for {
		n, err := e.vt.Read(b)
		e.replyMu.Lock()
		if len(e.replies)+n <= MaxReplies {
			e.replies = append(e.replies, b[:n]...)
		} else {
			e.replyOverflow = true
		}
		e.replyMu.Unlock()
		if err != nil {
			return
		}
	}
}
func (e *emulator) takeReplies() ([]byte, error) {
	// An empty pipe write is a barrier: the reader has stored every earlier
	// reply before accepting this write. No magic delimiter can collide with data.
	if _, err := e.input.Write(nil); err != nil {
		return nil, err
	}
	e.replyMu.Lock()
	defer e.replyMu.Unlock()
	b := e.replies
	e.replies = nil
	if e.replyOverflow {
		e.fault = fmt.Errorf("%w: query replies", ErrLimit)
		return nil, e.fault
	}
	return b, nil
}
func (e *emulator) Feed(b []byte) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, io.ErrClosedPipe
	}
	if e.fault != nil {
		return nil, e.fault
	}
	if len(b) > MaxChunk {
		e.fault = fmt.Errorf("%w: feed chunk", ErrLimit)
		e.revision++
		return nil, e.fault
	}
	// Validate before parsing any of this chunk. A rejected stream becomes
	// explicitly faulted; it must never be presented as an up-to-date screen.
	if err := e.guard.check(b); err != nil {
		e.fault = err
		e.revision++
		return nil, err
	}
	// The dependency flushes graphemes per Write. Bound each potential cell's
	// retained content independently of chunk length, including combining marks.
	for rest := b; len(rest) > 0; {
		cluster, _ := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		if len(cluster) > 256 {
			e.fault = fmt.Errorf("%w: grapheme", ErrLimit)
			e.revision++
			return nil, e.fault
		}
		if len(cluster) == 0 {
			break
		}
		rest = rest[len(cluster):]
	}
	if _, err := e.vt.Write(b); err != nil {
		e.fault = err
		return nil, err
	}
	if len(b) > 0 {
		e.revision++
	}
	return e.takeReplies()
}
func (e *emulator) Resize(cols, rows int) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, io.ErrClosedPipe
	}
	if e.fault != nil {
		return nil, e.fault
	}
	if !ValidSize(cols, rows) {
		return nil, fmt.Errorf("%w: resize", ErrLimit)
	}
	if cols == e.vt.Width() && rows == e.vt.Height() {
		return nil, nil
	}
	e.vt.Resize(cols, rows)
	e.revision++
	return e.takeReplies()
}
func (e *emulator) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cached != nil && e.cachedRev == e.revision {
		// Callers own their frame: the row slices are copied (string headers
		// only), the cells are not rebuilt.
		s := *e.cached
		s.Lines = append([]string(nil), e.cached.Lines...)
		s.ANSI = append([]string(nil), e.cached.ANSI...)
		if e.fault != nil {
			s.Fault = e.fault.Error()
		}
		return s
	}
	s := Snapshot{Engine: Name, Revision: e.revision, Cols: e.vt.Width(), Rows: e.vt.Height(), Alternate: e.vt.IsAltScreen(), Cursor: e.cursor, Modes: e.modes, Scrollback: e.vt.ScrollbackLen()}
	pos := e.vt.CursorPosition()
	s.Cursor.X = pos.X
	s.Cursor.Y = pos.Y
	if e.fault != nil {
		s.Fault = e.fault.Error()
	}
	s.Lines = make([]string, s.Rows)
	s.ANSI = make([]string, s.Rows)
	for y := 0; y < s.Rows; y++ {
		var plain, styled strings.Builder
		last := uv.Style{}
		for x := 0; x < s.Cols; {
			c := e.vt.CellAt(x, y)
			if c == nil || c.IsZero() {
				c = &uv.EmptyCell
			}
			content := strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return '\uFFFD'
				}
				return r
			}, c.Content)
			if !last.Equal(&c.Style) {
				styled.WriteString("\x1b[0m")
				styled.WriteString(c.Style.String())
				last = c.Style
			}
			plain.WriteString(content)
			styled.WriteString(content)
			x += max(1, c.Width)
		}
		styled.WriteString("\x1b[0m")
		s.Lines[y] = plain.String()
		s.ANSI[y] = styled.String()
	}
	frame := s
	frame.Lines = append([]string(nil), s.Lines...)
	frame.ANSI = append([]string(nil), s.ANSI...)
	e.cached, e.cachedRev = &frame, e.revision
	return s
}
func (e *emulator) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	_ = e.input.Close()
	<-e.done
	return e.vt.Close()
}

// sequenceGuard uses the SAME pinned parser transitions as the emulator.
// It retains one data byte only, counts the original sequence bytes, and limits
// numeric accumulation on ParamAction. Ignored/executed controls cannot split a
// number, nor can BEL terminate DCS/APC or embedded C1 restart an OSC payload.
type sequenceGuard struct {
	parser         *ansi.Parser
	length, number int
}

func (g *sequenceGuard) check(b []byte) error {
	if g.parser == nil {
		g.parser = ansi.NewParser()
		g.parser.SetDataSize(1)
	}
	inSequence := func(s byte) bool { return s != parser.GroundState && s != parser.Utf8State }
	for _, c := range b {
		before := g.parser.State()
		action := g.parser.Advance(c)
		after := g.parser.State()
		if inSequence(before) {
			g.length++
		} else if inSequence(after) {
			g.length = 1
		} else {
			g.length = 0
		}
		if action == parser.ClearAction {
			g.number = 0
			if c == 0x1b {
				g.length = 1
			}
		}
		if g.length > MaxSequence {
			return fmt.Errorf("%w: escape sequence", ErrLimit)
		}
		if action == parser.ParamAction {
			if c >= '0' && c <= '9' {
				g.number = g.number*10 + int(c-'0')
			} else {
				g.number = 0
			}
			if g.number > 4096 {
				return fmt.Errorf("%w: numeric parameter", ErrLimit)
			}
		}
		if !inSequence(after) {
			g.length = 0
			g.number = 0
		}
	}
	return nil
}
