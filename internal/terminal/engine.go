// Package terminal owns an experimental, headless terminal screen. It never
// owns a process or writes to a user's terminal. Replies belong to the PTY host.
package terminal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
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
	Epoch      string   `json:"epoch"`
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
	epoch    string
	revision uint64
	// cached is the last frame built, reused while the revision holds: a
	// viewer polling an idle screen must not rebuild 19,200 cells under the
	// same lock the output pump needs.
	cached    *Snapshot
	cachedRev uint64
	// held is the frame published while the application has synchronized
	// output (DEC private mode 2026) set: viewers keep seeing the screen as it
	// was before the update began, never a half-drawn one. holdUntil bounds
	// it so an unclosed pair cannot freeze viewers.
	held          *Snapshot
	holdUntil     time.Time
	cursor        Cursor
	modes         Modes
	modeSet       map[ansi.Mode]bool
	guard         sequenceGuard
	pending       []byte
	pendingAt     time.Time
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
	var epochBytes [16]byte
	if _, err := rand.Read(epochBytes[:]); err != nil {
		return nil, fmt.Errorf("terminal epoch: %w", err)
	}
	e := &emulator{vt: vt.NewEmulator(cols, rows), epoch: hex.EncodeToString(epochBytes[:]), cursor: Cursor{Visible: true, Blink: true}, modeSet: map[ansi.Mode]bool{}, done: make(chan struct{})}
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
		EnableMode: func(m ansi.Mode) {
			e.modeSet[m] = true
			e.updateModes()
			if m == ansi.ModeSynchronizedOutput && e.held == nil {
				e.beginHold()
			}
		},
		DisableMode: func(m ansi.Mode) {
			delete(e.modeSet, m)
			e.updateModes()
			if m == ansi.ModeSynchronizedOutput {
				e.held = nil
			}
		},
	})
	// The library's pipe is synchronous. Drain it even with zero viewers. Closing
	// the pipe and joining this reader BEFORE vt.Close avoids its closed-flag race.
	go e.drain()
	return e, nil
}

// SyncHoldCeiling bounds how long a synchronized-output hold may keep viewers
// on the previous frame: long enough for a real TUI's redraw, short enough
// that an application which sets 2026 and stalls does not freeze its viewers.
const SyncHoldCeiling = 150 * time.Millisecond

// GraphemeHoldCeiling lets adjacent PTY reads form one Unicode grapheme. A PTY
// read boundary has no text semantics, but the pinned emulator finalizes cells
// per Write. Holding only the trailing printable cluster keeps the added latency
// below one interactive frame while controls, queries and completed prefixes
// continue immediately.
const GraphemeHoldCeiling = 30 * time.Millisecond

// beginHold publishes the current screen for the duration of a synchronized
// update. It runs inside a Feed under e.mu (the mode callback fires from
// vt.Write), so it renders without locking.
func (e *emulator) beginHold() {
	frame := e.buildLocked()
	e.held = &frame
	e.holdUntil = time.Now().Add(SyncHoldCeiling)
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
	if len(e.pending) > 0 && time.Since(e.pendingAt) >= GraphemeHoldCeiling {
		if err := e.flushPendingLocked(); err != nil {
			return nil, err
		}
	}
	b = append(e.pending, b...)
	e.pending = nil
	// Keep the final printable grapheme for the next read. If another grapheme
	// or any control follows, the earlier cluster is complete and is written.
	for rest, offset := b, 0; len(rest) > 0; {
		cluster, width := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
		if len(cluster) == 0 {
			break
		}
		offset += len(cluster)
		rest = rest[len(cluster):]
		if len(rest) == 0 && width > 0 && e.guard.endsInText() {
			e.pending = append(e.pending[:0], cluster...)
			e.pendingAt = time.Now()
			b = b[:offset-len(cluster)]
		}
	}
	if len(e.pending) > 256 {
		e.fault = fmt.Errorf("%w: grapheme", ErrLimit)
		e.revision++
		return nil, e.fault
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

func (e *emulator) flushPendingLocked() error {
	if len(e.pending) == 0 {
		return nil
	}
	pending := e.pending
	e.pending = nil
	if _, err := e.vt.Write(pending); err != nil {
		e.fault = err
		return err
	}
	e.revision++
	return nil
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
	if err := e.flushPendingLocked(); err != nil {
		return nil, err
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
	if len(e.pending) > 0 && time.Since(e.pendingAt) >= GraphemeHoldCeiling {
		_ = e.flushPendingLocked()
	}
	if e.held != nil {
		if time.Now().Before(e.holdUntil) {
			return copyFrame(e.held, e.fault)
		}
		e.held = nil // the application never released the hold; show what there is
	}
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
	s := e.buildLocked()
	frame := s
	frame.Lines = append([]string(nil), s.Lines...)
	frame.ANSI = append([]string(nil), s.ANSI...)
	e.cached, e.cachedRev = &frame, e.revision
	return s
}

// copyFrame hands a caller its own frame: row slices copied, fault current.
func copyFrame(f *Snapshot, fault error) Snapshot {
	s := *f
	s.Lines = append([]string(nil), f.Lines...)
	s.ANSI = append([]string(nil), f.ANSI...)
	if fault != nil {
		s.Fault = fault.Error()
	}
	return s
}

// buildLocked renders the visible screen; the caller holds e.mu.
func (e *emulator) buildLocked() Snapshot {
	s := Snapshot{Engine: Name, Epoch: e.epoch, Revision: e.revision, Cols: e.vt.Width(), Rows: e.vt.Height(), Alternate: e.vt.IsAltScreen(), Cursor: e.cursor, Modes: e.modes, Scrollback: e.vt.ScrollbackLen()}
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
	return s
}
func (e *emulator) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	flushErr := e.flushPendingLocked()
	e.closed = true
	_ = e.input.Close()
	<-e.done
	closeErr := e.vt.Close()
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

// sequenceGuard uses the SAME pinned parser transitions as the emulator.
// It retains one data byte only, counts the original sequence bytes, and limits
// numeric accumulation on ParamAction. Ignored/executed controls cannot split a
// number, nor can BEL terminate DCS/APC or embedded C1 restart an OSC payload.
type sequenceGuard struct {
	parser         *ansi.Parser
	length, number int
	lastAction     parser.Action
}

func (g *sequenceGuard) endsInText() bool {
	return g.lastAction == parser.PrintAction || g.parser != nil && g.parser.State() == parser.Utf8State
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
		g.lastAction = action
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
