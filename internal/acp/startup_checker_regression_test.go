package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const checkerScript = `import sys,json,os,time
mode=sys.argv[1]
def send(m): print(json.dumps(m,separators=(',',':')),flush=True)
def upd(n,pad=0): send({'jsonrpc':'2.0','method':'session/update','params':{'sessionId':'checker-session','update':{'sessionUpdate':'agent_message_chunk','content':{'type':'text','text':str(n)+'x'*pad}}}})
def perm(n,pad=0): send({'jsonrpc':'2.0','id':9007199254740993 if n==0 else 'rpc-'+str(n),'method':'session/request_permission','params':{'sessionId':'checker-session','options':[{'optionId':'chosen-allow','kind':'allow_once','name':'Allow deliberately'},{'optionId':'chosen-deny','kind':'reject_once','name':'Reject deliberately'}],'padding':'x'*pad}})
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

func checkerCmd(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	p, e := exec.LookPath("python3")
	if e != nil {
		t.Fatal(e)
	}
	return exec.Command(p, "-c", checkerScript, mode)
}
func checkerStart(t *testing.T, mode string, resume bool) (*Session, <-chan struct{}) {
	t.Helper()
	ctx, c := context.WithTimeout(context.Background(), 8*time.Second)
	defer c()
	cmd := checkerCmd(t, mode)
	disabled := ""
	var s *Session
	var e error
	if resume {
		s, e = ResumeContext(ctx, "checker", "fake", t.TempDir(), cmd, "checker-session", &disabled)
	} else {
		s, e = StartContext(ctx, "checker", "fake", t.TempDir(), cmd, &disabled)
	}
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	go s.Run(func(int) { close(done) })
	t.Cleanup(func() {
		s.Kill()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("child not reaped")
		}
	})
	return s, done
}
func TestCheckerRealStartupMixedPermissionPositive(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprint(resume), func(t *testing.T) {
			s, _ := checkerStart(t, "mixed", resume)
			var order []string
			acks := make(chan string, 2)
			var errs []error
			s.SetSinks(func(b json.RawMessage) {
				var e Envelope
				if err := json.Unmarshal(b, &e); err != nil {
					errs = append(errs, err)
					return
				}
				var p struct {
					Update struct{ Content struct{ Text string } }
				}
				json.Unmarshal(e.Params, &p)
				text := p.Update.Content.Text
				if strings.HasPrefix(text, "ack:") {
					acks <- strings.TrimPrefix(text, "ack:")
				} else {
					order = append(order, text)
				}
			}, func(id string, b json.RawMessage) {
				order = append(order, id)
				var e Envelope
				json.Unmarshal(b, &e)
				want := `9007199254740993`
				if id == "pr-2" {
					want = `"rpc-1"`
				}
				if string(e.ID) != want {
					errs = append(errs, fmt.Errorf("id %s != %s", e.ID, want))
				}
				if !bytes.Contains(b, []byte(`"name":"Allow deliberately"`)) {
					errs = append(errs, fmt.Errorf("options lost"))
				}
				if err := s.RespondPermission(id, "approve", "not-offered"); err == nil {
					errs = append(errs, fmt.Errorf("invalid accepted"))
				}
				if err := s.RespondPermission(id, "", "chosen-allow"); err != nil {
					errs = append(errs, err)
				}
				if err := s.RespondPermission(id, "approve", ""); err == nil {
					errs = append(errs, fmt.Errorf("duplicate accepted"))
				}
			})
			if !reflect.DeepEqual(order, []string{"u0", "pr-1", "u1", "pr-2", "u2"}) {
				t.Fatalf("mixed order=%v", order)
			}
			if len(errs) > 0 {
				t.Fatal(errs)
			}
			for i := 0; i < 2; i++ {
				select {
				case a := <-acks:
					var e Envelope
					if err := json.Unmarshal([]byte(a), &e); err != nil {
						t.Fatal(err)
					}
					want := `9007199254740993`
					if i == 1 {
						want = `"rpc-1"`
					}
					if string(e.ID) != want || string(e.Result) != `{"optionId":"chosen-allow"}` {
						t.Fatalf("response changed %s", a)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("no child response acknowledgement")
				}
			}
			s.SetSinks(func(json.RawMessage) { t.Error("duplicate replay") }, func(string, json.RawMessage) { t.Error("duplicate permission replay") })
			s.updateMu.Lock()
			held, heldBytes := len(s.pendingEvents), s.pendingBytes
			s.updateMu.Unlock()
			s.mu.Lock()
			perms, pbytes := len(s.perms), s.permissionBytes
			s.mu.Unlock()
			if held != 0 || heldBytes != 0 || perms != 0 || pbytes != 0 {
				t.Fatalf("retained %d/%d %d/%d", held, heldBytes, perms, pbytes)
			}
		})
	}
}
func TestCheckerStartupOverflowAndGatedCancellationReap(t *testing.T) {
	for _, mode := range []string{"frames", "bytes", "permissions", "permissionbytes", "gated"} {
		t.Run(mode, func(t *testing.T) {
			ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
			if mode == "gated" {
				c()
				ctx, c = context.WithTimeout(context.Background(), 500*time.Millisecond)
			}
			defer c()
			cmd := checkerCmd(t, mode)
			disabled := ""
			began := time.Now()
			s, e := StartContext(ctx, "checker", "fake", t.TempDir(), cmd, &disabled)
			if s != nil || e == nil {
				t.Fatalf("overflow accepted s=%v err=%v", s, e)
			}
			want := "startup event budget"
			if strings.Contains(mode, "permission") {
				want = "pending permission budget"
			}
			if mode == "gated" {
				want = "context deadline exceeded"
			}
			if !strings.Contains(e.Error(), want) {
				t.Fatalf("error %v missing %s", e, want)
			}
			if time.Since(began) > 6*time.Second {
				t.Fatal("startup not bounded")
			}
			if cmd.ProcessState == nil {
				t.Fatal("child not reaped")
			}
			if err := syscall.Kill(cmd.Process.Pid, 0); err != syscall.ESRCH {
				t.Fatalf("child alive %v", err)
			}
			t.Logf("%s: %v; reaped in %v", mode, e, time.Since(began))
		})
	}
}
func TestCheckerEstablishedPermissionOverflowReaps(t *testing.T) {
	for _, mode := range []string{"permissions", "permissionbytes"} {
		t.Run(mode, func(t *testing.T) {
			s, done := checkerStart(t, "live-"+mode, false)
			var delivered atomic.Int32
			s.SetSinks(func(json.RawMessage) {}, func(string, json.RawMessage) { delivered.Add(1) })
			if _, e := s.Prompt("burst"); e != nil {
				t.Fatal(e)
			}
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				t.Fatal("overflow child did not exit")
			}
			if s.ReadError() == nil || !strings.Contains(s.ReadError().Error(), "pending permission budget") {
				t.Fatalf("missing explicit overflow: %v", s.ReadError())
			}
			if delivered.Load() > 64 || s.permissionBytes > 32<<20 {
				t.Fatalf("retention breached %d %d", delivered.Load(), s.permissionBytes)
			}
			t.Logf("%s delivered=%d retained_bytes=%d reaped", mode, delivered.Load(), s.permissionBytes)
		})
	}
}
func TestCheckerSinkReplacementNilAndConcurrentDelivery(t *testing.T) {
	var output bytes.Buffer
	s := &Session{perms: map[string]*pendingPerm{}, w: bufio.NewWriter(&output)}
	var updates, perms atomic.Int32
	u := func(json.RawMessage) { updates.Add(1) }
	p := func(id string, _ json.RawMessage) {
		perms.Add(1)
		if e := s.RespondPermission(id, "approve", ""); e != nil {
			t.Error(e)
		}
	}
	s.SetSinks(nil, nil)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			s.SetSinks(nil, nil)
			s.SetOnUpdate(u)
			s.SetOnPermissionRequest(p)
			s.SetSinks(u, p)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 600; i++ {
			s.dispatch(&Envelope{JSONRPC: "2.0", Method: "session/update", Params: json.RawMessage(`{}`)})
			if i%30 == 0 {
				s.dispatch(&Envelope{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprint(i + 1)), Method: "session/request_permission", Params: json.RawMessage(`{"options":[{"optionId":"ok","kind":"allow_once"}]}`)})
			}
		}
	}()
	wg.Wait()
	s.SetSinks(u, p)
	if updates.Load() != 600 || perms.Load() != 20 {
		t.Fatalf("callbacks %d %d", updates.Load(), perms.Load())
	}
	if s.EventError() != nil || s.pendingBytes != 0 || s.permissionBytes != 0 {
		t.Fatalf("error or retained bytes %v %d %d", s.EventError(), s.pendingBytes, s.permissionBytes)
	}
}
func TestCheckerExactStartupLimits(t *testing.T) {
	t.Run("frames", func(t *testing.T) {
		s := &Session{}
		for i := 0; i < 2048; i++ {
			s.deliverOrHold(startupEvent{payload: []byte("x")})
		}
		if s.EventError() != nil || len(s.pendingEvents) != 2048 {
			t.Fatal("exact count not accepted")
		}
		s.deliverOrHold(startupEvent{payload: []byte("x")})
		if s.EventError() == nil || len(s.pendingEvents) != 2048 || s.pendingBytes != 2048 {
			t.Fatal("count limit not enforced")
		}
	})
	t.Run("bytes", func(t *testing.T) {
		s := &Session{}
		for i := 0; i < 4; i++ {
			s.deliverOrHold(startupEvent{payload: make([]byte, 8<<20)})
		}
		if s.EventError() != nil || s.pendingBytes != 32<<20 {
			t.Fatal("exact bytes not accepted")
		}
		s.deliverOrHold(startupEvent{payload: []byte("x")})
		if s.EventError() == nil || s.pendingBytes != 32<<20 {
			t.Fatal("byte limit not enforced")
		}
	})
}
