package server

import (
	"encoding/json"
	"testing"

	"github.com/NakliTechie/menagerie/relay-go/internal/acp"
	"github.com/NakliTechie/menagerie/relay-go/internal/config"
)

// relay-L2: a child_spawned event that an ACP parent's outbox cannot take is
// counted as a dropped structured frame, not silently lost. The marker itself
// is queued through the same bounded outbox, so it lands once the pump drains.
func TestChildSpawnedDropUnderBackpressureIsCounted(t *testing.T) {
	s := New(&config.Config{})
	e := &sessionEntry{acp: &acp.Session{}, outbox: make(chan []byte, 1)}
	s.addSession(e, "parent")
	e.outbox <- []byte(`{"type":"filler"}`) // outbox full: the next trySend is a backpressure drop

	s.emitChildSpawned("parent", "child")

	e.outMu.Lock()
	dropped := e.dropped
	e.outMu.Unlock()
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}

	// The marker was queued into the same full outbox, so it was dropped too — the
	// counter is the durable record. Once there is room, delivery resumes.
	<-e.outbox
	s.emitChildSpawned("parent", "child2")
	var got map[string]any
	if err := json.Unmarshal(<-e.outbox, &got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "event" || got["event"] != "child_spawned" || got["child_session_id"] != "child2" {
		t.Fatalf("frame after drain = %v, want child_spawned for child2", got)
	}
}
