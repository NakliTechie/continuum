package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/internal/acp"
	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/journal"
	"github.com/coder/websocket"
)

const startupProbeScript = `import sys,json,os,time
mode=sys.argv[1]
def send(m): print(json.dumps(m,separators=(',',':')),flush=True)
def upd(n,pad=0): send({'jsonrpc':'2.0','method':'session/update','params':{'sessionId':'checker-session','update':{'sessionUpdate':'agent_message_chunk','content':{'type':'text','text':str(n)+'x'*pad}}}})
def perm(n,pad=0): send({'jsonrpc':'2.0','id':9007199254740993 if n==0 else 'rpc-'+str(n),'method':'session/request_permission','params':{'sessionId':'checker-session','toolCall':{'toolCallId':'checker-call-'+str(n)},'options':[{'optionId':'chosen-allow','kind':'allow_once','name':'Allow deliberately'},{'optionId':'chosen-deny','kind':'reject_once','name':'Reject deliberately'}],'padding':'x'*pad}})
def burst(which):
 if which=='frames':
  for n in range(2049): upd(n)
 elif which=='bytes':
  for n in range(5): upd(n,7<<20)
 elif which=='permissions':
  for n in range(65): perm(n)
 elif which=='permissionbytes':
  for n in range(5): perm(n,7<<20)
for line in sys.stdin:
 m=json.loads(line);method=m.get('method');ident=m.get('id')
 if method=='initialize':send({'jsonrpc':'2.0','id':ident,'result':{'protocolVersion':1,'agentCapabilities':{'loadSession':True}}})
 elif method in ('session/new','session/load'):
  if mode=='mixed':upd('u0');perm(0);upd('u1');perm(1);upd('u2')
  elif mode=='gated':perm(0);continue
  elif mode in ('frames','bytes','permissions','permissionbytes'):burst(mode)
  send({'jsonrpc':'2.0','id':ident,'result':{'sessionId':'checker-session'}})
 elif method=='session/prompt':
  if mode.startswith('live-'):burst(mode[5:]);continue
  send({'jsonrpc':'2.0','id':ident,'result':{'stopReason':'end_turn'}})
 elif method is None and ident is not None:upd('ack:'+json.dumps(m,separators=(',',':')))
`

func startupProbeServer(t *testing.T) (*Server, *httptest.Server, *journal.Store) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	disabled := ""
	cfg := &config.Config{Tmux: "off", RegistrationToken: "test-registration-token", CaptureDir: &disabled, Agents: map[string]config.Agent{"fake": {Command: python, Transports: []string{"acp"}, ACPArgs: []string{"-c", startupProbeScript}}}}
	s := New(cfg)
	store, err := journal.Open(filepath.Join(t.TempDir(), "journal"))
	if err != nil {
		t.Fatal(err)
	}
	s.EnableModern(store, "observer")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		s.StopAll()
		ctx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		if err := s.Drain(ctx); err != nil {
			t.Error(err)
		}
		ts.Close()
		store.Close()
	})
	return s, ts, store
}
func startupProbeConnect(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	c := dialWS(t, ts)
	recvUntil(t, c, func(f frame) bool { return f["type"] == "hello" })
	sendMsg(t, c, msg{"type": "register", "registration_token": "test-registration-token"})
	recvUntil(t, c, func(f frame) bool { return f["type"] == "registered" })
	recvUntil(t, c, func(f frame) bool { return f["type"] == "sessions" })
	return c
}

type checkerWire struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id"`
	Token     string          `json:"session_token"`
	RequestID string          `json:"request_id"`
	Seq       int             `json:"seq"`
	PID       int             `json:"pid"`
	Acp       json.RawMessage `json:"acp"`
	Event     string          `json:"event"`
	Code      string          `json:"code"`
	Message   string          `json:"message"`
}

