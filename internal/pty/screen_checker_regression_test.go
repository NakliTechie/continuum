package pty

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/term"
)

func TestCheckerPTYChild(t *testing.T) {
	mode := os.Getenv("CHECKER_PTY_MODE")
	if mode == "" {
		return
	}
	time.AfterFunc(8*time.Second, func() { os.Exit(97) })
	if _, err := term.MakeRaw(os.Stdin.Fd()); err != nil {
		os.Exit(96)
	}
	fmt.Print("READY")
	time.Sleep(150 * time.Millisecond)
	fmt.Print("\x1b[6n")
	if mode == "fault" {
		time.Sleep(3 * time.Second)
		fmt.Print("POST_FAULT")
		os.Exit(0)
	}
	want := bytes.Repeat([]byte{'u'}, 64<<10)
	got := make([]byte, len(want))
	if _, err := io.ReadFull(os.Stdin, got); err != nil || !bytes.Equal(want, got) {
		os.Exit(95)
	}
	want = []byte("\x1b[1;6R")
	got = make([]byte, len(want))
	if _, err := io.ReadFull(os.Stdin, got); err != nil || !bytes.Equal(want, got) {
		os.Exit(94)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cols, rows, err := term.GetSize(os.Stdin.Fd())
		if err == nil && cols == 90 && rows == 30 {
			fmt.Print("ATOMIC_RESIZE_OK")
			os.Exit(0)
		}
		time.Sleep(time.Millisecond)
	}
	os.Exit(93)
}

func checkerPTY(t *testing.T, mode string) (*Session, <-chan struct{}, <-chan int, *bytes.Buffer, *sync.Mutex) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestCheckerPTYChild$")
	cmd.Env = append(os.Environ(), "CHECKER_PTY_MODE="+mode)
	disabled := ""
	s, err := StartTerminal("checker", "custom", cmd, TerminalOptions{Cols: 80, Rows: 24}, &disabled)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 1)
	done := make(chan int, 1)
	var raw bytes.Buffer
	var mu sync.Mutex
	go s.Run(func(_ int, b []byte) {
		mu.Lock()
		raw.Write(b)
		mu.Unlock()
		if strings.Contains(string(b), "READY") {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
	}, func(code int) { done <- code })
	t.Cleanup(func() { s.Kill() })
	return s, ready, done, &raw, &mu
}

func TestCheckerPTYConcurrentReplyInputResize(t *testing.T) {
	s, ready, done, raw, mu := checkerPTY(t, "interleave")
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("not ready")
	}
	written := make(chan error, 1)
	go func() { written <- s.Write(bytes.Repeat([]byte{'u'}, 64<<10)) }()
	time.Sleep(250 * time.Millisecond)
	resized := make(chan error, 1)
	go func() { resized <- s.Resize(90, 30) }()
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-resized:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resize deadlocked")
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("helper exit %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("I/O deadlocked")
	}
	frame, _ := s.TerminalSnapshot()
	if frame.Fault != "" || frame.Cols != 90 || frame.Rows != 30 {
		t.Fatalf("snapshot: %+v", frame)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(raw.String(), "ATOMIC_RESIZE_OK") {
		t.Fatalf("helper marker absent: %q", raw.String())
	}
}

func TestCheckerPTYReplyDeadlineFaultDoesNotStopRawCapture(t *testing.T) {
	s, ready, done, raw, mu := checkerPTY(t, "fault")
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("not ready")
	}
	started := time.Now()
	if err := s.Write(bytes.Repeat([]byte{'u'}, 1<<20)); err == nil {
		t.Fatal("nonreading child accepted unbounded input")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("input exceeded deadline")
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("helper exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reply backpressure blocked drain")
	}
	frame, _ := s.TerminalSnapshot()
	if !strings.Contains(frame.Fault, "deadline exceeded") {
		t.Fatalf("reply failure not marked: %+v", frame)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(raw.String(), "POST_FAULT") {
		t.Fatalf("raw post-fault output lost: %q", raw.String())
	}
}

// A restarted daemon replays the holder's retained ring into a fresh screen
// engine. Bytes already committed before the crash must rebuild the screen but
// must not be emitted to the journal callback or capture file a second time.
func TestHeldTerminalReplayRebuildsWithoutRedelivery(t *testing.T) {
	ptmx, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	disabled := ""
	output := strings.NewReader("BEFORE\r\nAFTER")
	s, err := Held("replay", "custom", ptmx, 123, time.Now(), output, func() int { return 0 }, len("BEFORE\r\n"), &TerminalOptions{Cols: 20, Rows: 4}, &disabled)
	if err != nil {
		t.Fatal(err)
	}
	var delivered bytes.Buffer
	done := make(chan struct{})
	s.Run(func(_ int, b []byte) { delivered.Write(b) }, func(code int) {
		if code != 0 {
			t.Errorf("exit code %d", code)
		}
		close(done)
	})
	<-done
	if got := delivered.String(); got != "AFTER" {
		t.Fatalf("redelivered retained prefix: %q", got)
	}
	frame, ok := s.TerminalSnapshot()
	if !ok || frame.Epoch == "" || !strings.Contains(strings.Join(frame.Lines, "\n"), "BEFORE") || !strings.Contains(strings.Join(frame.Lines, "\n"), "AFTER") {
		t.Fatalf("screen was not rebuilt from retained output: %+v", frame)
	}
}
