package acp

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

const supplementACPChild = `import sys,json
mode=sys.argv[1]
def send(m): print(json.dumps(m,separators=(',',':')),flush=True)
for line in sys.stdin:
 m=json.loads(line);method=m.get('method');ident=m.get('id')
 if method=='initialize': send({'jsonrpc':'2.0','id':ident,'result':{'protocolVersion':1,'agentCapabilities':{'loadSession':True}}})
 elif method=='session/new':
  send({'jsonrpc':'2.0','id':'early-permission','method':'session/request_permission','params':{'sessionId':'audit-session','options':[{'optionId':'deny','name':'Reject','kind':'reject_once'}]}})
  send({'jsonrpc':'2.0','id':ident,'result':{'sessionId':'audit-session'}})
 elif method=='session/load':
  count=int(mode)
  for n in range(count): send({'jsonrpc':'2.0','method':'session/update','params':{'sessionId':'audit-session','update':{'sessionUpdate':'agent_message_chunk','content':{'type':'text','text':str(n)+':'+('x'*(128*1024))}}}})
  send({'jsonrpc':'2.0','id':ident,'result':{}})
`

func supplementStart(t *testing.T, mode string, resume bool) *Session {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-c", supplementACPChild, mode)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	disabled := ""
	var sess *Session
	if resume {
		sess, err = ResumeContext(ctx, "supplement", "fake", t.TempDir(), cmd, "audit-session", &disabled)
	} else {
		sess, err = StartContext(ctx, "supplement", "fake", t.TempDir(), cmd, &disabled)
	}
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go sess.Run(func(int) { close(done) })
	t.Cleanup(func() {
		sess.Kill()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("fake process did not reap")
		}
	})
	return sess
}

func TestSupplementEarlyPermissionMustReachInstalledSink(t *testing.T) {
	sess := supplementStart(t, "early", false)
	var delivered atomic.Int32
	sess.SetOnPermissionRequest(func(string, json.RawMessage) { delivered.Add(1) })
	sess.SetOnUpdate(func(json.RawMessage) {})
	sess.mu.Lock()
	pending := len(sess.perms)
	sess.mu.Unlock()
	t.Logf("after handshake: pending permissions=%d delivered callbacks=%d", pending, delivered.Load())
	if pending != 1 {
		t.Fatalf("fixture permission was not accepted: %d", pending)
	}
	if delivered.Load() != 1 {
		t.Fatalf("accepted startup permission never reaches the installed callback; pending=%d delivered=%d", pending, delivered.Load())
	}
}

func TestSupplementStartupReplayBufferCharacterization(t *testing.T) {
	for _, count := range []int{32, 128} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			sess := supplementStart(t, strconv.Itoa(count), true)
			sess.updateMu.Lock()
			buffered := len(sess.pendingUpdates)
			total := 0
			for _, b := range sess.pendingUpdates {
				total += len(b)
			}
			sess.updateMu.Unlock()
			t.Logf("accepted startup replay: frames=%d buffered_frames=%d buffered_bytes=%d", count, buffered, total)
			delivered := 0
			sess.SetOnUpdate(func(b json.RawMessage) {
				var frame struct {
					Params struct {
						Update struct {
							Content struct {
								Text string `json:"text"`
							} `json:"content"`
						} `json:"update"`
					} `json:"params"`
				}
				if err := json.Unmarshal(b, &frame); err != nil {
					t.Error(err)
					return
				}
				want := strconv.Itoa(delivered) + ":"
				if len(frame.Params.Update.Content.Text) < len(want) || frame.Params.Update.Content.Text[:len(want)] != want {
					t.Errorf("replay ordering failed at frame %d", delivered)
				}
				delivered++
			})
			if delivered != count {
				t.Fatalf("accepted transcript changed: got %d frames want %d", delivered, count)
			}
		})
	}
}
