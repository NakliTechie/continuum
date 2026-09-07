package server

import (
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"

	"github.com/NakliTechie/menagerie/relay-go/internal/config"
	"github.com/NakliTechie/menagerie/relay-go/internal/protocol"
)

// One live structured session, ready to be waited on.
func waitTestSession(t *testing.T) (*websocket.Conn, string, string) {
	t.Helper()
	cfg := &config.Config{
		Name: "test-relay", Listen: "127.0.0.1:0", Tmux: "off", RegistrationToken: "test-registration-token",
		Agents: map[string]config.Agent{"fake": {Command: fakeAgentBin, Transports: []string{"acp"}, ACPArgs: []string{}}},
	}
	ts := httptest.NewServer(New(cfg).Handler())
	t.Cleanup(ts.Close)

	c := dialWS(t, ts)
	recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeHello })
	sendMsg(t, c, msg{"type": "register", "registration_token": "test-registration-token"})
	recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeRegistered })
	sendMsg(t, c, msg{"type": "spawn", "agent": "fake", "cwd": t.TempDir(), "args": []any{}, "env": msg{},
		"client_id": "cid-wait", "transport": protocol.TransportACP})
	spawned := recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeSpawned })
	sid, _ := spawned["session_id"].(string)
	tok, _ := spawned["session_token"].(string)
	return c, sid, tok
}

func waited(t *testing.T, c *websocket.Conn) frame {
	t.Helper()
	return recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeWaited })
}

// The relay resolves on the transition it already tracks — no client polling.
func TestWaitResolvesOnTransition(t *testing.T) {
	c, sid, tok := waitTestSession(t)
	sendMsg(t, c, msg{"type": "wait", "session_id": sid, "session_token": tok, "until": []any{"done"}, "wait_id": "w1"})
	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": "go"})

	w := waited(t, c)
	if w["state"] != protocol.StatusDone || w["timed_out"] != false || w["wait_id"] != "w1" {
		t.Fatalf("waited = %v, want state=done timed_out=false wait_id=w1", w)
	}
}

// E1: a wait whose condition already holds resolves immediately. Without this a
// supervisor that misses the transition by a millisecond parks until timeout on
// something that already happened.
func TestWaitAlreadySatisfiedResolvesImmediately(t *testing.T) {
	c, sid, tok := waitTestSession(t)
	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": "go"})
	recvUntil(t, c, func(f frame) bool {
		ev, _ := f["event"].(string)
		return f["type"] == "event" && ev == protocol.EventDone
	})

	// The transition is already past; the wait must still return at once.
	sendMsg(t, c, msg{"type": "wait", "session_id": sid, "session_token": tok, "until": []any{"done"}, "wait_id": "late"})
	w := waited(t, c)
	if w["state"] != protocol.StatusDone || w["timed_out"] != false {
		t.Fatalf("waited = %v, want an immediate done", w)
	}
}

// E4: exit resolves every waiter, including ones that never asked for it. A
// waiter must not outlive its session.
func TestWaitDrainedOnSessionExit(t *testing.T) {
	c, sid, tok := waitTestSession(t)
	sendMsg(t, c, msg{"type": "wait", "session_id": sid, "session_token": tok, "until": []any{"needs_input"}, "wait_id": "never"})
	sendMsg(t, c, msg{"type": "signal", "session_id": sid, "session_token": tok, "signal": "kill"})

	w := waited(t, c)
	if w["state"] != protocol.StatusExited {
		t.Fatalf("waited = %v, want state=exited (drained, not left hanging)", w)
	}
	if w["wait_id"] != "never" {
		t.Fatalf("waited lost its wait_id: %v", w)
	}
}

// E8: the caller's timeout is honoured, and reports the state as it stands.
func TestWaitTimesOutWithTheCurrentState(t *testing.T) {
	c, sid, tok := waitTestSession(t)
	sendMsg(t, c, msg{"type": "wait", "session_id": sid, "session_token": tok,
		"until": []any{"needs_input"}, "timeout_ms": 150, "wait_id": "t1"})

	w := waited(t, c)
	if w["timed_out"] != true {
		t.Fatalf("waited = %v, want timed_out=true", w)
	}
	if w["state"] == protocol.StatusNeedsInput {
		t.Fatalf("a timeout must not claim the state it was waiting for: %v", w)
	}
}

