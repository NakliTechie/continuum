package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/protocol"
)

// A closed connection takes its waiters with it: nothing is left to receive
// them, and their day-long timers must not keep the dead connection reachable.
// A single connection cannot register waits without bound either.
func TestWaitersDropWithConnectionAndAreBounded(t *testing.T) {
	cfg := &config.Config{RegistrationToken: "t", Tmux: "off", Agents: map[string]config.Agent{"custom": {}}}
	s := New(cfg)
	frames := make(chan map[string]any, 512)
	newConn := func() *conn {
		return &conn{srv: s, ctx: context.Background(), registered: true, sink: func(b []byte) error {
			var f map[string]any
			_ = json.Unmarshal(b, &f)
			select {
			case frames <- f:
			default:
			}
			return nil
		}}
	}
	cn := newConn()
	cn.handleSpawnPTY(protocol.Spawn{Agent: "custom", Args: []string{"/bin/cat"}, Cwd: t.TempDir()})
	var spawned map[string]any
	deadline := time.After(3 * time.Second)
	for spawned == nil {
		select {
		case f := <-frames:
			if f["type"] == "spawned" {
				spawned = f
			}
		case <-deadline:
			t.Fatal("no spawned frame")
		}
	}
	id, token := spawned["session_id"].(string), spawned["session_token"].(string)
	t.Cleanup(func() {
		s.StopAll()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.Drain(ctx)
	})
	e := s.entry(id)
	wait := func(c *conn, i int) {
		raw, _ := json.Marshal(protocol.Wait{Type: protocol.TypeWait, SessionID: id, SessionToken: token, Until: []string{"done"}, WaitID: fmt.Sprintf("w%d", i), TimeoutMS: 86400000})
		c.handleWait(raw)
	}
	for i := 0; i < maxWaiters; i++ {
		wait(cn, i)
	}
	if n := e.pendingWaiters(); n != maxWaiters {
		t.Fatalf("registered %d waiters, want %d", n, maxWaiters)
	}
	wait(cn, maxWaiters)
	var refused map[string]any
	deadline = time.After(2 * time.Second)
	for refused == nil {
		select {
		case f := <-frames:
			if f["type"] == "error" && f["code"] == protocol.ErrBadWait {
				refused = f
			}
		case <-deadline:
			t.Fatalf("wait beyond the bound was not refused; %d pending", e.pendingWaiters())
		}
	}
	other := newConn()
	wait(other, 1000)
	if n := e.pendingWaiters(); n != maxWaiters {
		t.Fatalf("the bound is per session: %d pending", n)
	}
	s.detach(cn)
	if n := e.pendingWaiters(); n != 0 {
		t.Fatalf("detach left %d waiters of the closed connection", n)
	}
	// The surviving connection's wait was refused at the bound, so nothing remains.
}
