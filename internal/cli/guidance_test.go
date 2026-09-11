package cli

import (
	"bytes"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/term"
	creackpty "github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func assertAttachPreflight(t *testing.T, s *attachSession, want int) {
	t.Helper()
	select {
	case code := <-s.done:
		if code != want {
			t.Fatalf("exit %d, %s", code, s.diag.String())
		}
	case <-time.After(time.Second):
		t.Fatal("preflight hung")
	}
	state, err := term.GetState(s.slave.Fd())
	if err != nil || !reflect.DeepEqual(state, s.before) {
		t.Fatalf("tty changed: %v", err)
	}
	flags, err := unix.FcntlInt(s.slave.Fd(), unix.F_GETFL, 0)
	if err != nil || flags != s.flags {
		t.Fatalf("flags changed: %v", err)
	}
	if s.output.String() != "" {
		t.Fatalf("entered raw view during preflight: %q", s.output.String())
	}
}
func TestAttachDumbRefusesBeforeControl(t *testing.T) {
	dir, f := newAttachFixture(t)
	t.Setenv("TERM", "dumb")
	s := startAttach(t, dir, false)
	assertAttachPreflight(t, s, 9)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 0 {
		t.Fatalf("dumb terminal sent requests: %+v", f.requests)
	}
	if !strings.Contains(s.diag.String(), clientCommand("screen", dir, attachBlock)) {
		t.Fatal(s.diag.String())
	}
}
func TestAttachOccupiedControlHintsUseAttach(t *testing.T) {
	dir, f := newAttachFixture(t)
	f.mu.Lock()
	f.controlled = true
	f.mu.Unlock()
	s := startAttach(t, dir, false)
	assertAttachPreflight(t, s, 6)
	for _, mode := range []string{"--observer", "--takeover"} {
		if !strings.Contains(s.diag.String(), clientCommand("attach", dir, attachBlock)+" "+mode) {
			t.Fatal(s.diag.String())
		}
	}
	if strings.Contains(s.diag.String(), "continuum takeover") {
		t.Fatal("misleading saved-lease command")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.requests {
		if q.Operation == "resize" || q.Operation == "input" || q.Operation == "release" {
			t.Fatalf("refusal had effect: %s", q.Operation)
		}
	}
}
func TestAttachViewerColorAndDetachHint(t *testing.T) {
	for _, noColor := range []string{"", "1"} {
		t.Run("NO_COLOR="+noColor, func(t *testing.T) {
			dir, f := newAttachFixture(t)
			t.Setenv("NO_COLOR", noColor)
			f.mu.Lock()
			f.color = true
			f.mu.Unlock()
			s := startAttach(t, dir, true)
			waitFor(t, "colored row", func() bool { return strings.Contains(s.output.String(), "ATTACH_READY") })
			colored := strings.Contains(s.output.String(), "\x1b[31m")
			if colored != (noColor == "") {
				t.Fatalf("color=%v NO_COLOR=%q", colored, noColor)
			}
			s.master.Write([]byte{0x1d})
			s.finished(t, 0)
			if !strings.Contains(s.diag.String(), clientCommand("attach", dir, attachBlock)+" --observer") {
				t.Fatal(s.diag.String())
			}
		})
	}
}
func TestAttachNarrowControlsAndTinyRefusal(t *testing.T) {
	dir, _ := newAttachFixture(t)
	s := startAttach(t, dir, true)
	waitFor(t, "initial frame", func() bool { return strings.Contains(s.output.String(), "ATTACH_READY") })
	if err := creackpty.Setsize(s.master, &creackpty.Winsize{Cols: 40, Rows: 10}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "essential cropped footer", func() bool { return strings.Contains(s.output.String(), "Ctrl-] detach • Observer • cropped") })
	// A transient shrink below the minimum shows why nothing is drawn and keeps
	// the session; growing back resumes frames, and Ctrl-] still detaches.
	if err := creackpty.Setsize(s.master, &creackpty.Winsize{Cols: 20, Rows: 6}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "too-small notice", func() bool { return strings.Contains(s.output.String(), "Terminal too small") })
	before := len(s.output.String())
	if err := creackpty.Setsize(s.master, &creackpty.Winsize{Cols: 40, Rows: 10}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "frames resume", func() bool { return strings.Contains(s.output.String()[before:], "ATTACH_READY") })
	s.master.Write([]byte{0x1d})
	s.finished(t, 0)
	if strings.Contains(s.diag.String(), "40 columns") {
		t.Fatalf("shrink ended the session: %s", s.diag.String())
	}
}
func TestAttachHelpIsScoped(t *testing.T) {
	var out, diag bytes.Buffer
	if code := Run([]string{"attach", "--help"}, strings.NewReader(""), &out, &diag); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "--takeover") || !strings.Contains(out.String(), "Ctrl-]") {
		t.Fatal(out.String())
	}
	for _, wrong := range []string{"--listen", "--origin", "--cwd", "--after"} {
		if strings.Contains(out.String()+diag.String(), wrong) {
			t.Fatalf("unrelated help %s", wrong)
		}
	}
}
func TestHintsQuoteLiteralShellArguments(t *testing.T) {
	value := "/tmp/path with 'quotes' $(printf BAD) `printf ALSO_BAD`"
	got, err := exec.Command("/bin/sh", "-c", "set -- "+shellArgument(value)+"; printf '%s' \"$1\"").Output()
	if err != nil || string(got) != value {
		t.Fatalf("quoted argument changed: %q, %v", got, err)
	}
}
