package acp

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"
)

const o5StrictV1Agent = `
import json,sys
prompt_id=None
def send(value):
 print(json.dumps(value),flush=True)
for line in sys.stdin:
 q=json.loads(line)
 method=q.get('method')
 if method=='initialize':
  send({'jsonrpc':'2.0','id':q['id'],'result':{'protocolVersion':1,'agentCapabilities':{'loadSession':True}}})
 elif method=='session/new':
  send({'jsonrpc':'2.0','id':q['id'],'result':{'sessionId':'strict-v1-session'}})
 elif method=='session/prompt':
  prompt_id=q['id']
  send({'jsonrpc':'2.0','id':'permission-1','method':'session/request_permission','params':{'sessionId':'strict-v1-session','toolCall':{'toolCallId':'call-1','title':'Local test only','kind':'read','status':'pending'},'options':[{'optionId':'allow-once','name':'Allow once','kind':'allow_once'},{'optionId':'reject-once','name':'Reject','kind':'reject_once'}]}})
 elif q.get('id')=='permission-1':
  outcome=q.get('result',{}).get('outcome',{})
  if isinstance(outcome,dict) and outcome.get('outcome')=='selected' and outcome.get('optionId')=='allow-once':
   send({'jsonrpc':'2.0','id':prompt_id,'result':{'stopReason':'end_turn'}})
  else:
   send({'jsonrpc':'2.0','id':prompt_id,'error':{'code':-32602,'message':'Expected RequestPermissionResponse.outcome={outcome:selected,optionId:allow-once}; received '+json.dumps(q.get('result'))}})
`

func TestO5PermissionV1ResponseContract(t *testing.T) {
	for _, useRelay := range []bool{false, true} {
		label := "reference_v1_positive"
		if useRelay {
			label = "relay_generated_v1"
		}
		t.Run(label, func(t *testing.T) {
			disabled := ""
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			cmd := exec.Command("python3", "-u", "-c", o5StrictV1Agent)
			s, err := StartContext(ctx, "o5-probe", "fake-v1", t.TempDir(), cmd, &disabled)
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
					t.Error("child cleanup deadline")
				}
			})
			permissions := make(chan string, 1)
			s.SetSinks(func(json.RawMessage) {}, func(id string, _ json.RawMessage) { permissions <- id })
			response, err := s.Prompt("schema probe")
			if err != nil {
				t.Fatal(err)
			}
			var reqID string
			select {
			case reqID = <-permissions:
			case <-time.After(2 * time.Second):
				t.Fatal("permission deadline")
			}
			if useRelay {
				err = s.RespondPermission(reqID, "approve", "allow-once")
			} else {
				err = s.write(Envelope{JSONRPC: "2.0", ID: json.RawMessage(`"permission-1"`), Result: json.RawMessage(`{"outcome":{"outcome":"selected","optionId":"allow-once"}}`)})
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-response:
				if r == nil {
					t.Fatal("no prompt response")
				}
				if r.Error != nil {
					t.Fatalf("strict ACP v1 fake rejected response: %s", r.Error)
				}
				t.Logf("strict ACP v1 fake accepted decision and returned %s", r.Result)
			case <-time.After(2 * time.Second):
				t.Fatal("strict peer response deadline")
			}
		})
	}
}
