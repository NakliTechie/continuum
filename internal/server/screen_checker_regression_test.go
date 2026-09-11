package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/journal"
	"github.com/NakliTechie/continuum/internal/pty"
)

func checkerModern(t *testing.T) *Modern {
	t.Helper()
	store, err := journal.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	disabled := ""
	s := New(&config.Config{RegistrationToken: "operator", Agents: map[string]config.Agent{"custom": {}}, Tmux: "off", CaptureDir: &disabled})
	m := s.EnableModern(store, "viewer")
	t.Cleanup(func() {
		s.StopAll()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.Drain(ctx); err != nil {
			t.Error(err)
		}
		store.Close()
	})
	return m
}

func TestCheckerExitScreenHandoffHasNoAvailabilityGap(t *testing.T) {
	m := checkerModern(t)
	disabled := ""
	sess, err := pty.StartTerminal("handoff", "custom", exec.Command("/bin/sleep", "10"), pty.TerminalOptions{Cols: 80, Rows: 24}, &disabled)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go sess.Run(func(int, []byte) {}, func(int) { close(done) })
	t.Cleanup(func() {
		sess.Kill()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("child did not exit")
		}
	})
	m.s.addSession(&sessionEntry{sess: sess, pid: sess.PID, agent: "custom", startedAt: time.Now()}, "handoff")
	// Hold the process registry after the first retired lookup; wait until screen
	// reaches entry(). Retirement remains free to publish the final frame.
	m.s.mu.Lock()
	result := make(chan api.Response, 1)
	go func() { result <- m.screen(api.Request{Operation: "screen", Block: "handoff"}) }()
	deadline := time.Now().Add(time.Second)
	waiting := false
	for time.Now().Before(deadline) {
		stack := make([]byte, 1<<20)
		n := runtime.Stack(stack, true)
		if strings.Contains(string(stack[:n]), "(*Server).entry(") {
			waiting = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !waiting {
		m.s.mu.Unlock()
		t.Fatal("screen did not reach process lookup")
	}
	m.retireScreen("handoff", sess, 0)
	delete(m.s.sessions, "handoff")
	m.s.mu.Unlock()
	got := <-result
	if got.Class != "ok" {
		t.Fatalf("continuous live-to-retained availability violated: class=%s code=%s; retained count=%d", got.Class, got.Code, len(m.retired))
	}
}

func TestCheckerRetentionEvictsOnlyOldestAndKeepsIdentity(t *testing.T) {
	m := checkerModern(t)
	var ids []string
	for i := 0; i < 18; i++ {
		q := api.Request{Operation: "open", Terminal: "screen-v1", RequestID: fmt.Sprintf("retain-%d", i), Args: []string{"/bin/sh", "-c", fmt.Sprintf("printf FINAL_%d", i)}, Cwd: t.TempDir()}
		m.s.spawnMu.Lock()
		m.s.controlMu.Lock()
		res := m.mutate(q)
		m.s.controlMu.Unlock()
		m.s.spawnMu.Unlock()
		if res.Class != "ok" {
			t.Fatal(res)
		}
		var opened struct {
			Block string `json:"block_id"`
		}
		if err := json.Unmarshal(res.Result, &opened); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, opened.Block)
		deadline := time.Now().Add(2 * time.Second)
		for {
			res = m.screen(api.Request{Operation: "screen", Block: opened.Block})
			var frame screenResult
			_ = json.Unmarshal(res.Result, &frame)
			if res.Class == "ok" && frame.State == "exited" {
				if frame.Block != opened.Block || frame.Host != m.Store.Host || frame.Frame.Engine == "" || frame.Frame.Revision == 0 || frame.ExitCode == nil || *frame.ExitCode != 0 || !strings.Contains(strings.Join(frame.Frame.Lines, ""), fmt.Sprintf("FINAL_%d", i)) {
					t.Fatalf("incomplete final frame: %+v", frame)
				}
				if res.Durability != "volatile" {
					t.Fatal("persistent claim")
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("final frame missing: %+v", res)
			}
			time.Sleep(time.Millisecond)
		}
	}
	if len(m.retired) != 16 || len(m.retiredOrder) != 16 {
		t.Fatalf("retention=%d order=%d", len(m.retired), len(m.retiredOrder))
	}
	for i, id := range ids {
		r := m.screen(api.Request{Operation: "screen", Block: id})
		if i < 2 && r.Code != "screen_unavailable" {
			t.Fatal("oldest not evicted")
		}
		if i >= 2 && r.Class != "ok" {
			t.Fatalf("retained %d: %+v", i, r)
		}
	}
}

func TestCheckerScreenBypassesTmuxWhenEnabled(t *testing.T) {
	m := checkerModern(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "tmux-size.log")
	// The fake records dimensions only, never arbitrary argv or environment.
	script := `#!/bin/sh
case "$1" in
new-session)
 x= y=
 while [ "$#" -gt 0 ]; do
  case "$1" in
   -x) shift; x="$1";;
   -y) shift; y="$1";;
  esac
  shift
 done
 printf '%s %s\n' "$x" "$y" > "$CHECKER_TMUX_SIZE_LOG"
 ;;
attach-session) exec /bin/sleep 10;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CHECKER_TMUX_SIZE_LOG", log)
	m.s.cfg.Tmux = "on"
	q := api.Request{Operation: "open", Terminal: "screen-v1", RequestID: "tmux-open", Args: []string{"/bin/cat"}, Cwd: dir, Cols: 90, Rows: 30}
	m.s.spawnMu.Lock()
	m.s.controlMu.Lock()
	res := m.mutate(q)
	m.s.controlMu.Unlock()
	m.s.spawnMu.Unlock()
	if res.Class != "ok" {
		t.Fatalf("unexpected open error: %+v", res)
	}
	// screen-v1 owns the child directly: a PTY through tmux would replace the
	// engine's screen with tmux's. The fake tmux must never have been invoked.
	if b, err := os.ReadFile(log); !os.IsNotExist(err) {
		t.Fatalf("screen-v1 launch went through tmux (size log %q, err %v); the engine must own the PTY", strings.TrimSpace(string(b)), err)
	}
}
