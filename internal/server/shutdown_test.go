package server

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/config"
)

func TestShutdownCancelsHandshakeWithoutBlockingControl(t *testing.T) {
	m, call := modernTest(t)
	m.s.cfg.Agents["fake"] = config.Agent{Command: fakeAgentBin, Transports: []string{"acp"}, ACPArgs: []string{}}
	// Configuration completes before this second listener accepts any connection.
	ws := httptest.NewServer(m.s.Handler())
	defer ws.Close()
	opened := call("operator", api.Request{Operation: "open", RequestID: "open", Cwd: t.TempDir(), Args: []string{"/bin/cat"}})
	id := value(t, opened, "block_id")
	lease := value(t, call("operator", api.Request{Operation: "acquire", RequestID: "acquire", Block: id}), "lease")
	cn := dialWS(t, ws)
	recvUntil(t, cn, func(f frame) bool { return f["type"] == "hello" })
	sendMsg(t, cn, msg{"type": "register", "registration_token": "operator"})
	recvUntil(t, cn, func(f frame) bool { return f["type"] == "registered" })
	marker := filepath.Join(t.TempDir(), "handshake-started")
	sendMsg(t, cn, msg{"type": "spawn", "agent": "fake", "transport": "acp", "cwd": t.TempDir(), "env": map[string]string{"FAKE_STALL_INITIALIZE": marker}})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("handshake did not start")
		}
		time.Sleep(time.Millisecond)
	}
	control := make(chan api.Response, 1)
	go func() {
		control <- call("operator", api.Request{Operation: "input", RequestID: "during-handshake", Block: id, Lease: lease, Data: "still-responsive\n"})
	}()
	select {
	case r := <-control:
		if r.Class != "ok" {
			t.Fatal(r)
		}
	case <-time.After(time.Second):
		m.s.BeginShutdown()
		t.Fatal("unrelated control blocked by startup")
	}
	done := make(chan struct{})
	go func() { m.s.StopAll(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown blocked by startup")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.s.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if r := call("operator", api.Request{Operation: "open", RequestID: "late", Args: []string{"/bin/cat"}}); r.Code != "shutdown" || r.Class != "unreachable" || api.Exit(r.Class) != 5 {
		t.Fatal("accepted spawn during shutdown", r)
	}
}
