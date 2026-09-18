//go:build legacyintegration && (darwin || linux)

package compat_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type frame = map[string]any

type harness struct {
	url, token, home string
	relay, addr      string
	env              []string
	tmux             bool
	cmd              *exec.Cmd
	exited           chan error
}

func startRelay(t *testing.T) *harness { return startRelayWith(t, false) }

// startRelayWith launches a disposable relay; with tmux the relay wraps each
// PTY agent in a private tmux server (TMUX_TMPDIR under the disposable home)
// so the agent can outlive the relay process itself.
func startRelayWith(t *testing.T, withTmux bool) *harness {
	t.Helper()
	relay, fake := os.Getenv("CONTINUUM_TEST_RELAY"), os.Getenv("CONTINUUM_TEST_ACP")
	if !filepath.IsAbs(relay) || !filepath.IsAbs(fake) {
		t.Fatal("absolute CONTINUUM_TEST_RELAY and CONTINUUM_TEST_ACP paths required; run python3 scripts/check_legacy.py")
	}
	for _, path := range []string{relay, fake} {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			t.Fatalf("test executable unavailable: %s", path)
		}
	}
	home := t.TempDir()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	// The legacy CLI cannot inherit a listener. A collision fails this test;
	// it never falls back to the installed relay's port or credentials.
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	h := &harness{url: "ws://" + addr, token: hex.EncodeToString(secret), home: home}
	dir := filepath.Join(home, ".menagerie")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	tmuxMode, path := "off", "/usr/bin:/bin"
	if withTmux {
		tmuxBin, err := exec.LookPath("tmux")
		if err != nil {
			t.Skip("tmux not installed")
		}
		tmuxMode, path = "on", filepath.Dir(tmuxBin)+":"+path
	}
	config := fmt.Sprintf("name = %q\nlisten = %q\nregistration_token = %q\ntmux = %q\nadopt_foreign_tmux = false\nallow_localhost_origins = false\nallowed_origins = [\"https://continuum.test\"]\n[agents.custom]\n[agents.fake]\ncommand = %q\ntransports = [\"acp\"]\n", "continuum-test", addr, h.token, tmuxMode, fake)
	if err := os.WriteFile(filepath.Join(dir, "relay.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	// HOME is only the disposable child process's actual home. The parent
	// environment and user's configuration are never modified or inherited.
	// A private TMUX_TMPDIR keeps any tmux server away from the user's.
	h.relay, h.addr, h.tmux = relay, addr, withTmux
	h.env = []string{"HOME=" + home, "PATH=" + path, "TMPDIR=" + home, "TERM=xterm-256color"}
	if withTmux {
		// Unix socket paths are short; the disposable home is not.
		sock, err := os.MkdirTemp("/tmp", "ct-tmux-")
		if err != nil {
			t.Fatal(err)
		}
		h.env = append(h.env, "TMUX_TMPDIR="+sock)
		t.Cleanup(func() {
			kill := exec.Command("tmux", "kill-server")
			kill.Env = h.env
			_ = kill.Run()
			os.RemoveAll(sock)
		})
	}
	h.launch(t)
	return h
}

// launch starts (or restarts) the relay process on the harness's fixed
// address and waits for it to answer.
func (h *harness) launch(t *testing.T) {
	t.Helper()
	logs, err := os.OpenFile(filepath.Join(h.home, "relay.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	cmd := exec.Command(h.relay, "serve")
	cmd.Dir = h.home
	cmd.Env = h.env
	cmd.Stdout, cmd.Stderr = logs, logs
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	h.cmd, h.exited = cmd, exited
	t.Cleanup(func() {
		// ACP helpers inherit this test's group. PTY descriptors close with
		// the relay, delivering hangup to the disposable terminal process.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			t.Error("disposable relay did not exit")
		}
	})
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		c, _, err := websocket.Dial(ctx, h.url, nil)
		cancel()
		if err == nil {
			_ = c.CloseNow()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("disposable relay did not become reachable on its allocated loopback port")
}

// crash kills the relay process alone — not its process group — the way a
// crash or an unplanned upgrade would, then waits for it to be gone.
func (h *harness) crash(t *testing.T) {
	t.Helper()
	_ = h.cmd.Process.Kill()
	select {
	case err := <-h.exited:
		h.exited <- err // the launch-time cleanup still expects to observe the exit
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not die")
	}
}

func (h *harness) connect(t *testing.T, register bool) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, h.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(1 << 20)
	t.Cleanup(func() { _ = c.CloseNow() })
	hello := until(t, c, func(f frame) bool { return f["type"] == "hello" })
	if hello["protocol_version"] != "1.3" || hello["hosts_children"] != true {
		t.Fatalf("unexpected legacy capabilities: protocol=%v hosts_children=%v", hello["protocol_version"], hello["hosts_children"])
	}
	if register {
		send(t, c, frame{"type": "register", "registration_token": h.token})
		until(t, c, func(f frame) bool { return f["type"] == "registered" })
	}
	return c
}

func send(t *testing.T, c *websocket.Conn, f frame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, c, f); err != nil {
		t.Fatalf("send %v: %v", f["type"], err)
	}
}

func until(t *testing.T, c *websocket.Conn, accept func(frame) bool) frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for n := 0; n < 200; n++ {
		var f frame
		if err := wsjson.Read(ctx, c, &f); err != nil {
			t.Fatalf("await frame: %v", err)
		}
		if accept(f) {
			return f
		}
		if f["type"] == "error" {
			t.Fatalf("unexpected relay error code: %v (%v)", f["code"], f["message"])
		}
	}
	t.Fatal("frame limit reached before expected result")
	return nil
}

