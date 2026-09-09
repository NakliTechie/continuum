package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A child of this test executable needs no Python or installed agent.
func TestLifecycleAgent(t *testing.T) {
	mode := os.Getenv("CONTINUUM_TEST_AGENT")
	if mode == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var q Envelope
		_ = json.Unmarshal(scanner.Bytes(), &q)
		if mode == "stall" {
			time.Sleep(30 * time.Second)
			os.Exit(0)
		}
		switch q.Method {
		case "initialize":
			_ = json.NewEncoder(os.Stdout).Encode(Envelope{JSONRPC: "2.0", ID: q.ID, Result: json.RawMessage(`{"protocolVersion":1}`)})
		case "session/new":
			_ = json.NewEncoder(os.Stdout).Encode(Envelope{JSONRPC: "2.0", ID: q.ID, Result: json.RawMessage(`{"sessionId":"test"}`)})
		case "session/prompt":
			for _, marker := range []string{"one", "two"} {
				_ = json.NewEncoder(os.Stdout).Encode(Envelope{JSONRPC: "2.0", Method: "session/update", Params: mustJSON(map[string]string{"marker": marker})})
			}
			_ = os.WriteFile(os.Getenv("CONTINUUM_TEST_EMITTED"), []byte("done"), 0600)
			os.Exit(0)
		}
	}
	os.Exit(0)
}
func lifecycleCommand(mode, marker string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestLifecycleAgent$")
	cmd.Env = append(os.Environ(), "CONTINUUM_TEST_AGENT="+mode, "CONTINUUM_TEST_EMITTED="+marker)
	return cmd
}
func TestLifecycleCancelledHandshake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmd := lifecycleCommand("stall", "")
	disabled := ""
	started := time.Now()
	_, err := StartContext(ctx, "cancelled", "fake", t.TempDir(), cmd, &disabled)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("cancelled handshake did not reap promptly")
	}
	if cmd.ProcessState == nil {
		t.Fatal("cancelled child not reaped")
	}
}
func TestLifecycleFinalFramesDrain(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "emitted")
	disabled := ""
	s, err := Start("drain", "fake", dir, lifecycleCommand("emit", marker), &disabled)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Kill()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var mu sync.Mutex
	count := 0
	s.SetOnUpdate(func(json.RawMessage) {
		mu.Lock()
		count++
		n := count
		mu.Unlock()
		if n == 1 {
			close(entered)
			<-release
		}
	})
	go func() { s.Run(func(int) { close(done) }) }()
	if _, err := s.Prompt("emit"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first update missing")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never emitted both frames")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("exit overtook undelivered output")
	case <-time.After(100 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if count != 2 || s.ReadError() != nil {
		t.Fatalf("captured %d/2 frames, error %v", count, s.ReadError())
	}
}
