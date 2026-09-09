package server

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/NakliTechie/continuum/internal/acp"
	"github.com/NakliTechie/continuum/internal/protocol"
)

// One ACP turn owns its completion and any wait submitted with it. A second
// prompt cannot borrow an earlier turn's status or race its response handler.
func (cn *conn) handlePrompt(raw json.RawMessage) {
	var msg protocol.Prompt
	if err := json.Unmarshal(raw, &msg); err != nil {
		cn.sendError(msg.SessionID, "bad_message", "malformed prompt")
		return
	}
	e := cn.srv.authSession(msg.SessionID, msg.SessionToken)
	if e == nil {
		cn.sendError(msg.SessionID, protocol.ErrInvalidToken, "unknown session or bad token")
		return
	}
	if e.acp == nil {
		cn.sendError(msg.SessionID, "bad_message", "prompt applies to structured sessions only")
		return
	}
	e.promptMu.Lock()
	defer e.promptMu.Unlock()
	if blockedGuard(e) {
		cn.sendError(msg.SessionID, protocol.ErrSessionBlocked, "session is waiting on a decision; answer it deliberately instead")
		return
	}
	if e.promptActive {
		cn.sendError(msg.SessionID, "prompt_busy", "the previous prompt is still active; wait, cancel, or stop it before submitting another")
		return
	}
	if e.currentStatus() == protocol.StatusExited {
		cn.sendError(msg.SessionID, "session_exited", "session exited before prompt admission")
		return
	}
	var w *waiter
	if msg.Wait != nil {
		until, bad := parseUntil(msg.Wait.Until)
		if bad != "" {
			cn.sendError(msg.SessionID, protocol.ErrBadWait, bad)
			return
		}
		w = &waiter{id: msg.Wait.WaitID, until: until, cn: cn, sid: msg.SessionID}
	}
	// Reset and register atomically. Standalone waits keep their normal
	// already-satisfied semantics; this wait observes only its new generation.
	e.statusMu.Lock()
	e.turn++
	turn := e.turn
	e.status = protocol.StatusRunning
	e.statusN++
	if w != nil {
		w.turn = turn
		w.timer = time.AfterFunc(waitTimeout(msg.Wait.TimeoutMS), func() { e.expireWait(w) })
		e.waiters = append(e.waiters, w)
	}
	e.statusMu.Unlock()
	e.promptActive = true
	e.resolveWaiters(protocol.StatusRunning)
	cn.srv.queueStructuredEvent(e, msg.SessionID, protocol.EventRunning, nil)
	ch, err := e.acp.Prompt(msg.Text)
	if err != nil {
		cn.srv.failPrompt(e, msg.SessionID, turn, w, err, nil)
		e.promptActive = false
		return
	}
	done := make(chan struct{})
	e.promptDone = done
	go func() { defer close(done); cn.srv.completePrompt(e, msg.SessionID, turn, w, ch) }()
}

func (s *Server) completePrompt(e *sessionEntry, id string, turn uint64, w *waiter, ch <-chan *acp.Envelope) {
	resp := <-ch
	e.promptMu.Lock()
	defer e.promptMu.Unlock()
	if !e.promptActive || s.entry(id) != e || e.currentStatus() == protocol.StatusExited {
		return
	}
	defer func() { e.promptActive = false }()
	var r struct {
		StopReason string          `json:"stopReason"`
		Usage      json.RawMessage `json:"usage"`
	}
	var err error
	var rpcCode *int
	switch {
	case resp == nil:
		err = fmt.Errorf("agent exited without a prompt response")
	case resp.Error != nil:
		err = resp.Error
		code := resp.Error.Code
		rpcCode = &code
	case len(resp.Result) == 0:
		err = fmt.Errorf("agent returned no prompt result")
	default:
		if decodeErr := json.Unmarshal(resp.Result, &r); decodeErr != nil || r.StopReason == "" {
			err = fmt.Errorf("agent returned an invalid prompt result")
		}
	}
	if err != nil {
		s.failPrompt(e, id, turn, w, err, rpcCode)
		return
	}
	if len(r.Usage) > 0 && string(r.Usage) != "null" {
		frame := turnUsageUpdate(id, r.Usage)
		e.outMu.Lock()
		e.lastTurnUsage = frame
		e.outMu.Unlock()
		s.deliverStructured(id, frame)
	}
	s.queueStructuredEvent(e, id, protocol.EventDone, nil)
}

func (s *Server) failPrompt(e *sessionEntry, id string, turn uint64, w *waiter, cause error, rpcCode *int) {
	message := cause.Error()
	if len(message) > 4096 {
		message = message[:4096] + " (message truncated)"
	}
	failure := protocol.NewError(id, "acp_prompt_failed", message)
	failure.TurnID, failure.RPCCode = turn, rpcCode
	s.record(id, "acp_prompt_failed", failure)
	b, _ := json.Marshal(failure)
	e.appendTail(b)
	if sent, closed := e.trySend(b); !sent && !closed {
		s.dropStructured(e, id)
	}
	// A failed prompt terminates its own outstanding wait with a correlated
	// error, never a successful done/idle. Other waits still match exactly.
	if w != nil {
		e.statusMu.Lock()
		pending := !w.closed
		if pending {
			w.closed = true
			kept := e.waiters[:0]
			for _, v := range e.waiters {
				if v != w {
					kept = append(kept, v)
				}
			}
			e.waiters = kept
		}
		e.statusMu.Unlock()
		if pending {
			w.once.Do(func() {
				if w.timer != nil {
					w.timer.Stop()
				}
				wf := failure
				wf.Code = "prompt_wait_failed"
				wf.WaitID = w.id
				go func() { _ = w.cn.send(wf) }()
			})
		}
	}
	s.queueStructuredEvent(e, id, protocol.EventUnknown, nil)
}