// E3: nothing is implicit. `unknown` — or any state — resolves a wait only when
// the caller named it, and a state outside the vocabulary is refused rather than
// accepted into a wait that could never resolve.
func TestWaitRejectsStatesOutsideTheVocabulary(t *testing.T) {
	c, sid, tok := waitTestSession(t)
	sendMsg(t, c, msg{"type": "wait", "session_id": sid, "session_token": tok, "until": []any{"finished"}})
	e := recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeError })
	if e["code"] != protocol.ErrBadWait {
		t.Fatalf("error code = %v, want %s", e["code"], protocol.ErrBadWait)
	}
}

func TestWaitRejectsAnEmptyUntil(t *testing.T) {
	c, sid, tok := waitTestSession(t)
	sendMsg(t, c, msg{"type": "wait", "session_id": sid, "session_token": tok, "until": []any{}})
	e := recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeError })
	if e["code"] != protocol.ErrBadWait {
		t.Fatalf("error code = %v, want %s", e["code"], protocol.ErrBadWait)
	}
}

func TestWaitRejectsABadToken(t *testing.T) {
	c, sid, _ := waitTestSession(t)
	sendMsg(t, c, msg{"type": "wait", "session_id": sid, "session_token": "nope", "until": []any{"done"}})
	e := recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeError })
	if e["code"] != protocol.ErrInvalidToken {
		t.Fatalf("error code = %v, want %s", e["code"], protocol.ErrInvalidToken)
	}
}

// A wait names a set; any member satisfies it.
func TestWaitAcceptsSeveralStates(t *testing.T) {
	c, sid, tok := waitTestSession(t)
	sendMsg(t, c, msg{"type": "wait", "session_id": sid, "session_token": tok,
		"until": []any{"needs_input", "done", "exited"}, "wait_id": "any"})
	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": "go"})

	w := waited(t, c)
	if w["state"] != protocol.StatusDone {
		t.Fatalf("waited = %v, want the member that actually occurred", w)
	}
}

// Several waits can be outstanding on one session; each resolves under its own id.
func TestWaitResolvesEveryMatchingWaiterOnce(t *testing.T) {
	c, sid, tok := waitTestSession(t)
	sendMsg(t, c, msg{"type": "wait", "session_id": sid, "session_token": tok, "until": []any{"done"}, "wait_id": "a"})
	sendMsg(t, c, msg{"type": "wait", "session_id": sid, "session_token": tok, "until": []any{"done"}, "wait_id": "b"})
	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": "go"})

	seen := map[string]int{}
	for i := 0; i < 2; i++ {
		w := waited(t, c)
		id, _ := w["wait_id"].(string)
		seen[id]++
	}
	if seen["a"] != 1 || seen["b"] != 1 {
		t.Fatalf("each waiter resolves exactly once; got %v", seen)
	}
}

// §8.2 E6 — the sharpest edge. A session waiting on a human decision must not
// receive a prompt: the approval dialog would read it as the answer, approving
// or rejecting a tool call the sender never saw. Refuse, and send NOTHING.
func TestPromptRefusedWhileBlockedOnADecision(t *testing.T) {
	cfg := &config.Config{
		Name: "test-relay", Listen: "127.0.0.1:0", Tmux: "off", RegistrationToken: "test-registration-token",
		Agents: map[string]config.Agent{"fake": {Command: fakeAgentBin, Transports: []string{"acp"}, ACPArgs: []string{}}},
	}
	ts := httptest.NewServer(New(cfg).Handler())
	t.Cleanup(ts.Close)

	c := dialWS(t, ts)
	recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeHello })
	sendMsg(t, c, msg{"type": "register", "registration_token": "test-registration-token"})
	recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeRegistered })
	sendMsg(t, c, msg{"type": "spawn", "agent": "fake", "cwd": t.TempDir(), "args": []any{}, "env": msg{"FAKE_PERMISSION": "1"},
		"client_id": "cid-blocked", "transport": protocol.TransportACP})
	spawned := recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeSpawned })
	sid, _ := spawned["session_id"].(string)
	tok, _ := spawned["session_token"].(string)

	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": "first"})
	recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypePermissionRequest })
	recvUntil(t, c, func(f frame) bool {
		ev, _ := f["event"].(string)
		return f["type"] == "event" && ev == protocol.EventNeedsInput
	})

	// The supervisor now prompts a session that is sitting on a dialog.
	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": "yes please do it"})
	e := recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeError })
	if e["code"] != protocol.ErrSessionBlocked {
		t.Fatalf("error code = %v, want %s", e["code"], protocol.ErrSessionBlocked)
	}
}

