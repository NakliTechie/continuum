package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/internal/protocol"
	"github.com/coder/websocket"
)

// The legacy door streams to one client per session and attach reissues the
// token. The displaced client must hear about it (CU7, live run 2026-09-12:
// the first browser went silent until it re-attached by hand).
func TestLegacyAttachTellsTheDisplacedClient(t *testing.T) {
	ts := acpTestServer(t)
	first, sid, oldToken := registerAndSpawn(t, ts, nil, nil)

	second := registerClient(t, ts)
	sendMsg(t, second, msg{"type": "attach", "session_id": sid})
	attached := recvUntil(t, second, func(f frame) bool { return f["type"] == protocol.TypeAttached })
	if attached["session_token"] == oldToken || attached["session_token"] == "" {
		t.Fatalf("attach did not reissue the token: %v", attached)
	}

	taken := recvUntil(t, first, func(f frame) bool { return f["type"] == "error" && f["code"] == protocol.ErrSessionTaken })
	if taken["session_id"] != sid {
		t.Fatalf("session_taken names the wrong session: %v", taken)
	}

	// The old token is void; the displaced client learns that on its next control.
	sendMsg(t, first, msg{"type": "prompt", "session_id": sid, "session_token": oldToken, "text": "stale"})
	recvUntil(t, first, func(f frame) bool { return f["type"] == "error" && f["code"] == protocol.ErrInvalidToken })

	// Re-attaching from the first client takes the stream back and tells the second.
	sendMsg(t, first, msg{"type": "attach", "session_id": sid})
	recvUntil(t, first, func(f frame) bool { return f["type"] == protocol.TypeAttached })
	recvUntil(t, second, func(f frame) bool { return f["type"] == "error" && f["code"] == protocol.ErrSessionTaken })

	// The same client attaching again is a refresh, not a displacement: no notice.
	sendMsg(t, first, msg{"type": "attach", "session_id": sid})
	recvUntil(t, first, func(f frame) bool { return f["type"] == protocol.TypeAttached })
	sendMsg(t, first, msg{"type": "signal", "session_id": sid, "session_token": "bogus", "signal": "kill"})
	f := recvUntil(t, first, func(f frame) bool { return f["type"] == "error" })
	if f["code"] == protocol.ErrSessionTaken {
		t.Fatalf("self re-attach produced a session_taken notice: %v", f)
	}
}

// An answered permission request is not replayed to a client that attaches
// later; a still-pending one is.
func TestLegacyAttachReplaysOnlyPendingPermissions(t *testing.T) {
	ts := acpTestServer(t)
	first, sid, token := registerAndSpawn(t, ts, nil, map[string]string{"FAKE_PERMISSION": "1", "FAKE_SLOW_MS": "200"})
	sendMsg(t, first, msg{"type": "prompt", "session_id": sid, "session_token": token, "text": "turn one"})
	req := recvUntil(t, first, func(f frame) bool { return f["type"] == protocol.TypePermissionRequest })
	reqID, _ := req["request_id"].(string)
	sendMsg(t, first, msg{"type": "permission_response", "session_id": sid, "session_token": token, "request_id": reqID, "outcome": "approve"})
	recvUntil(t, first, func(f frame) bool {
		ev, _ := f["event"].(string)
		return f["type"] == "event" && ev == protocol.EventDone
	})

	second := registerClient(t, ts)
	sendMsg(t, second, msg{"type": "attach", "session_id": sid})
	attached := recvUntil(t, second, func(f frame) bool { return f["type"] == protocol.TypeAttached })
	newToken, _ := attached["session_token"].(string)
	// A context deadline on Read closes a coder/websocket conn, so the drain is a
	// goroutine feeding a channel that this client keeps reading from afterwards.
	frames := pumpFrames(second)
	quiet := time.After(600 * time.Millisecond)
drain:
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("second client closed during replay")
			}
			if f["type"] == protocol.TypePermissionRequest {
				t.Fatalf("an answered permission request was replayed: %v", f)
			}
		case <-quiet:
			break drain
		}
	}

	// A request that is still open does replay, at seq -1, to the newcomer.
	sendMsg(t, second, msg{"type": "prompt", "session_id": sid, "session_token": newToken, "text": "turn two"})
	waitFrame(t, frames, func(f frame) bool { return f["type"] == protocol.TypePermissionRequest })
	third := registerClient(t, ts)
	sendMsg(t, third, msg{"type": "attach", "session_id": sid})
	replayed := recvUntil(t, third, func(f frame) bool { return f["type"] == protocol.TypePermissionRequest })
	if seq, _ := replayed["seq"].(float64); seq != -1 {
		t.Fatalf("pending permission replayed with seq %v, want -1", replayed["seq"])
	}
}

func registerClient(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	c := dialWS(t, ts)
	recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeHello })
	sendMsg(t, c, msg{"type": "register", "registration_token": "test-registration-token"})
	recvUntil(t, c, func(f frame) bool { return f["type"] == protocol.TypeRegistered })
	return c
}

// pumpFrames reads a connection on its own goroutine and hands frames over a
// channel; the channel closes when the connection does.
func pumpFrames(c *websocket.Conn) <-chan frame {
	ch := make(chan frame, 256)
	go func() {
		defer close(ch)
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				return
			}
			f := frame{}
			if json.Unmarshal(b, &f) == nil {
				ch <- f
			}
		}
	}()
	return ch
}

func waitFrame(t *testing.T, frames <-chan frame, pred func(f frame) bool) frame {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("connection closed before the expected frame")
			}
			if pred(f) {
				return f
			}
		case <-deadline:
			t.Fatal("expected frame did not arrive")
		}
	}
}