func checkerRecv(t *testing.T, c *websocket.Conn) checkerWire {
	t.Helper()
	ctx, stop := context.WithTimeout(context.Background(), 8*time.Second)
	defer stop()
	_, b, e := c.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	var f checkerWire
	if e := json.Unmarshal(b, &f); e != nil {
		t.Fatal(e)
	}
	return f
}
func checkerContent(t *testing.T, b json.RawMessage) string {
	t.Helper()
	var p struct {
		Params struct {
			Update struct{ Content struct{ Text string } }
		}
	}
	if e := json.Unmarshal(b, &p); e != nil {
		t.Fatal(e)
	}
	return p.Params.Update.Content.Text
}
func TestCheckerWSStartupApprovalAndReconnect(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprint(resume), func(t *testing.T) {
			s, ts, _ := startupProbeServer(t)
			c := startupProbeConnect(t, ts)
			q := msg{"type": "spawn", "agent": "fake", "transport": "acp", "cwd": t.TempDir(), "args": []string{"mixed"}}
			if resume {
				q["resume_agent_session"] = "checker-session"
			}
			sendMsg(t, c, q)
			spawn := checkerRecv(t, c)
			if spawn.Type != "spawned" || spawn.Token == "" {
				t.Fatalf("spawn %+v", spawn)
			}
			var order []string
			var requests []string
			for len(order) < 5 {
				f := checkerRecv(t, c)
				if f.Type == "event" {
					continue
				}
				if f.Seq != len(order)+1 {
					t.Fatalf("bad sequence %+v", f)
				}
				switch f.Type {
				case "session_update":
					order = append(order, checkerContent(t, f.Acp))
				case "permission_request":
					requests = append(requests, f.RequestID)
					order = append(order, f.RequestID)
					var e acp.Envelope
					if err := json.Unmarshal(f.Acp, &e); err != nil {
						t.Fatal(err)
					}
					want := `9007199254740993`
					if len(requests) == 2 {
						want = `"rpc-1"`
					}
					if string(e.ID) != want || !strings.Contains(string(e.Params), `"name":"Allow deliberately"`) {
						t.Fatalf("identity/options changed %s", f.Acp)
					}
				default:
					t.Fatalf("unexpected startup %+v", f)
				}
			}
			if !reflect.DeepEqual(order, []string{"u0", "pr-1", "u1", "pr-2", "u2"}) {
				t.Fatal(order)
			}
			answer := func(id, opt string) {
				sendMsg(t, c, msg{"type": "permission_response", "session_id": spawn.SessionID, "session_token": spawn.Token, "request_id": id, "option_id": opt})
			}
			answer(requests[0], "not-offered")
			for {
				f := checkerRecv(t, c)
				if f.Type == "event" {
					continue
				}
				if f.Type != "error" || f.Code != "unknown_request" {
					t.Fatalf("invalid answer accepted %+v", f)
				}
				break
			}
			for i, id := range requests {
				answer(id, "chosen-allow")
				for {
					f := checkerRecv(t, c)
					if f.Type == "event" {
						continue
					}
					if f.Type != "session_update" {
						t.Fatalf("ack frame %+v", f)
					}
					text := checkerContent(t, f.Acp)
					var e acp.Envelope
					if err := json.Unmarshal([]byte(strings.TrimPrefix(text, "ack:")), &e); err != nil {
						t.Fatal(err)
					}
					want := `9007199254740993`
					if i == 1 {
						want = `"rpc-1"`
					}
					if string(e.ID) != want || string(e.Result) != `{"outcome":{"outcome":"selected","optionId":"chosen-allow"}}` {
						t.Fatal(text)
					}
					break
				}
			}
			answer(requests[0], "chosen-allow")
			f := checkerRecv(t, c)
			if f.Type != "error" || f.Code != "unknown_request" {
				t.Fatalf("duplicate accepted %+v", f)
			}
			c.CloseNow()
			c2 := startupProbeConnect(t, ts)
			sendMsg(t, c2, msg{"type": "attach", "session_id": spawn.SessionID})
			attached := checkerRecv(t, c2)
			if attached.Type != "attached" {
				t.Fatalf("reattach %+v", attached)
			}
			for i := 0; i < 7; i++ {
				f := checkerRecv(t, c2)
				if f.Seq != -1 || (f.Type != "session_update" && f.Type != "permission_request") {
					t.Fatalf("replay %+v", f)
				}
			}
			e := s.entry(spawn.SessionID)
			e.outMu.Lock()
			total := 0
			for _, b := range e.tail {
				total += len(b)
			}
			if total != e.tailBytes || e.tailBytes > tailByteCapacity || e.outBytes > outboxByteCapacity {
				t.Errorf("accounting tail=%d counter=%d out=%d", total, e.tailBytes, e.outBytes)
			}
			e.outMu.Unlock()
			t.Logf("startup resume=%v mixed order %v; original IDs/options; deliberate approvals; invalid/duplicate refused; seven replay frames seq=-1", resume, order)
		})
	}
}
func TestCheckerWSOverflowIncompleteIsPerBlock(t *testing.T) {
	s, ts, store := startupProbeServer(t)
	c := startupProbeConnect(t, ts)
	spawn := func(mode string) checkerWire {
		sendMsg(t, c, msg{"type": "spawn", "agent": "fake", "transport": "acp", "cwd": t.TempDir(), "args": []string{mode}})
		f := checkerRecv(t, c)
		if f.Type != "spawned" {
			t.Fatalf("spawn %+v", f)
		}
		return f
	}
	unrelated := spawn("idle")
	victim := spawn("live-permissions")
	sendMsg(t, c, msg{"type": "prompt", "session_id": victim.SessionID, "session_token": victim.Token, "text": "overflow"})
	perms := 0
	for i := 0; i < 140; i++ {
		f := checkerRecv(t, c)
		if f.Type == "permission_request" {
			perms++
		}
		if f.Type == "event" && f.Event == "exited" {
			break
		}
		if i == 139 {
			t.Fatal("no exit")
		}
	}
	if perms != 64 {
		t.Fatalf("delivered permissions=%d", perms)
	}
	p, err := store.Read(0, victim.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Incomplete {
		t.Fatal("overflow history not marked incomplete")
	}
	explicit := false
	for _, e := range p.Events {
		if e.Type == "capture_error" && strings.Contains(string(e.Payload), "pending permission budget") {
			explicit = true
		}
	}
	if !explicit {
		t.Fatal("no durable explicit budget error")
	}
	other, err := store.Read(0, unrelated.SessionID)
	if err != nil || other.Incomplete || s.modern.degraded.Load() {
		t.Fatalf("unrelated history affected incomplete=%v degraded=%v err=%v", other.Incomplete, s.modern.degraded.Load(), err)
	}
	if err := syscall.Kill(victim.PID, 0); err != syscall.ESRCH {
		t.Fatalf("child not reaped %v", err)
	}
	t.Logf("64 permissions delivered; child %d reaped; victim incomplete; unrelated history intact", victim.PID)
}
func TestCheckerViewerByteFrameBoundsPumpAndMarker(t *testing.T) {
	if outboxCapacity != 384 || outboxByteCapacity != 16<<20 || tailCapacity != 256 || tailByteCapacity != 16<<20 {
		t.Fatal("contract constants changed")
	}
	s := New(&config.Config{})
	sink := &captureSender{}
	e := &sessionEntry{acp: &acp.Session{}, outbox: make(chan []byte, outboxCapacity), oob: sink}
	s.addSession(e, "bound")
	payload := make([]byte, 1<<20)
	for i := 0; i < 16; i++ {
		if sent, _ := e.trySend(payload); !sent {
			t.Fatalf("under byte limit rejected %d", i)
		}
	}
	if sent, _ := e.trySend([]byte("x")); sent {
		t.Fatal("over byte limit accepted")
	}
	for i := 0; i < 3; i++ {
		s.dropStructured(e, "bound")
	}
	if !reflect.DeepEqual(sink.codes(), []string{"frames_dropped"}) {
		t.Fatal(sink.codes())
	}
	e.outMu.Lock()
	n := e.outBytes
	e.outMu.Unlock()
	if n != 16<<20 || len(e.outbox) != 16 {
		t.Fatalf("outbox accounting %d %d", n, len(e.outbox))
	}
	done := make(chan struct{})
	go func() { s.pumpStructured(e); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.outMu.Lock()
		n = e.outBytes
		e.outMu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pump did not release bytes")
		}
		time.Sleep(time.Millisecond)
	}
	e.closeOutbox()
	<-done
	if sent, closed := e.trySend(payload); sent || !closed {
		t.Fatal("send after close")
	}
	e = &sessionEntry{outbox: make(chan []byte, outboxCapacity)}
	for i := 0; i < 384; i++ {
		if sent, _ := e.trySend([]byte("x")); !sent {
			t.Fatal("under count rejected")
		}
	}
	if sent, _ := e.trySend([]byte("x")); sent {
		t.Fatal("over count accepted")
	}
	if e.outBytes != 384 {
		t.Fatal(e.outBytes)
	}
	e = &sessionEntry{}
	for i := 0; i < 20; i++ {
		b := make([]byte, 1<<20)
		b[0] = byte(i)
		e.appendTail(b)
	}
	tail := e.tailSnapshot()
	if len(tail) != 16 || tail[0][0] != 4 || tail[15][0] != 19 || e.tailBytes != 16<<20 {
		t.Fatal("tail byte eviction/order")
	}
	tail[0][0] = 255
	if e.tail[0][0] == 255 {
		t.Fatal("snapshot aliases tail")
	}
	for i := 0; i < 300; i++ {
		e.appendTail([]byte{byte(i)})
	}
	if len(e.tail) != 256 || e.tailBytes != 256 {
		t.Fatalf("tail count accounting %d %d", len(e.tail), e.tailBytes)
	}
	e.appendTail(make([]byte, (16<<20)+1))
	if len(e.tail) != 0 || e.tailBytes != 0 {
		t.Fatal("oversized tail retained")
	}
}
func TestCheckerConcurrentViewerAccounting(t *testing.T) {
	s := New(&config.Config{})
	e := &sessionEntry{outbox: make(chan []byte, outboxCapacity)}
	done := make(chan struct{})
	go func() { s.pumpStructured(e); close(done) }()
	var wg sync.WaitGroup
	for k := 0; k < 4; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				b := make([]byte, 1024)
				e.trySend(b)
				e.appendTail(b)
				if i%50 == 0 {
					e.tailSnapshot()
				}
			}
		}()
	}
	wg.Wait()
	e.closeOutbox()
	<-done
	total := 0
	for _, b := range e.tail {
		total += len(b)
	}
	if e.outBytes != 0 || e.tailBytes != total || len(e.tail) > 256 || total > 16<<20 {
		t.Fatalf("accounting out=%d tail=%d actual=%d count=%d", e.outBytes, e.tailBytes, total, len(e.tail))
	}
}

