// Package pty manages a single agent attached to a pseudo-terminal.
package pty

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	creackpty "github.com/creack/pty"
)

// SessionsDir is ~/.menagerie/sessions, where raw PTY byte streams are captured.
func SessionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".menagerie", "sessions"), nil
}

// Session is a running agent attached to a PTY.
type Session struct {
	ID        string
	Agent     string
	StartedAt time.Time
	PID       int

	ptmx *os.File
	cmd  *exec.Cmd
	cap  *os.File // append-only capture: ~/.menagerie/sessions/<id>.pty

	mu     sync.Mutex
	seq    int
	closed bool
}

// Start spawns cmd attached to a new PTY and opens the capture file
// (best-effort — capture failure does not fail the spawn).
func Start(id, agent string, cmd *exec.Cmd) (*Session, error) {
	ptmx, err := creackpty.Start(cmd)
	if err != nil {
		return nil, err
	}
	s := &Session{
		ID:        id,
		Agent:     agent,
		StartedAt: time.Now(),
		PID:       cmd.Process.Pid,
		ptmx:      ptmx,
		cmd:       cmd,
	}
	if dir, err := SessionsDir(); err == nil {
		if err := os.MkdirAll(dir, 0o700); err == nil {
			s.cap, _ = os.OpenFile(filepath.Join(dir, id+".pty"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		}
	}
	return s, nil
}

// Run pumps PTY output to onData (raw bytes + a monotonic per-session seq) and
// to the capture file, until the PTY closes; then it reaps the process and
// calls onExit with the exit code. Blocks — run it in a goroutine.
func (s *Session) Run(onData func(seq int, b []byte), onExit func(code int)) {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.mu.Lock()
			seq := s.seq
			s.seq++
			s.mu.Unlock()
			if s.cap != nil {
				_, _ = s.cap.Write(chunk)
			}
			onData(seq, chunk)
		}
		if err != nil {
			break // EOF when the child exits, or PTY closed
		}
	}
	code := 0
	if err := s.cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	s.closeFiles()
	onExit(code)
}

// Write sends input bytes to the PTY.
func (s *Session) Write(b []byte) error {
	_, err := s.ptmx.Write(b)
	return err
}

// Resize sets the PTY window size.
func (s *Session) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	return creackpty.Setsize(s.ptmx, &creackpty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Kill sends SIGKILL to the agent process.
func (s *Session) Kill() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

// Interrupt sends SIGINT to the agent process.
func (s *Session) Interrupt() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(os.Interrupt)
	}
}

func (s *Session) closeFiles() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	_ = s.ptmx.Close()
	if s.cap != nil {
		_ = s.cap.Close()
	}
}
