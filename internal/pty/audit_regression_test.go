package pty

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInputToNonReadingProcessIsBounded(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "stty -icanon -echo; printf ready; exec sleep 20")
	disabled := ""
	s, err := Start("blocked-input", "custom", cmd, &disabled)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Kill()
	ready := make(chan struct{}, 1)
	done := make(chan struct{})
	go s.Run(func(_ int, b []byte) {
		if strings.Contains(string(b), "ready") {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
	}, func(int) { close(done) })
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("child not ready")
	}
	start := time.Now()
	err = s.Write([]byte(strings.Repeat("x", 1<<20)))
	if err == nil {
		t.Fatal("nonreading PTY accepted unbounded input")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("input exceeded bounded deadline")
	}
	s.Kill()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("kill did not drain process")
	}
}

func TestAuditKillDescendants(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", `trap '' HUP; sleep 10 & echo $!; wait`)
	capture := ""
	s, err := Start("audit", "custom", cmd, &capture)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-s.PID, syscall.SIGKILL)
	pidch := make(chan int, 1)
	done := make(chan struct{})
	go s.Run(func(_ int, b []byte) {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if pid > 0 {
			select {
			case pidch <- pid:
			default:
			}
		}
	}, func(_ int) { close(done) })
	var pid int
	select {
	case pid = <-pidch:
	case <-time.After(time.Second):
		t.Fatal("no child pid")
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	s.Kill()
	select {
	case <-done:
	case <-time.After(time.Second):
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("descendant %d remains alive after Kill", pid)
	}
}

// The kernel takes a uint16 winsize; unbounded values wrap (65536 becomes a
// zero-column terminal) and non-positive ones were silently ignored.
func TestResizeRejectsOutOfRangeDimensions(t *testing.T) {
	disabled := ""
	s, err := Start("resize-bounds", "custom", exec.Command("/bin/cat"), &disabled)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Kill(); s.Run(func(int, []byte) {}, func(int) {}) })
	for _, size := range [][2]int{{0, 24}, {80, 0}, {65536, 24}, {80, 65616}, {-1, 24}, {MaxDimension + 1, 1}} {
		if err := s.Resize(size[0], size[1]); err == nil {
			t.Fatalf("resize %v accepted", size)
		}
	}
	if err := s.Resize(120, 40); err != nil {
		t.Fatalf("valid resize refused: %v", err)
	}
}
