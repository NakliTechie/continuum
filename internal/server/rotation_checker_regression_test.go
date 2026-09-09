package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/protocol"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func rotationCheckerServer(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.toml")
	cfg := &config.Config{Name: "checker", Listen: "127.0.0.1:0", Tmux: "off", RegistrationToken: "checker-old", Agents: map[string]config.Agent{"custom": {}}}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	disabled := ""
	loaded.CaptureDir = &disabled
	s := New(loaded)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		s.StopAll()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.Drain(ctx); err != nil {
			t.Error(err)
		}
		ts.Close()
	})
	return s, ts, path
}

func rotationCheckerRegister(t *testing.T, ts *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	c := dialWS(t, ts)
	if f := recvFrame(t, c); f["type"] != "hello" {
		t.Fatal("missing hello")
	}
	sendMsg(t, c, msg{"type": "register", "registration_token": token})
	if f := recvFrame(t, c); f["type"] != "registered" {
		t.Fatalf("current credential rejected: %v", f["type"])
	}
	if f := recvFrame(t, c); f["type"] != "sessions" {
		t.Fatal("missing sessions")
	}
	return c
}

func rotationCheckerRotate(t *testing.T, path string) {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RegistrationToken = "checker-new"
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
}

func rotationCheckerClosed(t *testing.T, c *websocket.Conn, forbidden string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i := 0; i < 100; i++ {
		var f frame
		err := wsjson.Read(ctx, c, &f)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				t.Fatalf("connection was not promptly closed: %v", err)
			}
			return
		}
		data, _ := f["data"].(string)
		decoded, _ := base64.StdEncoding.DecodeString(data)
		if forbidden == "" || strings.Contains(string(decoded), forbidden) {
			t.Fatalf("revoked connection received forbidden frame type %v", f["type"])
		}
	}
	t.Fatal("revoked connection did not close")
}

func rotationCheckerConn(t *testing.T, s *Server) *conn {
	t.Helper()
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if len(s.conns) != 1 {
		t.Fatalf("expected one connection, got %d", len(s.conns))
	}
	for cn := range s.conns {
		return cn
	}
	return nil
}

func TestRotationCheckerNewRegistrationAuthority(t *testing.T) {
	_, ts, path := rotationCheckerServer(t)
	rotationCheckerRotate(t, path)
	old := dialWS(t, ts)
	recvFrame(t, old)
	sendMsg(t, old, msg{"type": "register", "registration_token": "checker-old"})
	if f := recvFrame(t, old); f["type"] != "error" || f["code"] != protocol.ErrAuthFailed {
		t.Fatalf("old token did not receive auth_failed: %v", f["type"])
	}
	rotationCheckerClosed(t, old, "")
	rotationCheckerRegister(t, ts, "checker-new")
}

func TestRotationCheckerExistingOutputWriters(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(map[bool]string{false: "structured-send", true: "raw-send"}[raw], func(t *testing.T) {
			s, ts, path := rotationCheckerServer(t)
			c := rotationCheckerRegister(t, ts, "checker-old")
			cn := rotationCheckerConn(t, s)
			rotationCheckerRotate(t, path)
			var err error
			if raw {
				err = cn.sendRaw([]byte(`{"type":"session_update","checker":"secret"}`))
			} else {
				err = cn.send(msg{"type": "event", "checker": "secret"})
			}
			if err == nil {
				t.Fatal("revoked output writer accepted frame")
			}
			rotationCheckerClosed(t, c, "")
		})
	}
}

