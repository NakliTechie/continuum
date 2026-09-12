package server

import (
	"encoding/base64"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/holder"
	"github.com/NakliTechie/continuum/internal/pty"
	"golang.org/x/sys/unix"
)

// startHeld launches cmd under a block holder so the process outlives this
// daemon; the returned session drives the holder's PTY master directly and
// reads the child's output over the holder's stream.
func (s *Server) startHeld(id, agent string, cmd *exec.Cmd, opts *pty.TerminalOptions) (*pty.Session, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	spec := holder.Spec{Block: id, Agent: agent, Path: cmd.Path, Argv: cmd.Args, Dir: cmd.Dir, Env: cmd.Env}
	if opts != nil {
		spec.Cols, spec.Rows = opts.Cols, opts.Rows
	}
	a, err := holder.Launch(s.cfg.HoldersState, exe, spec)
	if err != nil {
		return nil, err
	}
	started, perr := time.Parse(time.RFC3339Nano, a.Hello.StartedAt)
	if perr != nil {
		started = time.Now()
	}
	sess, err := pty.Held(id, agent, a.Ptmx, a.Hello.PID, started, a.Output, a.Wait, opts, s.cfg.CaptureDir)
	if err != nil {
		a.Close()
		a.Ptmx.Close()
		return nil, err
	}
	return sess, nil
}

// AdoptHolders re-attaches every block a previous daemon left under a holder
// and settles the ones whose child exited meanwhile. Called once at start,
// before any client can connect; each outcome is journaled explicitly.
func (s *Server) AdoptHolders() {
	if s.cfg.HoldersState == "" {
		return
	}
	records, err := holder.Scan(s.cfg.HoldersState)
	if err != nil {
		log.Printf("holders: %v", err)
		return
	}
	for _, r := range records {
		switch {
		case r.Socket == "" && r.Exit != nil:
			s.settleExited(r)
		case r.Socket != "":
			s.adoptHeld(r)
		}
	}
}

// blockState reads a block's recorded lifecycle state, or "" when unknown.
func (s *Server) blockState(id string) string {
	if m := s.modern; m != nil {
		if blocks, err := m.Store.Blocks(); err == nil {
			for _, b := range blocks {
				if b.ID == id {
					return b.State
				}
			}
		}
	}
	return ""
}

// resumeOffset is how many output bytes the journal already holds for a block;
// the holder resumes streaming from exactly here so replay never duplicates.
func (s *Server) resumeOffset(block string) int {
	if m := s.modern; m != nil {
		if n, err := m.Store.OutputBytes(block); err == nil {
			return n
		}
	}
	return 0
}

func (s *Server) adoptHeld(r holder.Record) {
	resume := s.resumeOffset(r.Block)
	a, err := holder.Adopt(r.Socket, resume)
	if err != nil {
		s.record(r.Block, "holder_lost", map[string]any{"message": err.Error()})
		holder.Forget(s.cfg.HoldersState, r.Block)
		log.Printf("holder: %s unreachable (%v); block stays interrupted", r.Block, err)
		return
	}
	var opts *pty.TerminalOptions
	if a.Hello.Cols > 0 && a.Hello.Rows > 0 {
		cols, rows := a.Hello.Cols, a.Hello.Rows
		if ws, err := unix.IoctlGetWinsize(int(a.Ptmx.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 0 && ws.Row > 0 {
			cols, rows = int(ws.Col), int(ws.Row)
		}
		opts = &pty.TerminalOptions{Cols: cols, Rows: rows}
	}
	started, perr := time.Parse(time.RFC3339Nano, a.Hello.StartedAt)
	if perr != nil {
		started = time.Now()
	}
	sess, err := pty.Held(r.Block, a.Hello.Agent, a.Ptmx, a.Hello.PID, started, a.Output, a.Wait, opts, s.cfg.CaptureDir)
	if err != nil {
		a.Close()
		a.Ptmx.Close()
		s.record(r.Block, "holder_lost", map[string]any{"message": err.Error()})
		log.Printf("holder: %s adopt failed: %v", r.Block, err)
		return
	}
	token, err := config.GenerateToken()
	if err != nil {
		a.Close()
		a.Ptmx.Close()
		return
	}
	dropped := 0
	if a.Hello.RingStart > resume {
		dropped = a.Hello.RingStart - resume // downtime output past the 256 KiB ring
	}
	if m := s.modern; m != nil {
		if err := m.Store.Reactivate(r.Block, a.Hello.PID); err != nil {
			m.degraded.Store(true)
		}
	}
	// The downtime backlog (a.Gap bytes) streams as ordinary output right after
	// this event; the event says how much of the coming output no daemon saw.
	s.record(r.Block, "adopted", map[string]any{"held": true, "pid": a.Hello.PID, "gap_bytes": a.Gap, "dropped_bytes": dropped, "screen": opts != nil})
	s.mu.Lock()
	s.sessions[r.Block] = &sessionEntry{token: token, sess: sess, agent: a.Hello.Agent, startedAt: started, pid: a.Hello.PID}
	s.mu.Unlock()
	log.Printf("adopted %s (agent=%s pid=%d gap=%d dropped=%d)", r.Block, a.Hello.Agent, a.Hello.PID, a.Gap, dropped)
	s.runSession(r.Block, sess)
}

func (s *Server) settleExited(r holder.Record) {
	// Idempotent: a daemon that journaled `exited` before dying (its socket write
	// to the holder having landed) leaves a stale exit file too; settling again
	// must not re-emit its tail or a second exit event.
	if s.blockState(r.Block) == "exited" {
		holder.Forget(s.cfg.HoldersState, r.Block)
		return
	}
	resume := s.resumeOffset(r.Block)
	ring, _ := base64.StdEncoding.DecodeString(r.Exit.Ring)
	dropped := 0
	if r.Exit.RingStart > resume {
		dropped = r.Exit.RingStart - resume
	}
	// Emit the unobserved tail nobody saw, then the adoption and exit records.
	if from := resume - r.Exit.RingStart; from < len(ring) {
		if from < 0 {
			from = 0
		}
		if tail := ring[from:]; len(tail) > 0 {
			s.recordOutput(r.Block, tail, true)
		}
	}
	gap := r.Exit.Produced - resume
	if gap < 0 {
		gap = 0
	}
	s.record(r.Block, "adopted", map[string]any{"held": false, "gap_bytes": gap, "dropped_bytes": dropped, "exited_at": r.Exit.At})
	code := r.Exit.Code
	s.record(r.Block, "exited", map[string]any{"exit_code": &code})
	holder.Forget(s.cfg.HoldersState, r.Block)
	log.Printf("holder: %s exited (code=%d) while no daemon was attached", r.Block, r.Exit.Code)
}
