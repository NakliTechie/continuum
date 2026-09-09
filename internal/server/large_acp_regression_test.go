package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/journal"
	"github.com/NakliTechie/continuum/internal/protocol"
)

func TestSupplementAcceptedACPFrameMustNotDisableOtherControl(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	script := `import sys,json
for line in sys.stdin:
 m=json.loads(line);method=m.get('method');ident=m.get('id')
 if method=='initialize': r={'protocolVersion':1}
 elif method=='session/new': r={'sessionId':'audit-large-session'}
 elif method=='session/prompt':
  print(json.dumps({'jsonrpc':'2.0','method':'session/update','params':{'sessionId':'audit-large-session','update':{'sessionUpdate':'agent_message_chunk','content':{'type':'text','text':'x'*(300*1024)}}}},separators=(',',':')),flush=True)
  r={'stopReason':'end_turn'}
 else: continue
 print(json.dumps({'jsonrpc':'2.0','id':ident,'result':r}),flush=True)
`
	disabled := ""
	cfg := &config.Config{Name: "audit", Listen: "127.0.0.1:0", Tmux: "off", CaptureDir: &disabled, RegistrationToken: "test-registration-token", Agents: map[string]config.Agent{"custom": {}, "fake": {Command: python, Transports: []string{"acp"}, ACPArgs: []string{"-c", script}}}}
	store, err := journal.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg)
	m := srv.EnableModern(store, "audit-viewer")
	mux := http.NewServeMux()
	mux.Handle("/v1", m)
	mux.Handle("/", srv.Handler())
	ts := httptest.NewServer(mux)
	defer func() {
		srv.StopAll()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Drain(ctx); err != nil {
			t.Error(err)
		}
		ts.Close()
		store.Close()
	}()
	client := &http.Client{Timeout: 3 * time.Second}
	call := func(q api.Request) api.Response {
		t.Helper()
		raw, _ := json.Marshal(q)
		req, _ := http.NewRequest("POST", ts.URL+"/v1", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer test-registration-token")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var v api.Response
		if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	raw := call(api.Request{Operation: "open", RequestID: "raw-open", Args: []string{"/bin/cat"}, Cwd: t.TempDir()})
	rawID := value(t, raw, "block_id")
	held := call(api.Request{Operation: "acquire", RequestID: "raw-control", Block: rawID})
	lease := value(t, held, "lease")
	c, acpID, token := registerAndSpawn(t, ts, protocol.TransportACP, nil)
	c.SetReadLimit(2 << 20)
	sendMsg(t, c, msg{"type": "prompt", "session_id": acpID, "session_token": token, "text": "emit-large-frame"})
	received := 0
	for i := 0; i < 8; i++ {
		f := recvFrame(t, c)
		if f["type"] == "session_update" {
			b, _ := json.Marshal(f["acp"])
			received = len(b)
			break
		}
	}
	if received < 300*1024 {
		t.Fatalf("fake frame did not traverse ACP+WS: bytes=%d", received)
	}
	page, err := store.Read(0, acpID)
	if err != nil {
		t.Fatal(err)
	}
	retainedLarge := false
	for _, event := range page.Events {
		if event.Type == "session_update" && len(event.Payload) >= 300*1024 {
			retainedLarge = true
		}
	}
	result := call(api.Request{Operation: "input", RequestID: "raw-after-frame", Block: rawID, Lease: lease, Data: "still-controllable\n"})
	t.Logf("accepted ACP bytes=%d retained_large=%v degraded=%v unrelated_input_class=%s code=%s", received, retainedLarge, m.degraded.Load(), result.Class, result.Code)
	if result.Class != "ok" {
		t.Fatalf("accepted ACP frame disabled unrelated PTY input: class=%s code=%s", result.Class, result.Code)
	}
	if !retainedLarge {
		t.Fatal("accepted ACP frame was absent from durable history")
	}
}