func TestRotationCheckerMissingMalformedEmptyConfig(t *testing.T) {
	for _, kind := range []string{"missing", "malformed", "wrong-type", "empty", "absent"} {
		t.Run(kind, func(t *testing.T) {
			_, ts, path := rotationCheckerServer(t)
			old := rotationCheckerRegister(t, ts, "checker-old")
			var err error
			if kind == "missing" {
				err = os.Remove(path)
			} else {
				body := map[string]string{"malformed": "registration_token = [", "wrong-type": "registration_token = 42", "empty": "registration_token = \"\"", "absent": "name = \"no-token\""}[kind]
				err = os.WriteFile(path, []byte(body), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			sendMsg(t, old, msg{"type": "resume", "session_id": "checker"})
			rotationCheckerClosed(t, old, "")
			fresh := dialWS(t, ts)
			recvFrame(t, fresh)
			sendMsg(t, fresh, msg{"type": "register", "registration_token": "checker-old"})
			f := recvFrame(t, fresh)
			if f["type"] != "error" || f["code"] != protocol.ErrAuthFailed {
				t.Fatalf("invalid config admitted registration: %v", f["type"])
			}
			rotationCheckerClosed(t, fresh, "")
		})
	}
}

func TestRotationCheckerQueuedSpawn(t *testing.T) {
	s, ts, path := rotationCheckerServer(t)
	c := rotationCheckerRegister(t, ts, "checker-old")
	cn := rotationCheckerConn(t, s)
	marker := filepath.Join(t.TempDir(), "spawned")
	raw, _ := json.Marshal(msg{"type": "spawn", "agent": "custom", "cwd": t.TempDir(), "args": []string{"/bin/sh", "-c", "echo invalid > \"$1\"", "sh", marker}})
	s.spawnMu.Lock()
	started := make(chan struct{})
	done := make(chan struct{})
	go func() { close(started); cn.dispatch(protocol.Envelope{Type: "spawn"}, raw); close(done) }()
	<-started
	// Hold admission across rotation, with the command already submitted.
	time.Sleep(20 * time.Millisecond)
	rotationCheckerRotate(t, path)
	s.spawnMu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("queued spawn hung")
	}
	rotationCheckerClosed(t, c, "")
	if len(s.listSessions()) != 0 {
		t.Fatal("revoked queued spawn created a session")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("revoked queued spawn executed child")
	}
}

func TestRotationCheckerQueuedInputAndProcessSurvival(t *testing.T) {
	s, ts, path := rotationCheckerServer(t)
	c := rotationCheckerRegister(t, ts, "checker-old")
	marker := filepath.Join(t.TempDir(), "input")
	sendMsg(t, c, msg{"type": "spawn", "agent": "custom", "cwd": t.TempDir(), "args": []string{"/bin/sh", "-c", "printf 'READY\\n'; while IFS= read -r line; do printf '%s\\n' \"$line\" >> \"$1\"; done", "sh", marker}})
	spawned := recvUntil(t, c, func(f frame) bool { return f["type"] == "spawned" })
	sid := spawned["session_id"].(string)
	token := spawned["session_token"].(string)
	recvUntil(t, c, func(f frame) bool {
		d, _ := f["data"].(string)
		b, _ := base64.StdEncoding.DecodeString(d)
		return strings.Contains(string(b), "READY")
	})
	cn := rotationCheckerConn(t, s)
	raw, _ := json.Marshal(msg{"type": "input", "session_id": sid, "session_token": token, "data": "REVOKED_INPUT\n"})
	s.controlMu.Lock()
	started := make(chan struct{})
	done := make(chan struct{})
	go func() { close(started); cn.dispatch(protocol.Envelope{Type: "input"}, raw); close(done) }()
	<-started
	time.Sleep(20 * time.Millisecond)
	rotationCheckerRotate(t, path)
	s.controlMu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("queued input hung")
	}
	rotationCheckerClosed(t, c, "REVOKED_INPUT")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("revoked queued input reached child")
	}
	if s.entry(sid) == nil {
		t.Fatal("rotation terminated session")
	}
	fresh := rotationCheckerRegister(t, ts, "checker-new")
	sendMsg(t, fresh, msg{"type": "attach", "session_id": sid})
	attached := recvUntil(t, fresh, func(f frame) bool { return f["type"] == "attached" })
	sendMsg(t, fresh, msg{"type": "input", "session_id": sid, "session_token": attached["session_token"], "data": "CURRENT_INPUT\n"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(marker)
		if err == nil && len(data) > 0 {
			if string(data) != "CURRENT_INPUT\n" {
				t.Fatalf("unexpected child input %q", data)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("current connection could not use surviving process")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRotationCheckerNativeOutputRevocationAndReattach(t *testing.T) {
	s, ts, path := rotationCheckerServer(t)
	c := rotationCheckerRegister(t, ts, "checker-old")
	gate := filepath.Join(t.TempDir(), "release")
	sendMsg(t, c, msg{"type": "spawn", "agent": "custom", "cwd": t.TempDir(), "args": []string{"/bin/sh", "-c", "printf 'READY\\n'; while [ ! -f \"$1\" ]; do /bin/sleep 0.02; done; printf 'AFTER_ROTATION\\n'; while IFS= read -r line; do printf '%s\\n' \"$line\"; done", "sh", gate}})
	spawned := recvUntil(t, c, func(f frame) bool { return f["type"] == "spawned" })
	sid := spawned["session_id"].(string)
	recvUntil(t, c, func(f frame) bool {
		d, _ := f["data"].(string)
		b, _ := base64.StdEncoding.DecodeString(d)
		return strings.Contains(string(b), "READY")
	})
	rotationCheckerRotate(t, path)
	if err := os.WriteFile(gate, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	rotationCheckerClosed(t, c, "AFTER_ROTATION")
	e := s.entry(sid)
	if e == nil || !strings.Contains(string(e.sess.Buffer()), "AFTER_ROTATION") {
		t.Fatal("child did not survive rotation and generate detached output")
	}
	fresh := rotationCheckerRegister(t, ts, "checker-new")
	sendMsg(t, fresh, msg{"type": "attach", "session_id": sid})
	recvUntil(t, fresh, func(f frame) bool {
		d, _ := f["data"].(string)
		b, _ := base64.StdEncoding.DecodeString(d)
		return strings.Contains(string(b), "AFTER_ROTATION")
	})
}
