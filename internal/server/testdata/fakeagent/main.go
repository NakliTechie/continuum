// fakeagent is the test double ACP agent used by unit tests (v1.1 handoff §C2:
// unit tests must not require omp). It speaks enough of the protocol to drive
// the relay's structured-session path and is steered via env:
//
//	FAKE_PERMISSION=1   send a session/request_permission request during prompt,
//	                    hold the turn until its response arrives
//	FAKE_SLOW_MS=400    hold the prompt open this long (for cancel testing)
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type msg = map[string]any

var (
	out    *bufio.Writer
	writeM sync.Mutex
)

func send(m msg) {
	b, _ := json.Marshal(m)
	writeM.Lock()
	defer writeM.Unlock()
	out.Write(b)
	out.WriteByte('\n')
	out.Flush()
}

func reply(id any, result any) { send(msg{"jsonrpc": "2.0", "id": id, "result": result}) }

// Turn state. One turn at a time — tests are sequential per spawn.
var (
	turnMu     sync.Mutex
	turnCancel chan struct{}
	turnOnce   sync.Once
	permAck    = make(chan struct{}, 1)
)

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64*1024), 8<<20)
	out = bufio.NewWriter(os.Stdout)

	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var m msg
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		id := m["id"]
		method, _ := m["method"].(string)
		switch method {
		case "initialize":
			reply(id, msg{
				"protocolVersion": 1,
				"agentInfo":       msg{"name": "fakeagent", "title": "Fake Agent", "version": "0.0.1"},
				"authMethods":     []any{},
				"agentCapabilities": msg{
					"loadSession":        false,
					"promptCapabilities": msg{"embeddedContext": true},
				},
			})
		case "session/new":
			reply(id, msg{"sessionId": "fake-session-0001", "configOptions": []any{
				msg{"id": "model", "name": "Model", "category": "model", "type": "select", "currentValue": "fake/opus",
					"options": []any{
						msg{"value": "fake/opus", "name": "Fake Opus", "description": "the big one"},
						msg{"value": "fake/haiku", "name": "Fake Haiku", "description": "the small one"},
					}},
				msg{"id": "thought_level", "name": "Thinking", "category": "thought_level", "type": "select", "currentValue": "med",
					"options": []any{
						msg{"value": "low", "name": "Low"},
						msg{"value": "med", "name": "Medium"},
						msg{"value": "high", "name": "High"},
					}},
				msg{"id": "mode", "name": "Mode", "category": "mode", "type": "select", "currentValue": "default",
					"options": []any{
						msg{"value": "default", "name": "Default"},
						msg{"value": "plan", "name": "Plan"},
					}},
			}})
		case "session/prompt":
			sessionID := "fake-session-0001"
			if pm, ok := m["params"].(map[string]any); ok {
				if v, ok := pm["sessionId"].(string); ok && v != "" {
					sessionID = v
				}
			}
			turnMu.Lock()
			turnCancel = make(chan struct{})
			turnOnce = sync.Once{}
			local := turnCancel
			turnMu.Unlock()
			drainPermAck()
			go runTurn(sessionID, id, local)
		case "session/cancel":
			turnMu.Lock()
			cc := turnCancel
			turnMu.Unlock()
			if cc != nil {
				turnOnce.Do(func() { close(cc) })
			}
		default:
			if method == "" && id != nil {
				// A response to OUR request — the permission ask. Wake the turn.
				select {
				case permAck <- struct{}{}:
				default:
				}
			}
		}
	}
}

func drainPermAck() {
	for {
		select {
		case <-permAck:
			continue
		default:
			return
		}
	}
}

// runTurn streams a chunk and finishes the turn; the cancel channel lets a
// turn die mid-flight while the reader loop keeps serving messages.
func runTurn(sessionID string, id any, cancelled <-chan struct{}) {
	if slow := os.Getenv("FAKE_SLOW_MS"); slow != "" {
		ms, _ := strconv.Atoi(slow)
		if ms > 0 {
			select {
			case <-cancelled:
				reply(id, msg{"stopReason": "cancelled"})
				return
			case <-time.After(time.Duration(ms) * time.Millisecond):
			}
		}
	}
	if os.Getenv("FAKE_PERMISSION") == "1" {
		send(msg{"jsonrpc": "2.0", "id": int64(9001), "method": "session/request_permission",
			"params": msg{
				"sessionId": sessionID,
				"toolCall": msg{
					"toolCallId": "tc-fake-1", "title": "Edit main.py", "kind": "edit",
					"content": []any{msg{"type": "diff", "path": "main.py", "oldText": "a\n", "newText": "b\n"}},
				},
				"options": []any{
					msg{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"},
					msg{"optionId": "reject-once", "name": "Reject", "kind": "reject_once"},
				},
			}})
		select {
		case <-permAck:
		case <-cancelled:
			reply(id, msg{"stopReason": "cancelled"})
			return
		}
	}
	send(msg{"jsonrpc": "2.0", "method": "session/update", "params": msg{
		"sessionId": sessionID,
		"update": msg{
			"sessionUpdate": "agent_message_chunk",
			"content":       msg{"type": "text", "text": "SMOKE-OK"},
		},
	}})
	reply(id, msg{"stopReason": "end_turn", "usage": msg{"inputTokens": 12, "outputTokens": 3, "totalTokens": 15}})
}