func text(t *testing.T, f frame, key string) string {
	t.Helper()
	s, ok := f[key].(string)
	if !ok || s == "" {
		t.Fatalf("missing string field %s on %v", key, f["type"])
	}
	return s
}

func (h *harness) spawn(t *testing.T, c *websocket.Conn, transport string, env map[string]string, parent string) (string, string) {
	t.Helper()
	agent := "fake"
	args := []string{}
	if transport == "pty" {
		agent, args = "custom", []string{"/bin/cat"}
	}
	send(t, c, frame{"type": "spawn", "agent": agent, "transport": transport, "cwd": h.home,
		"args": args, "env": env, "parent_session_id": parent, "client_id": "test-spawn"})
	f := until(t, c, func(f frame) bool { return f["type"] == "spawned" })
	if parent != "" && f["parent_session_id"] != parent {
		t.Fatal("spawn lost parent relationship")
	}
	return text(t, f, "session_id"), text(t, f, "session_token")
}

func stop(t *testing.T, c *websocket.Conn, id, token string) {
	t.Helper()
	send(t, c, frame{"type": "signal", "session_id": id, "session_token": token, "signal": "kill"})
	until(t, c, func(f frame) bool { return f["type"] == "event" && f["event"] == "exited" && f["session_id"] == id })
}

func TestAuthentication(t *testing.T) {
	h := startRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, response, err := websocket.Dial(ctx, h.url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"https://untrusted.test"}}})
	if c != nil {
		_ = c.CloseNow()
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatal("untrusted Origin was not rejected with 403")
	}
	c = h.connect(t, false)
	send(t, c, frame{"type": "spawn", "agent": "custom", "args": []string{"/bin/cat"}})
	f := until(t, c, func(f frame) bool { return f["type"] == "error" })
	if f["code"] != "auth_failed" {
		t.Fatal("unregistered spawn not rejected")
	}
	send(t, c, frame{"type": "register", "registration_token": "wrong-test-token"})
	f = until(t, c, func(f frame) bool { return f["type"] == "error" })
	if f["code"] != "auth_failed" {
		t.Fatal("wrong registration token not rejected")
	}
	good := h.connect(t, true)
	until(t, good, func(f frame) bool { return f["type"] == "sessions" })
}

