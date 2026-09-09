package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/journal"
	"github.com/coder/websocket"
)

const o4Python = `import sys,json,os,time
payload=open(sys.argv[1],'rb').read()
for line in sys.stdin:
 m=json.loads(line); method=m.get('method'); ident=m.get('id')
 if method=='initialize': r={'protocolVersion':1}
 elif method=='session/new': r={'sessionId':'o4-session'}
 elif method=='session/prompt':
  if sys.argv[2]=='reader':
   pid=os.fork()
   if pid==0:
    os.setsid(); time.sleep(4); os._exit(0)
   os._exit(0)
  sys.stdout.buffer.write(payload+b'\n'); sys.stdout.buffer.flush()
  r={'stopReason':'end_turn'}
 else: continue
 print(json.dumps({'jsonrpc':'2.0','id':ident,'result':r}),flush=True)
`

type o4Harness struct {
	t      *testing.T
	s      *Server
	m      *Modern
	store  *journal.Store
	ts     *httptest.Server
	dir    string
	closed bool
}

func o4New(t *testing.T, payload []byte, mode string) *o4Harness {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "frame.json")
	if err = os.WriteFile(file, payload, 0600); err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	disabled := ""
	s := New(&config.Config{Name: "o4", RegistrationToken: "test-registration-token", Listen: "127.0.0.1:0", Tmux: "off", CaptureDir: &disabled, Agents: map[string]config.Agent{"custom": {}, "fake": {Command: python, Transports: []string{"acp"}, ACPArgs: []string{"-c", o4Python, file, mode}}}})
	m := s.EnableModern(store, "o4-observer")
	mux := http.NewServeMux()
	mux.Handle("/v1", m)
	mux.Handle("/", s.Handler())
	ts := httptest.NewServer(mux)
	h := &o4Harness{t: t, s: s, m: m, store: store, ts: ts, dir: dir}
	for name, value := range map[string]string{"endpoint": strings.TrimPrefix(ts.URL, "http://"), "operator.token": "test-registration-token"} {
		if err = os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		h.stop()
		if !h.closed {
			h.store.Close()
		}
	})
	return h
}
func (h *o4Harness) stop() {
	h.s.StopAll()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := h.s.Drain(ctx); err != nil {
		h.t.Error(err)
	}
	h.ts.Close()
}
func (h *o4Harness) call(q api.Request) api.Response {
	h.t.Helper()
	raw, err := json.Marshal(q)
	if err != nil {
		h.t.Fatal(err)
	}
	req, err := http.NewRequest("POST", h.ts.URL+"/v1", bytes.NewReader(raw))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-registration-token")
	c := &http.Client{Timeout: 15 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	if q.Operation == "events" && len(b) > 16<<20 {
		h.t.Fatalf("unbounded API bytes %d", len(b))
	}
	var v api.Response
	if err = json.Unmarshal(b, &v); err != nil {
		h.t.Fatal(err)
	}
	return v
}
func o4Frame(kind string, n int) []byte {
	prefix := `{"jsonrpc":"2.0","method":"session/update","x-unknown":{"value":9007199254740993},"params":{"sessionId":"o4-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`
	suffix := `"}}}}`
	if kind == "permission_request" {
		prefix = `{"jsonrpc":"2.0","id":"permission<&>日本","method":"session/request_permission","x-unknown":{"value":9007199254740993},"params":{"sessionId":"o4-session","options":[{"optionId":"allow","kind":"allow_once"}],"toolCall":{"content":"`
		suffix = `"}}}`
	}
	seed := "<>&日本🙂\u2028\u2029"
	seed = strings.ReplaceAll(strings.ReplaceAll(seed, "\\u2028", "\u2028"), "\\u2029", "\u2029")
	remaining := n - len(prefix) - len(suffix)
	body := strings.Repeat(seed, remaining/len(seed)) + strings.Repeat("x", remaining%len(seed))
	return []byte(prefix + body + suffix)
}
func o4Read(c *websocket.Conn, t *testing.T) map[string]json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]json.RawMessage
	if err = json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func o4EventPayload(e journal.Event) []byte {
	if e.Type == "permission_request" {
		var v struct {
			ACP json.RawMessage `json:"acp"`
		}
		_ = json.Unmarshal(e.Payload, &v)
		return v.ACP
	}
	return e.Payload
}
func o4Find(t *testing.T, store *journal.Store, id, kind string) journal.Event {
	t.Helper()
	after := uint64(0)
	for i := 0; i < 50; i++ {
		p, err := store.Read(after, id)
		if err != nil || p.Gap {
			t.Fatalf("read gap=%v err=%v", p.Gap, err)
		}
		for _, e := range p.Events {
			if e.Type == kind {
				return e
			}
		}
		if p.Next == p.Last {
			time.Sleep(10 * time.Millisecond)
		} else if p.Next <= after {
			t.Fatal("stuck cursor")
		}
		after = p.Next
	}
	t.Fatalf("event absent: %s", kind)
	return journal.Event{}
}
func o4Equal(t *testing.T, got, want []byte, route string) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("%s fidelity bytes got=%d want=%d hash got=%x want=%x", route, len(got), len(want), sha256.Sum256(got), sha256.Sum256(want))
	}
}
func (h *o4Harness) raw() (string, string) {
	h.t.Helper()
	r := h.call(api.Request{Operation: "open", RequestID: "o4-open", Args: []string{"/bin/cat"}, Cwd: h.t.TempDir()})
	id := value(h.t, r, "block_id")
	lease := value(h.t, h.call(api.Request{Operation: "acquire", RequestID: "o4-acquire", Block: id}), "lease")
	return id, lease
}
func (h *o4Harness) checkControl(id, lease string) {
	h.t.Helper()
	needle := "o4-pty-positive-<&>-日本"
	r := h.call(api.Request{Operation: "input", RequestID: journal.ID(), Block: id, Lease: lease, Data: needle + "\n"})
	if r.Class != "ok" {
		h.t.Fatalf("control failed %s/%s", r.Class, r.Code)
	}
	deadline := time.Now().Add(3 * time.Second)
	after := uint64(0)
	for time.Now().Before(deadline) {
		p, err := h.store.Read(after, id)
		if err != nil {
			h.t.Fatal(err)
		}
		for _, e := range p.Events {
			var v struct {
				Data string `json:"data"`
			}
			if e.Type == "output" {
				_ = json.Unmarshal(e.Payload, &v)
				raw, _ := base64.StdEncoding.DecodeString(v.Data)
				if bytes.Contains(raw, []byte(needle)) {
					return
				}
			}
		}
		if p.Next < after {
			h.t.Fatal("PTY replay cursor regressed")
		}
		after = p.Next
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatal("PTY input acknowledged but output absent")
}

func TestO4CheckerFullFidelity(t *testing.T) {
	// Supply the checker's required CLI from this exact checkout, never an
	// external or installed binary. The independent payload assertions follow.
	binary := filepath.Join(t.TempDir(), "continuum")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/continuum")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build replay CLI: %v: %s", err, out)
	}
	t.Setenv("O4_CHECKER_CLI", binary)
	for _, kind := range []string{"session_update", "permission_request"} {
		for _, n := range []int{300 << 10, (8 << 20) - 1, (8 << 20) - 2} {
			t.Run(kind+"/"+strconv.Itoa(n), func(t *testing.T) {
				want := o4Frame(kind, n)
				if n == (8<<20)-2 {
					want = bytes.ReplaceAll(want, []byte("日本"), []byte("<&><&>"))
					want = bytes.ReplaceAll(want, []byte("🙂"), []byte("<>&<"))
					want = bytes.ReplaceAll(want, []byte("  "), []byte("<&><&>"))
				}
				if !json.Valid(want) {
					t.Fatal("probe invalid JSON")
				}
				h := o4New(t, want, "")
				rawID, lease := h.raw()
				c, id, token := registerAndSpawn(t, h.ts, nil, nil)
				c.SetReadLimit(16 << 20)
				sendMsg(t, c, msg{"type": "prompt", "session_id": id, "session_token": token, "text": "emit"})
				found := false
				for i := 0; i < 20; i++ {
					v := o4Read(c, t)
					if string(v["type"]) == strconv.Quote(kind) {
						o4Equal(t, v["acp"], want, "websocket")
						found = true
						break
					}
				}
				if !found {
					t.Fatal("no WS payload")
				}
				e := o4Find(t, h.store, id, kind)
				o4Equal(t, o4EventPayload(e), want, "store")
				r := h.call(api.Request{Operation: "events", Block: id, After: e.Seq - 1})
				if r.Class != "ok" {
					t.Fatalf("events %s/%s", r.Class, r.Code)
				}
				var page journal.Page
				if err := json.Unmarshal(r.Result, &page); err != nil {
					t.Fatal(err)
				}
				if len(page.Events) == 0 || page.Events[0].Seq != e.Seq {
					t.Fatal("API missing selected event")
				}
				o4Equal(t, o4EventPayload(page.Events[0]), want, "API")
				if n > 512<<10 && len(page.Events) != 1 {
					t.Fatalf("large page has %d events", len(page.Events))
				}
				binary := os.Getenv("O4_CHECKER_CLI")
				if binary == "" {
					t.Fatal("missing built CLI path")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, binary, "events", "--state", h.dir, "--block", id, "--after", strconv.FormatUint(e.Seq-1, 10), "--json")
				var out, diag bytes.Buffer
				cmd.Stdout = &out
				cmd.Stderr = &diag
				if err := cmd.Run(); err != nil {
					t.Fatalf("CLI %v diag=%s output=%s", err, diag.String(), out.String()[:min(400, out.Len())])
				}
				dec := json.NewDecoder(&out)
				var cliEvent journal.Event
				if err := dec.Decode(&cliEvent); err != nil {
					t.Fatal(err)
				}
				var actual, wanted any
				_ = json.Unmarshal(o4EventPayload(cliEvent), &actual)
				_ = json.Unmarshal(want, &wanted)
				canonA, _ := json.Marshal(actual)
				canonW, _ := json.Marshal(wanted)
				o4Equal(t, canonA, canonW, "CLI semantic")
				if !bytes.Contains(o4EventPayload(cliEvent), []byte("9007199254740993")) {
					t.Fatal("CLI numeric precision lost")
				}
				if h.m.degraded.Load() {
					t.Fatal("writer degraded")
				}
				h.checkControl(rawID, lease)
				t.Logf("full fidelity kind=%s input_bytes=%d hash=%x API_events=%d", kind, n, sha256.Sum256(want), len(page.Events))
			})
		}
	}
}

