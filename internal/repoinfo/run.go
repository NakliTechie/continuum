// Package repoinfo reads repository and GitHub context for a session's
// directory: a bounded, read-only Git snapshot and a few `gh` queries
// (protocol 1.5, capabilities session_git_v1, github_inbox_v1, session_pr_v1).
//
// Every read runs a child process with a deadline, an output cap and a
// sanitized environment. Command stderr is classified into a fixed state and
// never returned, so no credential, configuration or path text from the
// child reaches a client.
package repoinfo

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Result states shared by every read (protocol.md, "Repository context").
const (
	StateOK               = "ok"
	StateNotRepository    = "not_repository"
	StateNoDirectory      = "no_directory"
	StateUnavailable      = "unavailable"
	StateTimeout          = "timeout"
	StateBusy             = "busy"
	StateGHMissing        = "gh_missing"
	StateNotAuthenticated = "not_authenticated"
	StateRateLimited      = "rate_limited"
	StateNoRemote         = "no_remote"
	StateNoPR             = "no_pr"
	StateNotFound         = "not_found"
)

const (
	gitDeadline = 5 * time.Second
	ghDeadline  = 10 * time.Second
	outputCap   = 1 << 20 // stdout bytes kept; more marks the read truncated
	stderrCap   = 8 << 10 // stderr bytes kept for classification only
)

// Reader runs the commands. Zero value is usable; tests set GitPath/GHPath to
// fakes and Env to a private environment.
type Reader struct {
	GitPath string   // default: "git" on PATH
	GHPath  string   // default: "gh" on PATH, then Homebrew locations
	Env     []string // default: os.Environ()
	sem     chan struct{}

	inboxMu   sync.Mutex
	inboxAt   time.Time
	inboxLast Inbox
}

// inboxMinInterval answers a repeated inbox refresh from the last result, so
// one client cannot spend the login's 30-a-minute search allowance, which
// agents on the same machine share.
const inboxMinInterval = 10 * time.Second

// New returns a Reader that allows at most n reads in flight; more answer busy.
func New(n int) *Reader {
	if n < 1 {
		n = 1
	}
	return &Reader{sem: make(chan struct{}, n)}
}

func (r *Reader) acquire() bool {
	if r.sem == nil {
		return true
	}
	select {
	case r.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (r *Reader) release() {
	if r.sem != nil {
		<-r.sem
	}
}

// capWriter keeps the first max bytes and records whether more arrived.
type capWriter struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
			w.overflow = true
		} else {
			w.buf.Write(p)
		}
	} else if len(p) > 0 {
		w.overflow = true
	}
	return len(p), nil
}

type runResult struct {
	stdout   []byte
	stderr   string
	code     int // -1 when the process did not exit normally
	timedOut bool
	overflow bool
	startErr error
}

func (r *Reader) run(ctx context.Context, deadline time.Duration, dir string, env []string, name string, args ...string) runResult {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = nil
	out := &capWriter{max: outputCap}
	errw := &capWriter{max: stderrCap}
	cmd.Stdout = out
	cmd.Stderr = errw
	cmd.WaitDelay = time.Second
	// Run the child in its own process group and kill the whole group on
	// timeout, so a filter started by git or a git started by gh cannot
	// outlive the read.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if err := cmd.Start(); err != nil {
		return runResult{code: -1, startErr: err}
	}
	err := cmd.Wait()
	res := runResult{stdout: out.buf.Bytes(), stderr: errw.buf.String(), overflow: out.overflow}
	if ctx.Err() == context.DeadlineExceeded {
		res.timedOut = true
		res.code = -1
		return res
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
		res.code = 0
	case errors.As(err, &exit):
		res.code = exit.ExitCode()
	case errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil:
		// The child exited but a descendant still held its pipes; its own
		// exit status is the answer.
		res.code = cmd.ProcessState.ExitCode()
	default:
		res.code = -1
	}
	return res
}

func (r *Reader) baseEnv() []string {
	if r.Env != nil {
		return append([]string{}, r.Env...)
	}
	return os.Environ()
}

// withoutPrefix drops every variable whose name starts with one of prefixes.
func withoutPrefix(env []string, prefixes ...string) []string {
	out := env[:0:0]
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}