func TestPTYReattachAndLegacyTakeover(t *testing.T) {
	h := startRelay(t)
	c := h.connect(t, true)
	id, old := h.spawn(t, c, "pty", nil, "")
	send(t, c, frame{"type": "input", "session_id": id, "session_token": old,
		"data": "before-detach\n"})
	until(t, c, func(f frame) bool {
		if f["type"] != "output" {
			return false
		}
		data, _ := base64.StdEncoding.DecodeString(text(t, f, "data"))
		return strings.Contains(string(data), "before-detach")
	})
	// A second client takes over before the first disconnects: the old token
	// must immediately cease authorizing input, independently of WS lifetime.
	d := h.connect(t, true)
	send(t, d, frame{"type": "attach", "session_id": id})
	attached := until(t, d, func(f frame) bool { return f["type"] == "attached" })
	current := text(t, attached, "session_token")
	if current == old {
		t.Fatal("legacy attach did not rotate the token")
	}
	send(t, c, frame{"type": "input", "session_id": id, "session_token": old, "data": "x"})
	// A newer relay first tells the displaced client `session_taken` (additive,
	// protocol 1.3 addendum); the baseline says nothing. Either way the old
	// token must be refused.
	denied := until(t, c, func(f frame) bool { return f["type"] == "error" })
	if denied["code"] == "session_taken" {
		denied = until(t, c, func(f frame) bool { return f["type"] == "error" })
	}
	if denied["code"] != "invalid_token" {
		t.Fatal("old token still controls session")
	}
	_ = c.CloseNow()
	_ = d.CloseNow()
	e := h.connect(t, true)
	send(t, e, frame{"type": "attach", "session_id": id})
	attached = until(t, e, func(f frame) bool { return f["type"] == "attached" })
	current = text(t, attached, "session_token")
	replay := until(t, e, func(f frame) bool { return f["type"] == "output" && f["seq"] == float64(-1) })
	data, err := base64.StdEncoding.DecodeString(text(t, replay, "data"))
	if err != nil || !strings.Contains(string(data), "before-detach") {
		t.Fatal("reconnect did not replay prior output")
	}
	send(t, e, frame{"type": "input", "session_id": id, "session_token": current,
		"data": "after-reattach\n"})
	until(t, e, func(f frame) bool {
		if f["type"] != "output" || f["seq"] == float64(-1) {
			return false
		}
		b, _ := base64.StdEncoding.DecodeString(text(t, f, "data"))
		return strings.Contains(string(b), "after-reattach")
	})
	stop(t, e, id, current)
}

func TestAtomicPromptWaitAndAttention(t *testing.T) {
	h := startRelay(t)
	c := h.connect(t, true)
	id, token := h.spawn(t, c, "acp", nil, "")
	send(t, c, frame{"type": "prompt", "session_id": id, "session_token": token, "text": "go",
		"wait": frame{"until": []string{"done"}, "wait_id": "atomic", "timeout_ms": 3000}})
	f := until(t, c, func(f frame) bool { return f["type"] == "waited" })
	if f["wait_id"] != "atomic" || f["state"] != "done" || f["timed_out"] != false {
		t.Fatal("atomic prompt wait did not resolve correctly")
	}
	d := h.connect(t, true)
	inventory := until(t, d, func(f frame) bool { return f["type"] == "sessions" })
	found := false
	for _, item := range inventory["sessions"].([]any) {
		session := item.(map[string]any)
		if session["session_id"] == id {
			found = session["status"] == "done"
		}
	}
	if !found {
		t.Fatal("inventory read failed to preserve done attention state")
	}
	send(t, c, frame{"type": "seen", "session_id": id, "session_token": token})
	send(t, c, frame{"type": "wait", "session_id": id, "session_token": token, "until": []string{"idle"}, "wait_id": "seen", "timeout_ms": 1000})
	f = until(t, c, func(f frame) bool { return f["type"] == "waited" && f["wait_id"] == "seen" })
	if f["state"] != "idle" || f["timed_out"] != false {
		t.Fatal("explicit seen did not demote done to idle")
	}
	stop(t, c, id, token)
}

func TestApprovalGuard(t *testing.T) {
	h := startRelay(t)
	c := h.connect(t, true)
	id, token := h.spawn(t, c, "acp", map[string]string{"FAKE_PERMISSION": "1"}, "")
	send(t, c, frame{"type": "prompt", "session_id": id, "session_token": token, "text": "edit"})
	request := until(t, c, func(f frame) bool { return f["type"] == "permission_request" })
	send(t, c, frame{"type": "prompt", "session_id": id, "session_token": token, "text": "do something else"})
	f := until(t, c, func(f frame) bool { return f["type"] == "error" })
	if f["code"] != "session_blocked" {
		t.Fatal("task prompt did not preserve pending approval")
	}
	send(t, c, frame{"type": "permission_response", "session_id": id, "session_token": token,
		"request_id": text(t, request, "request_id"), "outcome": "approve", "option_id": "allow-once"})
	until(t, c, func(f frame) bool { return f["type"] == "event" && f["event"] == "done" })
	stop(t, c, id, token)
}

func TestSubtreeKill(t *testing.T) {
	h := startRelay(t)
	c := h.connect(t, true)
	parent, token := h.spawn(t, c, "acp", nil, "")
	child, _ := h.spawn(t, c, "acp", nil, parent)
	send(t, c, frame{"type": "signal", "session_id": parent, "session_token": token, "signal": "kill", "subtree": true})
	exits := map[string]bool{}
	until(t, c, func(f frame) bool {
		if f["type"] == "event" && f["event"] == "exited" {
			exits[text(t, f, "session_id")] = true
		}
		return exits[parent] && exits[child]
	})
}