func TestO4CheckerReadLossIsolationRestart(t *testing.T) {
	for _, mode := range []string{"oversize", "reader"} {
		t.Run(mode, func(t *testing.T) {
			h := o4New(t, o4Frame("session_update", 8<<20), mode)
			rawID, lease := h.raw()
			c, id, token := registerAndSpawn(t, h.ts, nil, nil)
			c.SetReadLimit(16 << 20)
			sendMsg(t, c, msg{"type": "prompt", "session_id": id, "session_token": token, "text": "emit"})
			deadline := time.Now().Add(8 * time.Second)
			for h.s.entry(id) != nil && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if h.s.entry(id) != nil {
				t.Fatal("reader fault child not reaped")
			}
			if h.m.degraded.Load() {
				t.Fatal("frame/reader fault degraded global writer")
			}
			e := o4Find(t, h.store, id, "capture_error")
			t.Logf("capture error mode=%s payload=%s", mode, e.Payload)
			p, err := h.store.Read(0, id)
			if err != nil || !p.Incomplete {
				t.Fatalf("per-block incomplete absent: %v %v", p.Incomplete, err)
			}
			if r := h.call(api.Request{Operation: "events", Block: id}); r.Code != "capture_degraded" {
				t.Fatalf("affected API %s", r.Code)
			}
			if r := h.call(api.Request{Operation: "events", Block: rawID}); r.Class != "ok" {
				t.Fatalf("unrelated API %s", r.Code)
			}
			h.checkControl(rawID, lease)
			h.stop()
			if err = h.m.CleanShutdown(); err != nil {
				t.Fatal(err)
			}
			if err = h.store.Close(); err != nil {
				t.Fatal(err)
			}
			h.closed = true
			reopened, err := journal.Open(h.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			bad, err := reopened.Read(0, id)
			if err != nil || !bad.Incomplete {
				t.Fatal("restart concealed block loss")
			}
			good, err := reopened.Read(0, rawID)
			if err != nil || good.Incomplete {
				t.Fatalf("clean unrelated restart affected: %v %v", good.Incomplete, err)
			}
		})
	}
}

func TestO4CheckerRealJournalFailure(t *testing.T) {
	h := o4New(t, []byte(`{}`), "")
	id, lease := h.raw()
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	h.closed = true
	h.s.recordStructured(id, "session_update", json.RawMessage(`{"valid":true}`))
	if !h.m.degraded.Load() {
		t.Fatal("closed database did not degrade writer")
	}
	r := h.call(api.Request{Operation: "input", RequestID: "after-io-failure", Block: id, Lease: lease, Data: "must-not-run\n"})
	if r.Class != "resource_exhausted" || r.Code != "capture_degraded" {
		t.Fatalf("actual storage failure fence got %s/%s", r.Class, r.Code)
	}
	t.Log(fmt.Sprintf("genuine closed DB failure -> %s/%s", r.Class, r.Code))
}
