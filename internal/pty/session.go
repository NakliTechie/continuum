// Package pty manages a single agent attached to a pseudo-terminal.
package pty

import (
	"fmt"
	"github.com/NakliTechie/continuum/internal/config"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/NakliTechie/continuum/internal/terminal"
	creackpty "github.com/creack/pty"
)

// SessionsDir is ~/.menagerie/sessions, where raw PTY byte streams are captured.
func SessionsDir() (string, error) {
	dir, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sessions"), nil
}

// Session is a running agent attached to a PTY.
type Session struct {
	ID        string
	Agent     string
	StartedAt time.Time
	PID       int

	writeMu       sync.Mutex      // makes application input and generated replies indivisible writes
	termMu        sync.Mutex      // serializes output parsing, PTY resize and snapshots
	term          terminal.Engine // immutable after start; nil preserves legacy behavior
	termFault     string
	killRequested atomic.Bool

	ptmx   *os.File
	readFn func([]byte) (int, error) // output source: the PTY, or a holder's stream
	wait   func() int                // reaps the leader, or reads the holder's exit report
	cap    *os.File                  // append-only capture: ~/.menagerie/sessions/<id>.pty
	replay int                       // leading holder bytes feed term only; already recorded

	mu     sync.Mutex
	seq    int
	tail   []byte // recent output, capped, replayed when a client (re)attaches
	closed bool
}

// maxTail bounds the in-memory replay buffer per session.
const maxTail = 256 * 1024

// Start spawns cmd attached to a new PTY and opens the capture file
// (best-effort — capture failure does not fail the spawn).
func Start(id, agent string, cmd *exec.Cmd, capture ...*string) (*Session, error) {
	return start(id, agent, cmd, nil, capture...)
}

// TerminalOptions enables the experimental server-owned screen only at spawn.
// Existing legacy sessions cannot be converted from a truncated byte tail.
type TerminalOptions struct{ Cols, Rows int }

func StartTerminal(id, agent string, cmd *exec.Cmd, opts TerminalOptions, capture ...*string) (*Session, error) {
	return start(id, agent, cmd, &opts, capture...)
}
func start(id, agent string, cmd *exec.Cmd, opts *TerminalOptions, capture ...*string) (*Session, error) {
	var engine terminal.Engine
	var err error
	if opts != nil {
		engine, err = terminal.New(opts.Cols, opts.Rows)
		if err != nil {
			return nil, err
		}
	}
	var ptmx *os.File
	if opts == nil {
		ptmx, err = creackpty.Start(cmd)
	} else {
		ptmx, err = creackpty.StartWithSize(cmd, &creackpty.Winsize{Cols: uint16(opts.Cols), Rows: uint16(opts.Rows)})
	}
	if err != nil {
		if engine != nil {
			engine.Close()
		}
		return nil, err
	}
	_ = SetUTF8(ptmx) // best effort; the child still runs without it
	s := &Session{
		readFn:    nil, // set below to readPTY once s exists
		term:      engine,
		ID:        id,
		Agent:     agent,
		StartedAt: time.Now(),
		PID:       cmd.Process.Pid,
		ptmx:      ptmx,
		wait: func() int {
			if err := cmd.Wait(); err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					return ee.ExitCode()
				}
				return -1
			}
			return 0
		},
	}
	s.readFn = s.readPTY
	s.openCapture(capture)
	return s, nil
}

// Held wraps a PTY master received from a block holder: the child is the
// holder's, so wait reads the holder's exit report instead of reaping. gap is
// output the holder drained while no daemon was attached; it seeds the replay
// tail and, for a server-owned screen, the rebuilt frame.
func Held(id, agent string, ptmx *os.File, pid int, startedAt time.Time, output io.Reader, wait func() int, replay int, opts *TerminalOptions, capture ...*string) (*Session, error) {
	var engine terminal.Engine
	if opts != nil {
		var err error
		if engine, err = terminal.New(opts.Cols, opts.Rows); err != nil {
			return nil, err
		}
	}
	s := &Session{term: engine, ID: id, Agent: agent, StartedAt: startedAt, PID: pid, ptmx: ptmx, wait: wait, replay: replay}
	// The daemon never reads the master (the holder is the sole reader); output
	// arrives over the holder stream and is journaled here as it always was.
	s.readFn = func(b []byte) (int, error) { return output.Read(b) }
	s.openCapture(capture)
	return s, nil
}

func (s *Session) openCapture(capture []*string) {
	dir, capErr := SessionsDir()
	if len(capture) > 0 && capture[0] != nil {
		dir = *capture[0]
		capErr = nil
	}
	if capErr == nil && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err == nil {
			s.cap, _ = os.OpenFile(filepath.Join(dir, s.ID+".pty"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		}
	}
}

// Run pumps PTY output to onData (raw bytes + a monotonic per-session seq) and
// to the capture file, until the PTY closes; then it reaps the process and
// calls onExit with the exit code. Blocks — run it in a goroutine.
func (s *Session) Run(onData func(seq int, b []byte), onExit func(code int)) {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if s.term != nil {
				s.termMu.Lock()
				if s.termFault == "" {
					reply, err := s.term.Feed(chunk)
					if err == nil && len(reply) > 0 {
						err = s.Write(reply)
					}
					if err != nil {
						s.termFault = err.Error()
					}
				}
				s.termMu.Unlock()
			}
			// A holder adoption can prepend retained output so a new volatile
			// terminal engine reconstructs its screen. Drop the committed prefix
			// only after feeding the engine: events and capture remain exactly-once.
			recorded := chunk
			if s.replay > 0 {
				skip := min(s.replay, len(recorded))
				s.replay -= skip
				recorded = recorded[skip:]
			}
			if len(recorded) > 0 {
				s.mu.Lock()
				seq := s.seq
				s.seq++
				s.tail = append(s.tail, recorded...)
				if len(s.tail) > maxTail {
					s.tail = s.tail[len(s.tail)-maxTail:]
				}
				s.mu.Unlock()
				if s.cap != nil {
					_, _ = s.cap.Write(recorded)
				}
				onData(seq, recorded)
			}
		}
		if err != nil {
			break // EOF when the child exits, or PTY closed
		}
	}
	code := s.wait()
	// Wait reaps the leader, but SIGKILL delivery and orphan reaping for its
	// process group can lag it. Bound that drain before announcing explicit
	// group shutdown; normal child exit incurs no extra wait.
	if s.killRequested.Load() {
		deadline := time.Now().Add(250 * time.Millisecond)
		for syscall.Kill(-s.PID, 0) == nil && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	s.closeFiles()
	onExit(code)
}