func TestPTYOutputWhileDisconnected(t *testing.T) {
	h := startRelay(t)
	c := h.connect(t, true)
	trigger := filepath.Join(h.home, "produce-output")
	send(t, c, frame{"type": "spawn", "agent": "custom", "transport": "pty", "cwd": h.home,
		"args":      []string{"/bin/sh", "-c", `while [ ! -f "$1" ]; do sleep 0.02; done; printf 'offline-output\n'; exec /bin/cat`, "test-shell", trigger},
		"client_id": "offline"})
	spawned := until(t, c, func(f frame) bool { return f["type"] == "spawned" })
	id := text(t, spawned, "session_id")
	// No client can observe the new bytes until after the marker appears in
	// the child's own capture file. This tests output during the actual gap.
	_ = c.Close(websocket.StatusNormalClosure, "test detach")
	if err := os.WriteFile(trigger, nil, 0600); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(h.home, ".menagerie", "sessions", id+".pty")
	found := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(capture)
		if err == nil && strings.Contains(string(b), "offline-output") {
			found = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !found {
		t.Fatal("output was not captured with all clients disconnected")
	}
	d := h.connect(t, true)
	send(t, d, frame{"type": "attach", "session_id": id})
	attached := until(t, d, func(f frame) bool { return f["type"] == "attached" })
	if attached["pid"] != spawned["pid"] {
		t.Fatal("reattach replaced the original process")
	}
	f := until(t, d, func(f frame) bool { return f["type"] == "output" && f["seq"] == float64(-1) })
	b, err := base64.StdEncoding.DecodeString(text(t, f, "data"))
	if err != nil || !strings.Contains(string(b), "offline-output") {
		t.Fatal("reattach did not recover output produced while disconnected")
	}
	stop(t, d, id, text(t, attached, "session_token"))
}

// The restart/tmux cell of the compatibility matrix: an agent wrapped in tmux
// survives the relay's own death; the restarted relay adopts it, a client sees
// it in the inventory, attaches, and drives the same process.
func TestRestartWithTmuxAdoptsTheRunningAgent(t *testing.T) {
	h := startRelayWith(t, true)
	c := h.connect(t, true)
	// The agent prefixes every echoed line with its own PID, so the identity
	// of the process behind the session is observable across the restart: a
	// respawned shell would answer with a different number.
	send(t, c, frame{"type": "spawn", "agent": "custom", "transport": "pty", "cwd": h.home,
		"args": []string{"/bin/sh", "-c", `while read l; do echo "$$:$l"; done`}, "env": map[string]string{}, "client_id": "test-restart"})
	spawned := until(t, c, func(f frame) bool { return f["type"] == "spawned" })
	id, token := text(t, spawned, "session_id"), text(t, spawned, "session_token")
	echoed := func(conn *websocket.Conn, marker string) string {
		var pid string
		until(t, conn, func(f frame) bool {
			if f["type"] != "output" || f["seq"] == float64(-1) {
				return false
			}
			b, _ := base64.StdEncoding.DecodeString(text(t, f, "data"))
			// tmux redraws around the echo; only the digits immediately before
			// ":marker" are the PID.
			if m := regexp.MustCompile(`(\d+):` + regexp.QuoteMeta(marker)).FindSubmatch(b); m != nil {
				pid = string(m[1])
				return true
			}
			return false
		})
		return pid
	}
	send(t, c, frame{"type": "input", "session_id": id, "session_token": token, "data": "before-restart\n"})
	before := echoed(c, "before-restart")
	_ = c.CloseNow()
	h.crash(t)
	h.launch(t)
	d := h.connect(t, true)
	inventory := until(t, d, func(f frame) bool { return f["type"] == "sessions" })
	found := false
	for _, s := range inventory["sessions"].([]any) {
		if s.(map[string]any)["session_id"] == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("restarted relay did not adopt the tmux-hosted session %s: %v", id, inventory["sessions"])
	}
	send(t, d, frame{"type": "attach", "session_id": id})
	attached := until(t, d, func(f frame) bool { return f["type"] == "attached" })
	fresh := text(t, attached, "session_token")
	send(t, d, frame{"type": "input", "session_id": id, "session_token": fresh, "data": "after-restart\n"})
	after := echoed(d, "after-restart")
	if before == "" || before != after {
		t.Fatalf("the session is not the same process across restart: pid %q before, %q after", before, after)
	}
	stop(t, d, id, fresh)
}
