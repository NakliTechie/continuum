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