// Buffer returns a copy of the recent output, for replay when a client attaches.
func (s *Session) Buffer() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]byte, len(s.tail))
	copy(out, s.tail)
	return out
}

// Write sends input bytes to the PTY.
func (s *Session) Write(b []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	deadline := time.Now().Add(time.Second)
	for len(b) > 0 {
		n := 0
		var ioErr error
		raw, err := s.ptmx.SyscallConn()
		if err != nil {
			return err
		}
		err = raw.Control(func(fd uintptr) {
			if ioErr = unix.SetNonblock(int(fd), true); ioErr != nil {
				return
			}
			n, ioErr = unix.Write(int(fd), b)
		})
		if err != nil {
			return err
		}
		if n > 0 {
			b = b[n:]
		}
		if ioErr != nil && ioErr != unix.EAGAIN && ioErr != unix.EINTR {
			return ioErr
		}
		if len(b) > 0 {
			if time.Now().After(deadline) {
				return fmt.Errorf("PTY input deadline exceeded")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	return nil
}

// read pulls the next output bytes from the session's source.
func (s *Session) read(b []byte) (int, error) { return s.readFn(b) }

// readPTY uses a short poll while holding the file reference. Descriptor closure
// cannot race reuse, and no blocking read prevents the daemon from stopping.
func (s *Session) readPTY(b []byte) (int, error) {
	raw, err := s.ptmx.SyscallConn()
	if err != nil {
		return 0, err
	}
	n := 0
	var ioErr error
	err = raw.Control(func(fd uintptr) {
		if ioErr = unix.SetNonblock(int(fd), true); ioErr != nil {
			return
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		ready, e := unix.Poll(fds, 100)
		if e == unix.EINTR {
			return
		}
		if e != nil {
			ioErr = e
			return
		}
		if ready == 0 {
			return
		}
		n, ioErr = unix.Read(int(fd), b)
		if n == 0 && ioErr == nil {
			ioErr = io.EOF
		}
		if ioErr == unix.EAGAIN || ioErr == unix.EINTR {
			n = 0
			ioErr = nil
		}
	})
	if err != nil {
		return 0, err
	}
	return n, ioErr
}

// Resize never changes descriptor blocking mode through os.File.Fd.
// MaxDimension bounds any requested terminal size; the kernel takes uint16 and
// a wrapped zero would hand the child a zero-column terminal.
const MaxDimension = 1000

func (s *Session) Resize(cols, rows int) error {
	if cols < 1 || cols > MaxDimension || rows < 1 || rows > MaxDimension {
		return fmt.Errorf("resize %dx%d: dimensions must be within 1..%d", cols, rows, MaxDimension)
	}
	s.termMu.Lock()
	defer s.termMu.Unlock()
	if s.term != nil {
		if !terminal.ValidSize(cols, rows) {
			return fmt.Errorf("%w: resize", terminal.ErrLimit)
		}
		if s.termFault != "" {
			return fmt.Errorf("terminal fault: %s", s.termFault)
		}
	}
	if err := s.resizePTY(cols, rows); err != nil {
		return err
	}
	if s.term != nil {
		reply, err := s.term.Resize(cols, rows)
		if err == nil && len(reply) > 0 {
			err = s.Write(reply)
		}
		if err != nil {
			s.termFault = err.Error()
		}
		return err
	}
	return nil
}
func (s *Session) resizePTY(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	raw, err := s.ptmx.SyscallConn()
	if err != nil {
		return err
	}
	var ioErr error
	err = raw.Control(func(fd uintptr) {
		ioErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(cols), Row: uint16(rows)})
	})
	if err != nil {
		return err
	}
	return ioErr
}

// Kill sends SIGKILL to the managed process group.
func (s *Session) Kill() {
	s.killRequested.Store(true)
	if s.PID > 0 {
		_ = syscall.Kill(-s.PID, syscall.SIGKILL)
	}
}

// Interrupt sends SIGINT to the agent process.
func (s *Session) Interrupt() {
	if s.PID > 0 {
		_ = syscall.Kill(s.PID, syscall.SIGINT)
	}
}

func (s *Session) closeFiles() {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.term != nil {
		_ = s.term.Close()
	}
	_ = s.ptmx.Close()
	if s.cap != nil {
		_ = s.cap.Close()
	}
}

// HasTerminal is immutable after construction.
func (s *Session) HasTerminal() bool { return s.term != nil }
func (s *Session) TerminalSnapshot() (terminal.Snapshot, bool) {
	if s.term == nil {
		return terminal.Snapshot{}, false
	}
	s.termMu.Lock()
	defer s.termMu.Unlock()
	frame := s.term.Snapshot()
	if s.termFault != "" {
		frame.Fault = s.termFault
	}
	return frame, true
}
func (s *Session) TerminalFault() string {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	return s.termFault
}
