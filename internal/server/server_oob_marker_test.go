package server

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/NakliTechie/menagerie/relay-go/internal/acp"
	"github.com/NakliTechie/menagerie/relay-go/internal/config"
)

// captureSender stands in for a subscriber connection's serialized writer.
type captureSender struct {
	mu     sync.Mutex
	frames [][]byte
	err    error
}

func (c *captureSender) sendRaw(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	c.frames = append(c.frames, cp)
	return nil
}

func (c *captureSender) codes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, f := range c.frames {
		var m map[string]any
		if json.Unmarshal(f, &m) == nil {
			if s, ok := m["code"].(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// The regression this closes. Measured live 2026-09-09: with the marker queued
// onto the same full outbox that caused the drop, 5000 turns produced ~20 000
// frames, the client received 3177, and ZERO markers arrived. The marker must
// reach a client whose outbox is completely full.
func TestFramesDroppedMarkerReachesAClientWhoseOutboxIsFull(t *testing.T) {
	s := New(&config.Config{})
	cap1 := make(chan []byte, 1)
	sink := &captureSender{}
	e := &sessionEntry{acp: &acp.Session{}, outbox: cap1, oob: sink}
	s.addSession(e, "sess")

	cap1 <- []byte(`{"type":"filler"}`) // outbox now full: the next send is a drop
	s.dropStructured(e, "sess")

	codes := sink.codes()
	if len(codes) != 1 || codes[0] != "frames_dropped" {
		t.Fatalf("out-of-band frames = %v, want exactly one frames_dropped", codes)
	}
	e.outMu.Lock()
	dropped := e.dropped
	e.outMu.Unlock()
	if dropped != 1 {
		t.Errorf("dropped counter = %d, want 1", dropped)
	}
}

// A client already too slow to drain the queue must not be handed thousands of
// extra frames telling it so.
func TestSustainedBackpressureEmitsOneMarkerPerEpisode(t *testing.T) {
	s := New(&config.Config{})
	cap1 := make(chan []byte, 1)
	sink := &captureSender{}
	e := &sessionEntry{acp: &acp.Session{}, outbox: cap1, oob: sink}
	s.addSession(e, "sess")

	cap1 <- []byte(`{"type":"filler"}`)
	for i := 0; i < 500; i++ {
		s.dropStructured(e, "sess")
	}
	if got := len(sink.codes()); got != 1 {
		t.Fatalf("markers during one episode = %d, want 1", got)
	}

	// The queue drains and a frame gets through: the episode is over.
	<-cap1
	if sent, _ := e.trySend([]byte(`{"type":"recovered"}`)); !sent {
		t.Fatal("expected the frame to queue once there was room")
	}
	<-cap1
	cap1 <- []byte(`{"type":"filler-again"}`)
	s.dropStructured(e, "sess")
	if got := len(sink.codes()); got != 2 {
		t.Fatalf("markers after a second episode = %d, want 2", got)
	}
	e.outMu.Lock()
	dropped := e.dropped
	e.outMu.Unlock()
	if dropped != 501 {
		t.Errorf("dropped counter = %d, want 501 — every lost frame still counts", dropped)
	}
}

// With no subscriber and no override, the marker falls back to the queue rather
// than vanishing — best-effort, but never worse than before this change.
func TestMarkerFallsBackToTheQueueWithNoSubscriber(t *testing.T) {
	s := New(&config.Config{})
	out := make(chan []byte, 4)
	e := &sessionEntry{acp: &acp.Session{}, outbox: out}
	s.addSession(e, "sess")
	s.dropStructured(e, "sess")
	select {
	case b := <-out:
		var m map[string]any
		if json.Unmarshal(b, &m); m["code"] != "frames_dropped" {
			t.Fatalf("queued frame = %v, want a frames_dropped marker", m)
		}
	default:
		t.Fatal("no marker queued and no subscriber to receive one — the drop was silent")
	}
}

// The episode cap must hold on the fallback path too: queueing the marker is not
// the client draining its backlog, so it must not re-arm the notifier.
func TestQueueFallbackDoesNotReArmTheEpisodeFlag(t *testing.T) {
	s := New(&config.Config{})
	out := make(chan []byte, 8)
	e := &sessionEntry{acp: &acp.Session{}, outbox: out}
	s.addSession(e, "sess")

	for i := 0; i < 5; i++ {
		s.dropStructured(e, "sess")
	}
	var markers int
	for len(out) > 0 {
		var m map[string]any
		if json.Unmarshal(<-out, &m); m["code"] == "frames_dropped" {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("markers queued during one episode = %d, want 1", markers)
	}
}
