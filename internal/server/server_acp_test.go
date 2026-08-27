package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/NakliTechie/menagerie/relay-go/internal/config"
	"github.com/NakliTechie/menagerie/relay-go/internal/protocol"
)

// The fake ACP agent is built once per test run; unit tests must not require
// omp on the machine (v1.1 handoff §C2).
var fakeAgentBin string

func TestMain(m *testing.M) {
	bin := filepath.Join(os.TempDir(), "menagerie-fakeagent")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/fakeagent")
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Stderr.WriteString("build fakeagent: " + string(out))
		os.Exit(1)
	}
	fakeAgentBin = bin
	code := m.Run()
	os.Remove(bin)
	os.Exit(code)
}

type frame map[string]any

type msg = map[string]any

func acpTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		Name:              "test-relay",
		Listen:            "127.0.0.1:0",
		Tmux:              "off",
		RegistrationToken: "test-registration-token",
		Agents: map[string]config.Agent{
			"fake": {Command: fakeAgentBin, Transports: []string{"acp"}, ACPArgs: []string{}},
			"mini": {Command: "mini"},
		},
	}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func dialWS(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+ts.URL[len("http"):], nil) // empty Origin = non-browser client face
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func sendMsg(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, c, v); err != nil {
		t.Fatalf("send %T: %v", v, err)
	}
}

func recvFrame(t *testing.T, c *websocket.Conn) frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var raw json.RawMessage
	if err := wsjson.Read(ctx, c, &raw); err != nil {
		t.Fatalf("recv: %v", err)
	}
	var f frame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return f
}

// recvUntil consumes frames until pred accepts one.
func recvUntil(t *testing.T, c *websocket.Conn, pred func(f frame) bool) frame {
	t.Helper()
	for i := 0; i < 200; i++ {
		f := recvFrame(t, c)
		b, _ := json.Marshal(f)
		t.Logf("recvUntil[%d]: %s", i, b)
		if pred(f) {
			return f
		}
	}
	t.Fatal("recvUntil: predicate never matched")
	return nil
}

// registerAndSpawn walks hello → register → spawn and hands back the pieces
// the tests need to drive a structured session.
func registerAndSpawn(t *testing.T, ts *httptest.Server, transport any, env map[string]string) (*websocket.Conn, string, string) {
	t.Helper()
	if transport == nil {
		transport = protocol.TransportACP
	}
	c := dialWS(t, ts)

	hello := recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeHello })
	if hello["protocol_version"] != "1.2" {
		t.Fatalf("protocol_version = %v, want 1.2", hello["protocol_version"])
	}
	transports, _ := hello["transports"].([]any)
	var sawACP bool
	for _, tr := range transports {
		if tr == protocol.TransportACP {
			sawACP = true
		}
	}
	if !sawACP {
		t.Fatalf("hello lacks acp transport: %v", transports)
	}
	at, _ := hello["agent_transports"].(map[string]any)
	fts, _ := at["fake"].([]any)
	if len(fts) != 1 || fts[0] != protocol.TransportACP {
		t.Fatalf("hello agent_transports[fake] = %v", at["fake"])
	}

	sendMsg(t, c, msg{"type": "register", "registration_token": "test-registration-token"})
	recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeRegistered })

	sendMsg(t, c, msg{"type": "spawn", "agent": "fake", "cwd": t.TempDir(), "args": []any{}, "env": env, "client_id": "cid-1", "transport": transport})
	spawned := recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeSpawned })
	sid, _ := spawned["session_id"].(string)
	token, _ := spawned["session_token"].(string)
	if sid == "" || token == "" {
		t.Fatalf("spawned missing ids/token: %v", spawned)
	}
	return c, sid, token
}

func TestACPHelloAdvertisesTransports(t *testing.T) {
	ts := acpTestServer(t)
	c := dialWS(t, ts)
	hello := recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeHello })
	if hello["hosts_children"] != false {
		t.Fatalf("hosts_children = %v", hello["hosts_children"])
	}
}

// The §C2 core flow: spawn → streamed updates → prompt accepted → turn completes.
func TestACPSpawnStreamPromptIdle(t *testing.T) {
	ts := acpTestServer(t)
	c, sid, token := registerAndSpawn(t, ts, nil, nil)

	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": token, "text": "say ok"})

	var sawChunk, sawIdle bool
	for i := 0; i < 50 && !(sawChunk && sawIdle); i++ {
		f := recvFrame(t, c)
		b, _ := json.Marshal(f)
		t.Logf("frame[%d] %s", i, b)
		switch f["type"] {
		case protocol.TypeSessionUpdate:
			acpBody, _ := f["acp"].(map[string]any)
			params, _ := acpBody["params"].(map[string]any)
			upd, _ := params["update"].(map[string]any)
			content, _ := upd["content"].(map[string]any)
			if text, _ := content["text"].(string); text == "SMOKE-OK" {
				sawChunk = true
			}
			if seq, ok := f["seq"].(float64); !ok || seq < 1 {
				t.Fatalf("session_update seq bad: %v", f["seq"])
			}
		case "event":
			if ev, _ := f["event"].(string); ev == "idle" {
				sawIdle = true
			}
		}
	}
	if !sawChunk || !sawIdle {
		t.Fatalf("flow incomplete: chunk=%v idle=%v", sawChunk, sawIdle)
	}
}

