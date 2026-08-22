//go:build acpintegration

// Integration test against a REAL `omp acp` child (v1.1 handoff §C2). Gated
// behind a build tag so unit runs never require omp:
//
//	go test ./internal/server/ -tags acpintegration -run TestIntegrationOMP -v
package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/NakliTechie/menagerie/relay-go/internal/config"
	"github.com/NakliTechie/menagerie/relay-go/internal/protocol"
)

func acpServerWithAgents(t *testing.T, agents map[string]config.Agent) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		Name:              "test-relay",
		Listen:            "127.0.0.1:0",
		Tmux:              "off",
		RegistrationToken: "test-registration-token",
		Agents:            agents,
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func registerAndSpawnAgent(t *testing.T, ts *httptest.Server, agent string, env map[string]string) (*websocket.Conn, string, string) {
	t.Helper()
	c := dialWS(t, ts)
	recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeHello })
	sendMsg(t, c, msg{"type": "register", "registration_token": "test-registration-token"})
	recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeRegistered })
	sendMsg(t, c, msg{"type": "spawn", "agent": agent, "cwd": t.TempDir(), "args": []any{}, "env": env, "client_id": "cid-integ", "transport": "acp"})
	spawned := recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeSpawned })
	sid, _ := spawned["session_id"].(string)
	token, _ := spawned["session_token"].(string)
	if sid == "" || token == "" {
		t.Fatalf("bad spawned: %v", spawned)
	}
	return c, sid, token
}

func TestIntegrationOMPPromptRoundTrip(t *testing.T) {
	ts := acpServerWithAgents(t, map[string]config.Agent{
		"omp": {Command: "omp", Transports: []string{"acp"}, ACPArgs: []string{"acp"}},
	})
	c, sid, token := registerAndSpawnAgent(t, ts, "omp", nil)

	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": token, "text": "Reply with exactly: INTEG-OK"})

	var sawChunk, sawIdle bool
	for i := 0; i < 200 && !(sawChunk && sawIdle); i++ {
		f := recvFrame(t, c)
		switch f["type"] {
		case "session_update":
			acpBody, _ := f["acp"].(map[string]any)
			params, _ := acpBody["params"].(map[string]any)
			upd, _ := params["update"].(map[string]any)
			content, _ := upd["content"].(map[string]any)
			if text, _ := content["text"].(string); strings.Contains(text, "INTEG-OK") {
				sawChunk = true
			}
		case "event":
			if ev, _ := f["event"].(string); ev == "idle" {
				sawIdle = true
			}
		}
	}
	if !sawChunk || !sawIdle {
		t.Fatalf("integration flow incomplete: chunk=%v idle=%v", sawChunk, sawIdle)
	}
}