// §8.2 — the wait is armed in the same frame, so the transition the prompt
// causes cannot land in the gap between a separate prompt and wait.
func TestPromptWithWaitResolvesAtomically(t *testing.T) {
	c, sid, tok := waitTestSession(t)
	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": "go",
		"wait": msg{"until": []any{"done"}, "wait_id": "pw"}})

	w := waited(t, c)
	if w["state"] != protocol.StatusDone || w["wait_id"] != "pw" || w["timed_out"] != false {
		t.Fatalf("waited = %v, want an atomic done for wait_id=pw", w)
	}
}

// A malformed wait rides in on the prompt: refuse the whole frame rather than
// prompting with a wait that could never resolve.
func TestPromptWithABadWaitIsRefusedWhole(t *testing.T) {
	c, sid, tok := waitTestSession(t)
	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": "go",
		"wait": msg{"until": []any{"nonsense"}}})
	e := recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeError })
	if e["code"] != protocol.ErrBadWait {
		t.Fatalf("error code = %v, want %s", e["code"], protocol.ErrBadWait)
	}
	// And nothing was prompted: no turn ran, so no done event follows.
	sendMsg(t, c, msg{"type": "seen", "session_id": sid, "session_token": tok})
	sendMsg(t, c, msg{"type": "signal", "session_id": sid, "session_token": tok, "signal": "kill"})
	f := recvUntil(t, c, func(fr frame) bool {
		ev, _ := fr["event"].(string)
		return fr["type"] == "event" && (ev == protocol.EventDone || ev == protocol.EventExited)
	})
	if ev, _ := f["event"].(string); ev != protocol.EventExited {
		t.Fatalf("a refused prompt still ran a turn: %v", f)
	}
}

// E8: a short caller timeout is honoured as a plain timeout on the wait the
// prompt carried.
func TestPromptShortTimeoutIsHonoured(t *testing.T) {
	c, sid, tok := waitTestSession(t)
	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": "go",
		"wait": msg{"until": []any{"needs_input"}, "timeout_ms": 120, "wait_id": "short"}})

	w := waited(t, c)
	if w["timed_out"] != true || w["wait_id"] != "short" {
		t.Fatalf("waited = %v, want a timed-out short wait", w)
	}
}

// E7, as the build revised it: on ACP there is no stall guard, because that edge
// belongs to blind keystroke injection (see the note in wait.go). A prompt the
// agent swallows entirely is reported by the wait's own timeout — honest, and
// it cannot mislabel a slow agent.
func TestPromptSwallowedByTheAgentTimesOut(t *testing.T) {
	cfg := &config.Config{
		Name: "test-relay", Listen: "127.0.0.1:0", Tmux: "off", RegistrationToken: "test-registration-token",
		Agents: map[string]config.Agent{"fake": {Command: fakeAgentBin, Transports: []string{"acp"}, ACPArgs: []string{}}},
	}
	ts := httptest.NewServer(New(cfg).Handler())
	t.Cleanup(ts.Close)

	c := dialWS(t, ts)
	recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeHello })
	sendMsg(t, c, msg{"type": "register", "registration_token": "test-registration-token"})
	recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeRegistered })
	sendMsg(t, c, msg{"type": "spawn", "agent": "fake", "cwd": t.TempDir(), "args": []any{},
		"env": msg{"FAKE_SWALLOW_PROMPT": "1"}, "client_id": "cid-stall", "transport": protocol.TransportACP})
	spawned := recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeSpawned })
	sid, _ := spawned["session_id"].(string)
	tok, _ := spawned["session_token"].(string)

	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": "into the void",
		"wait": msg{"until": []any{"done"}, "timeout_ms": 200, "wait_id": "void"}})

	w := waited(t, c)
	if w["timed_out"] != true || w["wait_id"] != "void" {
		t.Fatalf("waited = %v, want a timed-out void wait", w)
	}
}

