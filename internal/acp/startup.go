package acp

import (
	"encoding/json"
	"fmt"
)

const (
	maxStartupFrames          = 2048
	maxStartupBytes           = 32 << 20
	maxPendingPermissions     = 64
	maxPendingPermissionBytes = 32 << 20
)

type startupEvent struct {
	requestID string
	payload   []byte
}

// Callbacks run under updateMu and may answer permissions, but must not replace
// these sinks recursively. The shared queue preserves update/permission order.
func (s *Session) SetSinks(update func(json.RawMessage), permission func(string, json.RawMessage)) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.onUpdate, s.onPermissionRequest = update, permission
	s.flushHeldLocked()
}
func (s *Session) SetOnUpdate(f func(json.RawMessage)) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.onUpdate = f
	s.flushHeldLocked()
}
func (s *Session) SetOnPermissionRequest(f func(string, json.RawMessage)) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.onPermissionRequest = f
	s.flushHeldLocked()
}

// Seed legacy update-only fixtures without changing their replay assertions.
// Actual incoming frames populate both views together under the same lock.
func (s *Session) seedHeldLocked() {
	if len(s.pendingEvents) == 0 && len(s.pendingUpdates) > 0 {
		for _, b := range s.pendingUpdates {
			s.pendingEvents = append(s.pendingEvents, startupEvent{payload: b})
			s.pendingBytes += len(b)
		}
	}
}
func (s *Session) flushHeldLocked() {
	s.seedHeldLocked()
	for len(s.pendingEvents) > 0 {
		ev := s.pendingEvents[0]
		if ev.requestID == "" {
			if s.onUpdate == nil {
				return
			}
			s.onUpdate(ev.payload)
			s.pendingUpdates[0] = nil
			s.pendingUpdates = s.pendingUpdates[1:]
		} else if s.HasPendingPermission(ev.requestID) {
			if s.onPermissionRequest == nil {
				return
			}
			s.onPermissionRequest(ev.requestID, ev.payload)
		}
		s.pendingBytes -= len(ev.payload)
		s.pendingEvents[0] = startupEvent{}
		s.pendingEvents = s.pendingEvents[1:]
	}
}
func (s *Session) deliverOrHold(ev startupEvent) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	if s.eventErr != nil {
		return
	}
	if ev.requestID != "" && !s.HasPendingPermission(ev.requestID) {
		return
	}
	s.seedHeldLocked()
	if len(s.pendingEvents) == 0 {
		if ev.requestID == "" && s.onUpdate != nil {
			s.onUpdate(ev.payload)
			return
		}
		if ev.requestID != "" && s.onPermissionRequest != nil {
			s.onPermissionRequest(ev.requestID, ev.payload)
			return
		}
	}
	if len(s.pendingEvents) >= maxStartupFrames || s.pendingBytes+len(ev.payload) > maxStartupBytes {
		s.eventErr = fmt.Errorf("ACP startup event budget exceeded (%d frames / %d bytes)", maxStartupFrames, maxStartupBytes)
		return
	}
	s.pendingEvents = append(s.pendingEvents, ev)
	s.pendingBytes += len(ev.payload)
	if ev.requestID == "" {
		s.pendingUpdates = append(s.pendingUpdates, ev.payload)
	}
	s.flushHeldLocked()
}
func (s *Session) failEvents(err error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	if s.eventErr == nil {
		s.eventErr = err
	}
}

// EventError reports an admission failure without racing sink installation.
func (s *Session) EventError() error { s.updateMu.Lock(); defer s.updateMu.Unlock(); return s.eventErr }

// HasPendingPermissions reports whether the agent still awaits a deliberate answer.
func (s *Session) HasPendingPermissions() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.perms) > 0
}
