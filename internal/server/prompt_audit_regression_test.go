package server

import (
	"context"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/protocol"
)

func TestOvernightSecondPromptWaitCannotUsePreviousTurn(t *testing.T) {
	disabled := ""
	cfg := &config.Config{Name: "audit", Listen: "127.0.0.1:0", Tmux: "off", CaptureDir: &disabled, RegistrationToken: "test-registration-token", Agents: map[string]config.Agent{"fake": {Command: fakeAgentBin, Transports: []string{"acp"}, ACPArgs: []string{}}}}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer func() {
		srv.StopAll()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Drain(ctx); err != nil {
			t.Error(err)
		}
	}()
	c, sid, tok := registerAndSpawn(t, ts, protocol.TransportACP, map[string]string{"FAKE_SLOW_MS": "600"})
	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": "first"})
	recvUntil(t, c, func(f frame) bool { return f["type"] == "event" && f["event"] == protocol.EventDone })
	before := time.Now()
	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": "second", "wait": msg{"until": []any{"done"}, "wait_id": "second", "timeout_ms": 2000}})
	w := waited(t, c)
	elapsed := time.Since(before)
	if elapsed < 500*time.Millisecond {
		t.Fatalf("second-turn wait returned in %v before its 600ms turn completed: %v", elapsed, w)
	}
}

func TestOvernightACPErrorIsSurfaced(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	script := `import sys,json
for line in sys.stdin:
 m=json.loads(line); method=m.get('method'); r={'jsonrpc':'2.0','id':m.get('id')}
 if method=='initialize': r['result']={'protocolVersion':1}
 elif method=='session/new': r['result']={'sessionId':'audit-error-session'}
 elif method=='session/prompt': r['error']={'code':-32001,'message':'AUDIT_PROVIDER_FAILURE'}
 else: continue
 print(json.dumps(r),flush=True)
`
	disabled := ""
	cfg := &config.Config{Name: "audit", Listen: "127.0.0.1:0", Tmux: "off", CaptureDir: &disabled, RegistrationToken: "test-registration-token", Agents: map[string]config.Agent{"fake": {Command: python, Transports: []string{"acp"}, ACPArgs: []string{"-c", script}}}}
	srv := New(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer func() {
		srv.StopAll()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Drain(ctx)
	}()
	c, sid, tok := registerAndSpawn(t, ts, protocol.TransportACP, nil)
	sendMsg(t, c, msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": "cause-error"})
	f := recvUntil(t, c, func(f frame) bool {
		return f["type"] == "error" || (f["type"] == "event" && f["event"] == protocol.EventDone)
	})
	if f["type"] != "error" {
		t.Fatalf("agent returned -32001 AUDIT_PROVIDER_FAILURE but client received only %v", f)
	}
}
