package server

import (
	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/pty"
	"github.com/NakliTechie/continuum/internal/terminal"
	"time"
)

type screenResult struct {
	Block           string            `json:"block_id"`
	Host            string            `json:"host_id"`
	State           string            `json:"state"`
	ExitCode        *int              `json:"exit_code,omitempty"`
	Frame           terminal.Snapshot `json:"frame"`
	ObservedAt      string            `json:"observed_at"`
	CaptureDegraded bool              `json:"capture_degraded"`
}

func (m *Modern) activeScreens() int {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	n := 0
	for _, e := range m.s.sessions {
		if e.sess != nil && e.sess.HasTerminal() {
			n++
		}
	}
	return n
}

// Retain only the last 16 exited screens, without keeping engines/processes.
// Raw retained events remain available independently through the journal.
func (m *Modern) retireScreen(id string, sess *pty.Session, code int) {
	frame, ok := sess.TerminalSnapshot()
	if !ok {
		return
	}
	if err := m.Store.ReplaceVisibleScreen(id, frame, renderRecordedScreen(frame)); err != nil {
		m.degraded.Store(true)
	}
	m.retiredMu.Lock()
	defer m.retiredMu.Unlock()
	m.retired[id] = screenResult{Block: id, Host: m.Store.Host, State: "exited", ExitCode: &code, Frame: frame}
	m.retiredOrder = append(m.retiredOrder, id)
	if len(m.retiredOrder) > 16 {
		delete(m.retired, m.retiredOrder[0])
		m.retiredOrder = m.retiredOrder[1:]
	}
}
func (m *Modern) screen(q api.Request) api.Response {
	if q.Block == "" {
		return api.Error(q.RequestID, "invalid_request", "block_id", "screen requires block_id", "status")
	}
	// An exit snapshot wins over a briefly still-present process entry.
	m.retiredMu.Lock()
	frame, done := m.retired[q.Block]
	m.retiredMu.Unlock()
	committed := false
	if !done {
		e := m.s.entry(q.Block)
		if e == nil {
			// Retirement is published before registry removal. A request may have
			// missed that publication before waiting for the registry lock.
			m.retiredMu.Lock()
			frame, done = m.retired[q.Block]
			m.retiredMu.Unlock()
			if !done {
				var snapshot terminal.Snapshot
				found, err := m.Store.RecordedScreen(q.Block, &snapshot)
				if err != nil {
					return api.Error(q.RequestID, "resource_exhausted", "store_read", "cannot read recorded screen", "status")
				}
				if !found {
					return api.Error(q.RequestID, "conflict", "screen_unavailable", "no live or retained screen; inspect recorded events", "events")
				}
				block, err := m.Store.Block(q.Block)
				if err != nil {
					return api.Error(q.RequestID, "conflict", "screen_unavailable", "recorded block is unavailable", "status")
				}
				frame = screenResult{Block: q.Block, Host: m.Store.Host, State: block.State, Frame: snapshot}
				committed = true
			}
		} else {
			sess := m.s.entrySess(e)
			if sess == nil || !sess.HasTerminal() {
				return api.Error(q.RequestID, "unsupported", "terminal_profile_required", "block was not opened with terminal screen-v1", "help")
			}
			snapshot, _ := sess.TerminalSnapshot()
			frame = screenResult{Block: q.Block, Host: m.Store.Host, State: "active", Frame: snapshot}
		}
	}
	frame.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	frame.CaptureDegraded = m.degraded.Load()
	v := api.Result(q.RequestID, frame)
	if committed {
		v.Durability = "committed"
	} else {
		v.Durability = "volatile"
	}
	if frame.Frame.Fault != "" {
		v.Class = "indeterminate"
		v.Code = "terminal_fault"
		v.Message = "terminal screen is stale after an engine or reply failure; raw recording is independent"
		v.Next = api.Action{Kind: "command", Operation: "events"}
	}
	return v
}
