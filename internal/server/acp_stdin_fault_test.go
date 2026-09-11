package server

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/internal/acp"
	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/journal"
	"github.com/NakliTechie/continuum/internal/protocol"
)

// An agent that stops draining stdin used to poison the session's writer for
// good: every later cancel or permission answer failed with the first stale
// timeout while the session stayed "running". The first bounded write failure
// now latches, kills the child, and is recorded, so the operator sees an exit
// with a reason instead of a session that cannot be controlled.
func TestACPStdinFaultKillsAndRecords(t *testing.T) {
	script := `import json,sys,time
for line in sys.stdin:
 q=json.loads(line)
 if q.get('method')=='initialize': print(json.dumps({'jsonrpc':'2.0','id':q['id'],'result':{'protocolVersion':1}}),flush=True)
 elif q.get('method')=='session/new': print(json.dumps({'jsonrpc':'2.0','id':q['id'],'result':{'sessionId':'stopped-reader'}}),flush=True)
 elif q.get('method')=='session/prompt':
  print(json.dumps({'jsonrpc':'2.0','id':'huge-option','method':'session/request_permission','params':{'sessionId':'stopped-reader','toolCall':{'toolCallId':'large-option'},'options':[{'optionId':'A'*131072,'name':'Allow','kind':'allow_once'}]}}),flush=True)
  time.sleep(30)
  break
`
	disabled := ""
	cfg := &config.Config{RegistrationToken: "fault-test", Tmux: "off", CaptureDir: &disabled, Agents: map[string]config.Agent{"custom": {}, "stuck": {Command: "python3", Transports: []string{"acp"}, ACPArgs: []string{"-u", "-c", script}}}}
	s := New(cfg)
	store, err := journal.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	s.EnableModern(store, "viewer")
	frames := make(chan map[string]any, 256)
	cn := &conn{srv: s, ctx: context.Background(), registered: true, sink: func(b []byte) error {
		var frame map[string]any
		_ = json.Unmarshal(b, &frame)
		select {
		case frames <- frame:
		default:
		}
		return nil
	}}
	t.Cleanup(func() {
		s.StopAll()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.Drain(ctx)
		store.Close()
	})
	receive := func(match func(map[string]any) bool, what string) map[string]any {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case f := <-frames:
				if match(f) {
					return f
				}
			case <-deadline:
				t.Fatalf("no %s frame", what)
				return nil
			}
		}
	}
	dispatch := func(v any) {
		raw, _ := json.Marshal(v)
		var env protocol.Envelope
		_ = json.Unmarshal(raw, &env)
		cn.dispatch(env, raw)
	}
	cn.handleSpawnACP(protocol.Spawn{Agent: "stuck", Transport: "acp", Cwd: t.TempDir()})
	agent := receive(func(f map[string]any) bool { return f["type"] == "spawned" }, "spawned")
	id, token := agent["session_id"].(string), agent["session_token"].(string)
	dispatch(protocol.Prompt{Type: "prompt", SessionID: id, SessionToken: token, Text: "permission"})
	permission := receive(func(f map[string]any) bool { return f["type"] == "permission_request" }, "permission_request")

	sess := s.entry(id).acp // the Session outlives its registry entry

	// The oversized answer times out against a reader that stopped: first fault.
	started := time.Now()
	dispatch(protocol.PermissionResponse{Type: "permission_response", SessionID: id, SessionToken: token, RequestID: permission["request_id"].(string), Outcome: "approve"})
	first := receive(func(f map[string]any) bool { return f["type"] == "error" }, "first error")
	if !strings.Contains(first["message"].(string), "i/o timeout") {
		t.Fatalf("first failure must be the bounded write: %v", first)
	}
	// A later control must not wait another second on the dead pipe, and must
	// name both the fault and its cause.
	before := time.Now()
	err = sess.Cancel()
	if !errors.Is(err, acp.ErrStdinFaulted) {
		t.Fatalf("second write must report the latched fault, got %v", err)
	}
	var timeout interface{ Timeout() bool }
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("the original timeout must stay in the unwrap chain: %v", err)
	}
	if time.Since(before) > 200*time.Millisecond {
		t.Fatalf("second write waited on the dead pipe: %s", time.Since(before))
	}
	exited := receive(func(f map[string]any) bool { return f["type"] == "event" && f["event"] == "exited" }, "exited")
	if time.Since(started) > 8*time.Second {
		t.Fatalf("exit took %s; the faulted child should have been killed", time.Since(started))
	}
	if exited["session_id"] != id {
		t.Fatalf("wrong session exited: %v", exited)
	}
	page, err := store.Read(0, id)
	if err != nil {
		t.Fatal(err)
	}
	var fault map[string]any
	for _, ev := range page.Events {
		if ev.Type == "agent_fault" {
			_ = json.Unmarshal(ev.Payload, &fault)
		}
	}
	if fault == nil || fault["kind"] != "stdin_write" {
		t.Fatalf("journal must record the stdin fault: %+v", page.Events)
	}
}
