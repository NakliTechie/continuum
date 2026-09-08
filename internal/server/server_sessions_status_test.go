package server

import (
	"testing"

	"github.com/NakliTechie/menagerie/relay-go/internal/protocol"
)

// A re-attaching client must learn the relay's tracked status, not assume
// running: a session that finished while the tab was closed comes back `done`.
func TestSessionsListCarriesTrackedStatus(t *testing.T) {
	ts := acpTestServer(t)
	c, sid, tok := registerAndSpawn(t, ts, protocol.TransportACP, nil)
	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": "go"})
	recvUntil(t, c, func(f frame) bool {
		ev, _ := f["event"].(string)
		return f["type"] == "event" && ev == protocol.EventDone
	})

	c2 := dialWS(t, ts)
	recvUntil(t, c2, func(f frame) bool { return f["type"] == protocol.TypeHello })
	sendMsg(t, c2, msg{"type": "register", "registration_token": "test-registration-token"})
	list := recvUntil(t, c2, func(f frame) bool { return f["type"] == protocol.TypeSessions })
	var got string
	for _, x := range list["sessions"].([]any) {
		info := x.(map[string]any)
		if info["session_id"] == sid {
			got, _ = info["status"].(string)
		}
	}
	if got != protocol.StatusDone {
		t.Fatalf("sessions list status = %q, want %q", got, protocol.StatusDone)
	}
}
