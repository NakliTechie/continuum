package acp

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"
)

const o5StrictCancelAgent = `
import json,sys
need_permission=sys.argv[1]=='with_permission'
turn=0
pending_prompt=None
pending_permission=False
cancel_requested=False
def send(q): print(json.dumps(q),flush=True)
def finish(reason):
 global pending_prompt
 send({'jsonrpc':'2.0','id':pending_prompt,'result':{'stopReason':reason}})
 pending_prompt=None
for line in sys.stdin:
 q=json.loads(line);method=q.get('method')
 if method=='initialize':send({'jsonrpc':'2.0','id':q['id'],'result':{'protocolVersion':1,'agentCapabilities':{}}})
 elif method=='session/new':send({'jsonrpc':'2.0','id':q['id'],'result':{'sessionId':'o5-cancel-session'}})
 elif method=='session/prompt':
  turn+=1;pending_prompt=q['id']
  if turn>1:finish('end_turn')
  elif need_permission:
   pending_permission=True
   send({'jsonrpc':'2.0','id':'cancel-permission-1','method':'session/request_permission','params':{'sessionId':'o5-cancel-session','toolCall':{'toolCallId':'cancel-call-1'},'options':[{'optionId':'allow','name':'Allow','kind':'allow_once'}]}})
  else:send({'jsonrpc':'2.0','method':'session/update','params':{'sessionId':'o5-cancel-session','update':{'sessionUpdate':'agent_message_chunk','content':{'type':'text','text':'ready'}}}})
 elif method=='session/cancel':
  cancel_requested=True
  if not pending_permission:finish('cancelled')
 elif q.get('id')=='cancel-permission-1':
  if q.get('result',{}).get('outcome',{}).get('outcome')=='cancelled':
   pending_permission=False
   if cancel_requested:finish('cancelled')
  else:send({'jsonrpc':'2.0','id':pending_prompt,'error':{'code':-32602,'message':'cancelled outcome required'}})
`

func TestO5CancelDischargesPermissionsBeforeFreshPrompt(t *testing.T) {
	for _, mode := range []string{"without_permission", "with_permission"} {
		t.Run(mode, func(t *testing.T) {
			disabled := ""
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			s, err := StartContext(ctx, "o5-cancel", "strict-fake", t.TempDir(), exec.Command("python3", "-u", "-c", o5StrictCancelAgent, mode), &disabled)
			if err != nil {
				t.Fatal(err)
			}
			finished := make(chan struct{})
			go s.Run(func(int) { close(finished) })
			t.Cleanup(func() {
				s.Kill()
				select {
				case <-finished:
				case <-time.After(3 * time.Second):
					t.Error("cleanup deadline")
				}
			})
			ready := make(chan struct{}, 1)
			s.SetSinks(func(json.RawMessage) {
				select {
				case ready <- struct{}{}:
				default:
				}
			}, func(string, json.RawMessage) {
				select {
				case ready <- struct{}{}:
				default:
				}
			})
			first, err := s.Prompt("first")
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-ready:
			case <-time.After(time.Second):
				t.Fatal("initial turn readiness deadline")
			}
			if err := s.Cancel(); err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-first:
				if r == nil || r.Error != nil {
					t.Fatalf("invalid cancel outcome: %#v", r)
				}
				t.Logf("cancel returned %s", r.Result)
			case <-time.After(500 * time.Millisecond):
				t.Errorf("session/cancel left strict peer waiting for its permission response; pending=%v", s.HasPendingPermissions())
				// A reference reply establishes that the local peer is awaiting the ACP
				// response, rather than testing a fake that cannot finish a cancellation.
				if err := s.write(Envelope{JSONRPC: "2.0", ID: json.RawMessage(`"cancel-permission-1"`), Result: json.RawMessage(`{"outcome":{"outcome":"cancelled"}}`)}); err != nil {
					t.Fatal(err)
				}
				select {
				case r := <-first:
					if r == nil || r.Error != nil {
						t.Fatalf("reference cancel failed %#v", r)
					}
					t.Logf("reference cancelled response released strict peer: %s", r.Result)
				case <-time.After(time.Second):
					t.Fatal("reference cancelled response did not release peer")
				}
			}
			if s.HasPendingPermissions() {
				t.Fatal("cancelled turn leaves a pending permission; server blockedGuard refuses every fresh prompt")
			}
			second, err := s.Prompt("fresh")
			if err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-second:
				if r == nil || r.Error != nil {
					t.Fatalf("fresh prompt outcome %#v", r)
				}
				t.Logf("fresh prompt returned %s", r.Result)
			case <-time.After(time.Second):
				t.Fatal("fresh prompt blocked")
			}
		})
	}
}