func TestCheckerStartupApprovalAllowsFirstPrompt(t *testing.T) {
	_, ts, _ := startupProbeServer(t)
	c := startupProbeConnect(t, ts)
	sendMsg(t, c, msg{"type": "spawn", "agent": "fake", "transport": "acp", "cwd": t.TempDir(), "args": []string{"mixed"}})
	spawned := checkerRecv(t, c)
	if spawned.Type != "spawned" {
		t.Fatalf("spawn %+v", spawned)
	}
	requests := 0
	acks := 0
	for acks < 2 {
		f := checkerRecv(t, c)
		if f.Type == "permission_request" {
			requests++
			sendMsg(t, c, msg{"type": "permission_response", "session_id": spawned.SessionID, "session_token": spawned.Token, "request_id": f.RequestID, "option_id": "chosen-allow"})
		}
		if f.Type == "session_update" && strings.HasPrefix(checkerContent(t, f.Acp), "ack:") {
			acks++
		}
	}
	if requests != 2 {
		t.Fatal(requests)
	}
	sendMsg(t, c, msg{"type": "prompt", "session_id": spawned.SessionID, "session_token": spawned.Token, "text": "first ordinary prompt after approving startup"})
	for i := 0; i < 10; i++ {
		f := checkerRecv(t, c)
		if f.Type == "error" {
			t.Fatalf("first prompt refused after both startup permissions answered: code=%s message=%s", f.Code, f.Message)
		}
		if f.Type == "event" && f.Event == "done" {
			return
		}
	}
	t.Fatal("first prompt did not reach done")
}