// Permission round trip: agent asks → relay surfaces request (+ needs_input in
// order) → client approves → turn completes (§C2 checkpoint). Frames are
// collected tolerantly; the outbox guarantees relative order, not absolute.
func TestACPPermissionRoundTrip(t *testing.T) {
	ts := acpTestServer(t)
	c, sid, token := registerAndSpawn(t, ts, nil, map[string]string{"FAKE_PERMISSION": "1"})

	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": token, "text": "edit a file"})

	var reqID string
	var sawNeedsInput bool
	for i := 0; i < 50 && reqID == ""; i++ {
		f := recvFrame(t, c)
		switch f["type"] {
		case protocol.TypePermissionRequest:
			reqID, _ = f["request_id"].(string)
		case "event":
			if ev, _ := f["event"].(string); ev == protocol.EventNeedsInput {
				sawNeedsInput = true
			}
		}
	}
	if reqID == "" {
		t.Fatal("no permission_request surfaced")
	}
	if !sawNeedsInput {
		t.Log("needs_input not observed before the request (ordering tolerated)")
	}

	sendMsg(t, c, msg{"type": "permission_response", "session_id": sid, "session_token": token, "request_id": reqID, "outcome": "approve"})

	var sawIdle bool
	for i := 0; i < 50 && !sawIdle; i++ {
		f := recvFrame(t, c)
		if ev, _ := f["event"].(string); f["type"] == "event" && ev == "idle" {
			sawIdle = true
		}
	}
	if !sawIdle {
		t.Fatal("no idle after approval")
	}
}

// Cancel maps to ACP cancel (interrupt signal), kill stays the hard stop.
func TestACPCancelThenKill(t *testing.T) {
	ts := acpTestServer(t)
	c, sid, token := registerAndSpawn(t, ts, nil, map[string]string{"FAKE_SLOW_MS": "400"})

	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": token, "text": "long turn"})
	sendMsg(t, c, msg{"type": "signal", "session_id": sid, "session_token": token, "signal": "interrupt"})

	recvUntil(t, c, func(f frame) bool {
		ev, _ := f["event"].(string)
		return f["type"] == "event" && ev == "idle" // cancelled turn still completes politely
	})

	sendMsg(t, c, msg{"type": "signal", "session_id": sid, "session_token": token, "signal": "kill"})
	recvUntil(t, c, func(f frame) bool {
		ev, _ := f["event"].(string)
		return f["type"] == "event" && ev == "exited"
	})
}

// acpUpdate pulls the ACP `update` object out of a session_update frame, or nil.
func acpUpdate(f frame) map[string]any {
	if f["type"] != protocol.TypeSessionUpdate {
		return nil
	}
	acpBody, _ := f["acp"].(map[string]any)
	params, _ := acpBody["params"].(map[string]any)
	upd, _ := params["update"].(map[string]any)
	return upd
}

// The instrument bar's data — model/mode/thinking selectors and per-turn token
// usage — is delivered by the agent in the session/new and prompt *responses*,
// which the browser never sees. The relay must re-surface both through the
// session_update funnel: config on spawn, usage on turn end.
func TestACPInstrumentFrames(t *testing.T) {
	ts := acpTestServer(t)
	c, sid, token := registerAndSpawn(t, ts, nil, nil)

	// config_option_update lands right after spawn, carrying the agent's own
	// configOptions verbatim (model=Fake Opus, thinking=Medium, mode=Default).
	cfg := recvUntil(t, c, func(f frame) bool {
		u := acpUpdate(f)
		return u != nil && u["sessionUpdate"] == "config_option_update"
	})
	opts, _ := acpUpdate(cfg)["configOptions"].([]any)
	if len(opts) != 3 {
		t.Fatalf("expected 3 config options, got %d", len(opts))
	}
	seen := map[string]string{}
	for _, o := range opts {
		om, _ := o.(map[string]any)
		cat, _ := om["category"].(string)
		cur, _ := om["currentValue"].(string)
		seen[cat] = cur
	}
	if seen["model"] != "fake/opus" || seen["thought_level"] != "med" || seen["mode"] != "default" {
		t.Fatalf("config selectors wrong: %v", seen)
	}

	// A prompt turn ends with the token counts from the result.
	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": token, "text": "say ok"})
	usg := recvUntil(t, c, func(f frame) bool {
		u := acpUpdate(f)
		return u != nil && u["sessionUpdate"] == "_menagerie/turn_usage"
	})
	usage, _ := acpUpdate(usg)["usage"].(map[string]any)
	if tot, _ := usage["totalTokens"].(float64); tot != 15 {
		t.Fatalf("expected totalTokens 15, got %v", usage["totalTokens"])
	}
}

func TestACPTransportGuards(t *testing.T) {
	ts := acpTestServer(t)
	c, _, token := registerAndSpawn(t, ts, nil, nil)

	// An unknown transport is refused outright — never silently downgraded.
	sendMsg(t, c, msg{"type": "spawn", "agent": "fake", "cwd": t.TempDir(), "args": []any{}, "env": nil, "client_id": "cid-9", "transport": "rpc"})
	errF := recvUntil(t, c, func(f frame) bool { return f["type"] == "error" })
	if code, _ := errF["code"].(string); code != "unsupported_transport" {
		t.Fatalf("expected unsupported_transport, got %v", errF)
	}

	// input stays PTY-only; prompt is the structured analogue.
	sendMsg(t, c, msg{"type": "prompt", "session_id": "nope", "session_token": token, "text": "x"})
	errF = recvUntil(t, c, func(f frame) bool {
		code, _ := f["code"].(string)
		return f["type"] == "error" && (code == protocol.ErrInvalidToken || code == "bad_message")
	})
	_ = errF
}
