package holder

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestMain lets the test binary stand in for the daemon executable: when
// re-executed with the holder subcommand it runs the holder and nothing else.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == Subcommand {
		os.Exit(Main(os.Stdin))
	}
	os.Exit(m.Run())
}

func launch(t *testing.T, state string, argv ...string) *Attached {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	a, err := Launch(state, exe, Spec{Block: "b" + strings.Repeat("0", 15), Agent: "custom", Path: argv[0], Argv: argv, Dir: state, Env: []string{"PATH=/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func readOutputUntil(t *testing.T, a *Attached, want string, within time.Duration) string {
	t.Helper()
	rd := bufio.NewReader(a.Output)
	var got bytes.Buffer
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := rd.Read(buf)
			if n > 0 {
				got.Write(buf[:n])
				if strings.Contains(got.String(), want) {
					close(done)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
		return got.String()
	case <-time.After(within):
		t.Fatalf("did not read %q within %v; got %q", want, within, got.String())
		return ""
	}
}

func TestHolderStreamsOutputAndReportsExit(t *testing.T) {
	state := t.TempDir()
	a := launch(t, state, "/bin/sh", "-c", "read line; echo got:$line; exit 3")
	defer a.Ptmx.Close()
	if a.Hello.PID <= 0 || a.Hello.Agent != "custom" {
		t.Fatalf("hello %+v", a.Hello)
	}
	if _, err := a.Ptmx.WriteString("ping\n"); err != nil { // input goes through the master
		t.Fatal(err)
	}
	readOutputUntil(t, a, "got:ping", 5*time.Second) // output comes over the stream
	if code := a.Wait(); code != 3 {
		t.Fatalf("exit code %d, want 3", code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Lstat(filepath.Join(Dir(state), a.Hello.Block+".sock")); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket still present after exit")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The defining property: a daemon that vanishes must not take the child with
// it, and a new daemon that resumes from an offset gets contiguous output —
// including everything produced while nothing was attached.
func TestChildSurvivesDetachAndResumesContiguously(t *testing.T) {
	state := t.TempDir()
	a := launch(t, state, "/bin/sh", "-c", "i=0; while :; do i=$((i+1)); echo tick $i; sleep 0.03; done")
	first := readOutputUntil(t, a, "tick 3", 5*time.Second)
	// The resume offset the "daemon" has committed: everything it has read.
	committed := len(first)
	pid := a.Hello.PID
	a.Close()
	a.Ptmx.Close()
	time.Sleep(400 * time.Millisecond)
	if err := unix.Kill(pid, 0); err != nil {
		t.Fatalf("child %d died with the daemon: %v", pid, err)
	}
	b, err := Adopt(filepath.Join(Dir(state), a.Hello.Block+".sock"), committed)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Ptmx.Close()
	if b.Hello.PID != pid {
		t.Fatalf("adopted pid %d, want %d", b.Hello.PID, pid)
	}
	if b.Gap <= 0 {
		t.Fatalf("downtime gap not reported: %d", b.Gap)
	}
	resumed := readOutputUntil(t, b, "tick", 5*time.Second)
	nums := ticks(first + resumed)
	for i, n := range nums {
		if n != i+1 {
			t.Fatalf("ticks not contiguous across detach at index %d: %v", i, nums[:min(len(nums), i+3)])
		}
	}
	_ = unix.Kill(-pid, unix.SIGKILL)
	b.Wait()
}

func TestExitWhileDetachedLeavesARecordWithTail(t *testing.T) {
	state := t.TempDir()
	a := launch(t, state, "/bin/sh", "-c", "sleep 0.3; echo late-words; exit 5")
	a.Close()
	a.Ptmx.Close()
	var records []Record
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		records, _ = Scan(state)
		if len(records) == 1 && records[0].Exit != nil && records[0].Socket == "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(records) != 1 || records[0].Exit == nil || records[0].Socket != "" {
		t.Fatalf("records %+v", records)
	}
	ex := records[0].Exit
	if ex.Code != 5 {
		t.Fatalf("exit code %d", ex.Code)
	}
	ring, _ := base64.StdEncoding.DecodeString(ex.Ring)
	if !strings.Contains(string(ring), "late-words") {
		t.Fatalf("downtime tail not retained: %q", ring)
	}
	Forget(state, records[0].Block)
	if left, _ := Scan(state); len(left) != 0 {
		t.Fatalf("records after Forget: %+v", left)
	}
}

func TestSpawnFailureIsReportedAndLeavesNothing(t *testing.T) {
	state := t.TempDir()
	exe, _ := os.Executable()
	_, err := Launch(state, exe, Spec{Block: "c" + strings.Repeat("0", 15), Agent: "custom", Path: "/nonexistent/binary", Argv: []string{"/nonexistent/binary"}, Dir: state})
	if err == nil || !strings.Contains(err.Error(), "nonexistent") {
		t.Fatalf("err %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if records, _ := Scan(state); len(records) != 0 {
		t.Fatalf("records after failed spawn: %+v", records)
	}
}

func ticks(s string) []int {
	var out []int
	for _, f := range strings.Fields(s) {
		n := 0
		ok := len(f) > 0
		for _, c := range f {
			if c < '0' || c > '9' {
				ok = false
				break
			}
			n = n*10 + int(c-'0')
		}
		if ok {
			out = append(out, n)
		}
	}
	return out
}
