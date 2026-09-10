package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/protocol"
	"strings"
	"testing"
	"time"
)

func TestO5ACPStoppedReaderDoesNotPinControlOrShutdown(t *testing.T) {
	script := `import json,sys,time
for line in sys.stdin:
 q=json.loads(line)
 if q.get('method')=='initialize': print(json.dumps({'jsonrpc':'2.0','id':q['id'],'result':{'protocolVersion':1}}),flush=True)
 elif q.get('method')=='session/new': print(json.dumps({'jsonrpc':'2.0','id':q['id'],'result':{'sessionId':'stopped-reader'}}),flush=True)
 elif q.get('method')=='session/prompt':
  print(json.dumps({'jsonrpc':'2.0','id':'huge-option','method':'session/request_permission','params':{'sessionId':'stopped-reader','toolCall':{'toolCallId':'large-option'},'options':[{'optionId':'A'*131072,'name':'Allow','kind':'allow_once'}]}}),flush=True)
  time.sleep(20)
  break
`
	disabled := ""
	cfg := &config.Config{RegistrationToken: "o5-test-only", Tmux: "off", CaptureDir: &disabled, Agents: map[string]config.Agent{"custom": {}, "o5-fake": {Command: "python3", Transports: []string{"acp"}, ACPArgs: []string{"-u", "-c", script}}}}
	s := New(cfg)
	frames := make(chan map[string]any, 64)
	cn := &conn{srv: s, ctx: context.Background(), registered: true, sink: func(b []byte) error {
		var frame map[string]any
		if err := json.Unmarshal(b, &frame); err != nil {
			return err
		}
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
		if err := s.Drain(ctx); err != nil {
			t.Error(err)
		}
	})
	receive := func(kind string) map[string]any {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case f := <-frames:
				if f["type"] == kind {
					return f
				}
			case <-deadline:
				t.Fatalf("frame %s deadline", kind)
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
	cn.handleSpawnACP(protocol.Spawn{Agent: "o5-fake", Transport: "acp", Cwd: t.TempDir()})
	agent := receive("spawned")
	cn.handleSpawnPTY(protocol.Spawn{Agent: "custom", Args: []string{"/bin/cat"}, Cwd: t.TempDir()})
	other := receive("spawned")
	dispatch(protocol.Prompt{Type: "prompt", SessionID: agent["session_id"].(string), SessionToken: agent["session_token"].(string), Text: "permission"})
	permission := receive("permission_request")
	started := time.Now()
	answerDone := make(chan struct{})
	go func() {
		defer close(answerDone)
		dispatch(protocol.PermissionResponse{Type: "permission_response", SessionID: agent["session_id"].(string), SessionToken: agent["session_token"].(string), RequestID: permission["request_id"].(string), Outcome: "approve"})
	}()
	// The fake no longer reads stdin, and its selected option exceeds pipe capacity.
	time.Sleep(100 * time.Millisecond)
	otherDone := make(chan struct{})
	go func() {
		defer close(otherDone)
		dispatch(protocol.Signal{Type: "signal", SessionID: other["session_id"].(string), SessionToken: other["session_token"].(string), Signal: "kill"})
	}()
	for label, ch := range map[string]<-chan struct{}{"large permission write": answerDone, "unrelated control": otherDone} {
		select {
		case <-ch:
			t.Logf("%s returned after %s", label, time.Since(started))
		case <-time.After(3 * time.Second):
			t.Fatalf("%s remained blocked", label)
		}
	}
	before := time.Now()
	s.StopAll()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("shutdown drained after %s", time.Since(before))
	// Assert the refusal really came from bounded pipe I/O rather than a test bypass.
	var errors []string
	for len(frames) > 0 {
		f := <-frames
		if f["type"] == "error" {
			errors = append(errors, fmt.Sprint(f["message"]))
		}
	}
	if !strings.Contains(strings.Join(errors, " "), "i/o timeout") {
		t.Fatalf("no bounded write timeout seen: %v", errors)
	}
}