func TestCheckerOutstandingStartupPermissionStillBlocks(t *testing.T) {
	s, ts, _ := startupProbeServer(t)
	c := startupProbeConnect(t, ts)
	sendMsg(t, c, msg{"type": "spawn", "agent": "fake", "transport": "acp", "cwd": t.TempDir(), "args": []string{"mixed"}, "resume_agent_session": "checker-session"})
	spawned := checkerRecv(t, c)
	if spawned.Type != "spawned" {
		t.Fatalf("spawn %+v", spawned)
	}
	var ids []string
	for len(ids) < 2 {
		f := checkerRecv(t, c)
		if f.Type == "permission_request" {
			ids = append(ids, f.RequestID)
		}
	}
	sendMsg(t, c, msg{"type": "permission_response", "session_id": spawned.SessionID, "session_token": spawned.Token, "request_id": ids[0], "option_id": "chosen-allow"})
	for {
		f := checkerRecv(t, c)
		if f.Type == "session_update" && strings.HasPrefix(checkerContent(t, f.Acp), "ack:") {
			break
		}
	}
	sendMsg(t, c, msg{"type": "prompt", "session_id": spawned.SessionID, "session_token": spawned.Token, "text": "must remain blocked with one permission"})
	for {
		f := checkerRecv(t, c)
		if f.Type == "event" {
			continue
		}
		if f.Type != "error" || f.Code != "session_blocked" {
			t.Fatalf("outstanding permission admitted prompt %+v", f)
		}
		break
	}
	e := s.entry(spawned.SessionID)
	e.setStatus("idle")
	if !blockedGuard(e) {
		t.Fatal("stale lifecycle status bypassed authoritative pending permissions")
	}
	e.setStatus("needs_input")
	sendMsg(t, c, msg{"type": "permission_response", "session_id": spawned.SessionID, "session_token": spawned.Token, "request_id": ids[1], "option_id": "chosen-allow"})
	for {
		f := checkerRecv(t, c)
		if f.Type == "session_update" && strings.HasPrefix(checkerContent(t, f.Acp), "ack:") {
			break
		}
	}
	sendMsg(t, c, msg{"type": "prompt", "session_id": spawned.SessionID, "session_token": spawned.Token, "text": "first prompt after both resumed permissions"})
	for i := 0; i < 10; i++ {
		f := checkerRecv(t, c)
		if f.Type == "error" {
			t.Fatalf("first resumed prompt refused %+v", f)
		}
		if f.Type == "event" && f.Event == "done" {
			return
		}
	}
	t.Fatal("first resumed prompt did not finish")
}
