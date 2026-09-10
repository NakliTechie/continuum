package acp

import (
	"encoding/json"
	"sort"
)

// ACP v1 RequestPermissionResponse is an outcome union. Browser outcomes are
// mapped to an offered option separately; cancellation never chooses an option.
func permissionResult(kind, option string) json.RawMessage {
	type outcome struct {
		Kind     string `json:"outcome"`
		OptionID string `json:"optionId,omitempty"`
	}
	return mustJSON(struct {
		Outcome outcome `json:"outcome"`
	}{outcome{kind, option}})
}

func (s *Session) retirePermission(id string, pending *pendingPerm) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perms[id] == pending {
		delete(s.perms, id)
		s.permissionBytes -= pending.bytes
	}
}

// HasPendingPermission also lets delayed startup delivery discard a request
// that a deliberate cancellation already discharged.
func (s *Session) HasPendingPermission(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.perms[id] != nil
}

func (s *Session) cancelPermissions() error {
	s.decisionMu.Lock()
	defer s.decisionMu.Unlock()
	s.mu.Lock()
	s.cancelling = true
	ids := make([]string, 0, len(s.perms))
	for id := range s.perms {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	sort.Strings(ids)
	if err := s.write(Envelope{JSONRPC: "2.0", Method: "session/cancel", Params: mustJSON(map[string]string{"sessionId": s.ACPSessionID})}); err != nil {
		return err
	}
	for _, id := range ids {
		s.mu.Lock()
		pending := s.perms[id]
		s.mu.Unlock()
		if pending == nil {
			continue
		}
		if err := s.write(Envelope{JSONRPC: "2.0", ID: pending.rpcID, Result: permissionResult("cancelled", "")}); err != nil {
			return err
		}
		s.retirePermission(id, pending)
	}
	return nil
}
