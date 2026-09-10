package server

import (
	"encoding/json"
	"github.com/NakliTechie/continuum/internal/protocol"
	"testing"
)

type o5MarkerCapture struct{ frames [][]byte }

func (c *o5MarkerCapture) sendRaw(b []byte) error {
	c.frames = append(c.frames, append([]byte(nil), b...))
	return nil
}

func TestO5LifecycleLossMustNotify(t *testing.T) {
	for _, event := range []string{protocol.EventRunning, protocol.EventDone, protocol.EventNeedsInput, protocol.EventExited} {
		t.Run(event, func(t *testing.T) {
			capture := &o5MarkerCapture{}
			s := &Server{}
			e := &sessionEntry{outbox: make(chan []byte, outboxCapacity), oob: capture}
			s.queueStructuredEvent(e, "sample", event, nil)
			var normal protocol.Event
			if err := json.Unmarshal(<-e.outbox, &normal); err != nil || normal.Event != event {
				t.Fatalf("positive delivery: %#v %v", normal, err)
			}
			e.outBytes = 0
			for i := 0; i < outboxCapacity; i++ {
				if sent, _ := e.trySend([]byte(`{"filler":true}`)); !sent {
					t.Fatal("fill queue")
				}
			}
			s.queueStructuredEvent(e, "sample", event, nil)
			if len(capture.frames) != 1 || e.dropped != 1 {
				t.Fatalf("lifecycle loss must notify: event=%s markers=%d dropped=%d", event, len(capture.frames), e.dropped)
			}
			var marker protocol.Error
			if err := json.Unmarshal(capture.frames[0], &marker); err != nil || marker.Code != "frames_dropped" || marker.SessionID != "sample" {
				t.Fatalf("invalid drop marker: %#v %v", marker, err)
			}
			if e.currentStatus() != event {
				t.Fatalf("delivery loss changed tracked status: %s", e.currentStatus())
			}
			e.closeOutbox()
			s.queueStructuredEvent(e, "sample", event, nil)
			if len(capture.frames) != 1 || e.dropped != 1 {
				t.Fatal("closed queue counted as live backpressure")
			}
		})
	}
}