// The bug that took the stall guard out: an agent that simply THINKS for longer
// than any grace window must not be called stalled. A prompt does not move the
// session's status by itself, and a slow agent emits nothing while it thinks, so
// every signal available to a guard here fires on the common case.
func TestPromptSlowAgentIsNotAStall(t *testing.T) {
	cfg := &config.Config{
		Name: "test-relay", Listen: "127.0.0.1:0", Tmux: "off", RegistrationToken: "test-registration-token",
		Agents: map[string]config.Agent{"fake": {Command: fakeAgentBin, Transports: []string{"acp"}, ACPArgs: []string{}}},
	}
	ts := httptest.NewServer(New(cfg).Handler())
	t.Cleanup(ts.Close)

	c := dialWS(t, ts)
	recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeHello })
	sendMsg(t, c, msg{"type": "register", "registration_token": "test-registration-token"})
	recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeRegistered })
	sendMsg(t, c, msg{"type": "spawn", "agent": "fake", "cwd": t.TempDir(), "args": []any{},
		"env": msg{"FAKE_SLOW_MS": "600"}, "client_id": "cid-slow", "transport": protocol.TransportACP})
	spawned := recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeSpawned })
	sid, _ := spawned["session_id"].(string)
	tok, _ := spawned["session_token"].(string)

	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": "think hard",
		"wait": msg{"until": []any{"done"}, "timeout_ms": 60000, "wait_id": "slow"}})

	w := waited(t, c)
	if w["state"] != protocol.StatusDone || w["timed_out"] != false {
		t.Fatalf("a slow turn resolved wrongly: %v", w)
	}
}

// §8.3 — an agent declaring its own state is authoritative, and the declaration
// resolves waits like any other transition.
func TestReportStatusResolvesAWait(t *testing.T) {
	c, sid, tok := waitTestSession(t)
	sendMsg(t, c, msg{"type": "wait", "session_id": sid, "session_token": tok, "until": []any{"needs_input"}, "wait_id": "rs"})
	sendMsg(t, c, msg{"type": "report_status", "session_id": sid, "session_token": tok, "state": "needs_input", "message": "which branch?"})

	w := waited(t, c)
	if w["state"] != protocol.StatusNeedsInput || w["wait_id"] != "rs" {
		t.Fatalf("waited = %v, want the declared state", w)
	}
}

// E3's producer: `unknown` is declarable, and resolves a wait only when the
// caller named it. Nothing about it is implicit.
func TestReportStatusUnknownResolvesOnlyWhenNamed(t *testing.T) {
	c, sid, tok := waitTestSession(t)
	// This wait does NOT name unknown, so the report must not satisfy it.
	sendMsg(t, c, msg{"type": "wait", "session_id": sid, "session_token": tok,
		"until": []any{"done"}, "timeout_ms": 250, "wait_id": "notunknown"})
	sendMsg(t, c, msg{"type": "report_status", "session_id": sid, "session_token": tok, "state": "unknown"})

	w := waited(t, c)
	if w["timed_out"] != true {
		t.Fatalf("unknown satisfied a wait that never named it: %v", w)
	}
	// Named explicitly, it resolves.
	sendMsg(t, c, msg{"type": "wait", "session_id": sid, "session_token": tok, "until": []any{"unknown"}, "wait_id": "named"})
	w2 := waited(t, c)
	if w2["state"] != protocol.StatusUnknown || w2["wait_id"] != "named" {
		t.Fatalf("waited = %v, want an immediate unknown", w2)
	}
}

// A session may not declare its own exit: a process ending is observed, not
// announced, and a session that could announce it could hide that it is running.
func TestReportStatusRefusesExited(t *testing.T) {
	c, sid, tok := waitTestSession(t)
	sendMsg(t, c, msg{"type": "report_status", "session_id": sid, "session_token": tok, "state": "exited"})
	e := recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeError })
	if e["code"] != protocol.ErrBadStatus {
		t.Fatalf("error code = %v, want %s", e["code"], protocol.ErrBadStatus)
	}
}

func TestReportStatusRefusesAnUnknownState(t *testing.T) {
	c, sid, tok := waitTestSession(t)
	sendMsg(t, c, msg{"type": "report_status", "session_id": sid, "session_token": tok, "state": "vibing"})
	e := recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeError })
	if e["code"] != protocol.ErrBadStatus {
		t.Fatalf("error code = %v, want %s", e["code"], protocol.ErrBadStatus)
	}
}

// E9: after a self-report the relay's own heuristics stand down for that
// session, so the two sources cannot fight over the status.
func TestReportStatusTakesAuthorityFromTheHeuristics(t *testing.T) {
	cfg := &config.Config{
		Name: "test-relay", Listen: "127.0.0.1:0", Tmux: "off", RegistrationToken: "test-registration-token",
		Agents: map[string]config.Agent{"sh": {Command: "/bin/sh"}},
	}
	srv := New(cfg)
	e := &sessionEntry{token: "t"}
	srv.addSession(e, "s1")

	if e.hasStatusAuthority() {
		t.Fatal("a fresh session must not claim status authority")
	}
	e.takeStatusAuthority()
	if !e.hasStatusAuthority() {
		t.Fatal("a self-reporting session must hold status authority")
	}
}
