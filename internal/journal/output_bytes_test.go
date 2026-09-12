package journal

import (
	"path/filepath"
	"testing"
)

// The resume offset a holder streams from is the block's lifetime committed
// output-byte count. It must keep growing even after event retention has
// deleted the oldest output events — otherwise a long-lived block resumes too
// early after a daemon restart and re-sends bytes already in the journal.
func TestOutputBytesSurvivesEventRetention(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.MaxEvents = 8 // force retention quickly
	if err := s.AddBlock(Block{ID: "blk", Agent: "custom", State: "active", Started: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	total := 0
	for i := 0; i < 200; i++ {
		chunk := []byte("tick-line\n")
		if err := s.AppendOutput("blk", chunk, false); err != nil {
			t.Fatal(err)
		}
		total += len(chunk)
	}
	// Only the last MaxEvents events remain, but the counter is the lifetime sum.
	got, err := s.OutputBytes("blk")
	if err != nil {
		t.Fatal(err)
	}
	if got != total {
		t.Fatalf("OutputBytes = %d, want lifetime %d (retention must not lower it)", got, total)
	}
	p, err := s.Read(0, "blk")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Events) >= 200 {
		t.Fatalf("retention did not fire: %d events retained", len(p.Events))
	}
	// A second block is independent.
	_ = s.AddBlock(Block{ID: "two", State: "active", Started: "2026-01-01T00:00:01Z"})
	_ = s.AppendOutput("two", []byte("x"), false)
	if n, _ := s.OutputBytes("two"); n != 1 {
		t.Fatalf("second block counter %d", n)
	}
	if n, _ := s.OutputBytes("blk"); n != total {
		t.Fatalf("first block counter changed to %d", n)
	}
}

// Unobserved output (a downtime tail) advances the counter like any other
// output, so a subsequent resume does not replay it.
func TestUnobservedOutputAdvancesTheOffset(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.AddBlock(Block{ID: "blk", State: "active", Started: "2026-01-01T00:00:00Z"})
	_ = s.AppendOutput("blk", []byte("seen"), false)
	_ = s.AppendOutput("blk", []byte("downtime"), true)
	if n, _ := s.OutputBytes("blk"); n != len("seen")+len("downtime") {
		t.Fatalf("counter %d", n)
	}
}
