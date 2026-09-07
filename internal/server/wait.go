package server

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/NakliTechie/menagerie/relay-go/internal/protocol"
)

// The coordination layer's load-bearing primitive (spec §8.1): a client parks
// until a session reaches one of the states it named, and the relay resolves it
// from the lifecycle transitions it already tracks. No polling, and no waiter
// may outlive its session.
//
// Every rule here exists because it was written down as an edge case first
// (spec §8.1.1) rather than discovered in production:
//   - E1 a wait whose condition already holds resolves at once, atomically with
//     registration — a transition between "check" and "register" would be lost.
//   - E3 `unknown` resolves a wait only when the caller named it. Nothing is
//     implicit: matching is exact, against the set the caller passed.
//   - E4 session exit resolves EVERY waiter, not only those that asked for it.
//     A waiter must never outlive its session.
//   - E7/E8 timeouts are the caller's; expiry reports the state as it stands.

// defaultWaitTimeout bounds a wait that named none. There is no unbounded
// server-side wait: a leaked waiter is a slow memory leak plus a client parked
// forever, and both fail silently.
const defaultWaitTimeout = 30 * time.Minute

// maxWaitTimeout caps what a caller may ask for, same reasoning.
const maxWaitTimeout = 24 * time.Hour

type waiter struct {
	id     string
	until  map[string]bool
	cn     *conn
	sid    string
	timer  *time.Timer
	once   sync.Once
	closed bool // resolved already; guarded by the entry's statusMu
}

// resolve delivers exactly once. The WS write happens off the caller's
// goroutine: resolution runs from status-transition paths, and a slow peer must
// not stall the session's event pump.
func (w *waiter) resolve(state string, timedOut bool) {
	w.once.Do(func() {
		if w.timer != nil {
			w.timer.Stop()
		}
		go func() {
			_ = w.cn.send(protocol.Waited{
				Type:      protocol.TypeWaited,
				SessionID: w.sid,
				WaitID:    w.id,
				State:     state,
				TimedOut:  timedOut,
			})
		}()
	})
}

// validWaitStates is the lifecycle vocabulary a wait may name. `running` is
// included deliberately: waiting for work to START is as legitimate as waiting
// for it to finish.
var validWaitStates = map[string]bool{
	protocol.StatusRunning:     true,
	protocol.StatusIdle:        true,
	protocol.StatusDone:        true,
	protocol.StatusNeedsInput:  true,
	protocol.StatusStalled:     true,
	protocol.StatusRateLimited: true,
	protocol.StatusExited:      true,
	protocol.StatusUnknown:     true,
}

// armWait registers a waiter, or reports the state that already satisfies it.
// The check and the registration happen under one lock (E1): doing them apart
// drops a transition that lands in between.
func (e *sessionEntry) armWait(w *waiter) (state string, satisfied bool) {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	current := e.status
	if current == "" {
		current = protocol.StatusRunning
	}
	if w.closed { // its timer beat registration; already resolved
		return "", false
	}
	if w.until[current] {
		return current, true
	}
	e.waiters = append(e.waiters, w)
	return "", false
}

// resolveWaiters is called on every status transition. On exit it drains every
// waiter regardless of what they asked for (E4).
func (e *sessionEntry) resolveWaiters(status string) {
	e.statusMu.Lock()
	exiting := status == protocol.StatusExited
	kept := e.waiters[:0]
	var fire []*waiter
	for _, w := range e.waiters {
		if exiting || w.until[status] {
			w.closed = true
			fire = append(fire, w)
			continue
		}
		kept = append(kept, w)
	}
	e.waiters = kept
	e.statusMu.Unlock()

	for _, w := range fire {
		w.resolve(status, false)
	}
}

// expireWait resolves a waiter on its own timeout, reporting the state as it
// stands rather than a guess. It no-ops if a transition beat the timer.
func (e *sessionEntry) expireWait(w *waiter) {
	e.statusMu.Lock()
	if w.closed {
		e.statusMu.Unlock()
		return
	}
	w.closed = true
	current := e.status
	if current == "" {
		current = protocol.StatusRunning
	}
	kept := e.waiters[:0]
	for _, existing := range e.waiters {
		if existing != w {
			kept = append(kept, existing)
		}
	}
	e.waiters = kept
	e.statusMu.Unlock()

	w.resolve(current, true)
}

func (cn *conn) handleWait(raw json.RawMessage) {
	var msg protocol.Wait
	if err := json.Unmarshal(raw, &msg); err != nil {
		cn.sendError("", "bad_message", "malformed wait")
		return
	}
	e := cn.srv.authSession(msg.SessionID, msg.SessionToken)
	if e == nil {
		cn.sendError(msg.SessionID, protocol.ErrInvalidToken, "unknown session or bad token")
		return
	}
	// A wait that names nothing, or names a state that can never arrive, would
	// be indistinguishable from a hang. Refuse it instead (E3's spirit: nothing
	// implicit).
	if len(msg.Until) == 0 {
		cn.sendError(msg.SessionID, protocol.ErrBadWait, "wait must name at least one state")
		return
	}
	until := make(map[string]bool, len(msg.Until))
	for _, st := range msg.Until {
		if !validWaitStates[st] {
			cn.sendError(msg.SessionID, protocol.ErrBadWait, "not a lifecycle state: "+st)
			return
		}
		until[st] = true
	}

	timeout := time.Duration(msg.TimeoutMS) * time.Millisecond
	if msg.TimeoutMS <= 0 {
		timeout = defaultWaitTimeout
	}
	if timeout > maxWaitTimeout {
		timeout = maxWaitTimeout
	}

	w := &waiter{id: msg.WaitID, until: until, cn: cn, sid: msg.SessionID}
	// Arm the timer BEFORE registering. Setting it afterwards races a transition
	// that resolves the waiter on another goroutine while this one is still
	// writing the field. A timer that fires before registration is harmless:
	// expireWait resolves once and armWait then declines to enqueue a closed
	// waiter.
	w.timer = time.AfterFunc(timeout, func() { e.expireWait(w) })
	if state, satisfied := e.armWait(w); satisfied {
		w.resolve(state, false) // E1
	}
}
