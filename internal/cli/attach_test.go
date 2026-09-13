package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/ipc"
	"github.com/NakliTechie/continuum/internal/terminal"
	"github.com/charmbracelet/x/term"
	creackpty "github.com/creack/pty"
	"golang.org/x/sys/unix"
)

type safeBuffer struct {
	sync.Mutex
	b bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) { b.Lock(); defer b.Unlock(); return b.b.Write(p) }
func (b *safeBuffer) String() string              { b.Lock(); defer b.Unlock(); return b.b.String() }

const attachBlock = "0123456789abcdef"

type attachFixture struct {
	mu           sync.Mutex
	requests     []api.Request
	input        []byte
	screens      int
	failInput    bool
	hangScreen   bool
	controlled   bool
	color        bool
	screenEpoch  string
	capabilities []string
}

func newAttachFixture(t *testing.T) (string, *attachFixture) {
	t.Helper()
	// This fixture emulates a capable outer terminal, independent of the test runner.
	t.Setenv("TERM", "xterm-256color")
	f := &attachFixture{screenEpoch: "epoch-one", capabilities: []string{"terminal_screen_v1", "terminal_input_base64", "control_renewal"}}
	frame := terminal.Snapshot{Engine: terminal.Name, Epoch: f.screenEpoch, Cols: 80, Rows: 23, Revision: 1, Cursor: terminal.Cursor{Visible: true, X: 5, Y: 1}, Lines: make([]string, 23), ANSI: make([]string, 23)}
	frame.Lines[0] = "ATTACH_READY"
	frame.ANSI[0] = "ATTACH_READY"
	h := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q api.Request
		if json.NewDecoder(r.Body).Decode(&q) != nil {
			t.Error("invalid API request")
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, q)
		if q.Operation == "screen" {
			f.screens++
		}
		hang := f.hangScreen && f.screens > 1 && q.Operation == "screen"
		capabilities := append([]string(nil), f.capabilities...)
		controlled, color, screenEpoch := f.controlled, f.color, f.screenEpoch
		fail := f.failInput
		f.mu.Unlock()
		if hang {
			<-r.Context().Done()
			return
		}
		token := r.Header.Get("Authorization")
		if token != "Bearer op" && token != "Bearer observer" {
			t.Errorf("unexpected credential type")
			w.WriteHeader(401)
			return
		}
		if token == "Bearer observer" && q.Operation != "screen" {
			t.Errorf("observer attempted %s", q.Operation)
		}
		result := api.Result(q.RequestID, map[string]bool{"accepted": true})
		switch q.Operation {
		case "status":
			result = api.Result("", map[string]any{"capabilities": capabilities})
		case "screen":
			current := frame
			current.Epoch = screenEpoch
			if color {
				current.ANSI = append([]string(nil), frame.ANSI...)
				current.ANSI[0] = "\x1b[31mATTACH_READY\x1b[0m"
			}
			result = api.Result("", screenView{Block: attachBlock, Host: "test-host", State: "active", Frame: current})
			result.Durability = "volatile"
		case "acquire", "takeover":
			result = api.Result(q.RequestID, map[string]string{"lease": "private-attach-lease"})
			if controlled && q.Operation == "acquire" { // takeover replaces; acquire is refused
				result = api.Error(q.RequestID, "conflict", "controlled", "control is held", "takeover")
			}
		case "input":
			if fail {
				result = api.Error(q.RequestID, "indeterminate", "transport_lost", "input outcome is unknown", "status")
			} else {
				if q.Encoding != "base64" {
					t.Error("interactive input did not use lossless bytes")
				}
				b, err := base64.StdEncoding.DecodeString(q.Data)
				if err != nil {
					t.Error(err)
				}
				f.mu.Lock()
				f.input = append(f.input, b...)
				f.mu.Unlock()
			}
		}
		if q.Operation != "status" && q.Operation != "screen" && q.Operation != "acquire" && q.Operation != "takeover" && q.Lease != "private-attach-lease" {
			t.Errorf("wrong lease for %s", q.Operation)
		}
		json.NewEncoder(w).Encode(result)
	}))
	dir := t.TempDir()
	serveOnSocket(t, h, dir)
	for name, value := range map[string]string{"operator.token": "op", "observer.token": "observer"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, f
}

type attachSession struct {
	master, slave *os.File
	output, diag  *safeBuffer
	done          chan int
	before        *term.State
	flags         int
}

func startAttach(t *testing.T, dir string, observer bool) *attachSession {
	t.Helper()
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err = creackpty.Setsize(master, &creackpty.Winsize{Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	// Use independent input/output file descriptions. Darwin marks a written
	// description with its immutable FWASWRITTEN bit; that is not an input-mode
	// setting that attach can restore. Keep the exact input flag assertion.
	outputTTY, err := os.OpenFile(slave.Name(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { outputTTY.Close() })
	state, err := term.GetState(slave.Fd())
	if err != nil {
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(slave.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := &attachSession{master: master, slave: slave, output: &safeBuffer{}, diag: &safeBuffer{}, done: make(chan int, 1), before: state, flags: flags}
	t.Cleanup(func() { master.Close(); slave.Close() })
	go io.Copy(s.output, master)
	go func() { s.done <- attach(dir, attachBlock, observer, false, slave, outputTTY, s.diag) }()
	return s
}

// startTakeover is startAttach with --takeover: the controller path that
// replaces an existing controller instead of refusing.
func startTakeover(t *testing.T, dir string) *attachSession {
	t.Helper()
	master, slave, err := creackpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err = creackpty.Setsize(master, &creackpty.Winsize{Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	outputTTY, err := os.OpenFile(slave.Name(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { outputTTY.Close() })
	state, err := term.GetState(slave.Fd())
	if err != nil {
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(slave.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := &attachSession{master: master, slave: slave, output: &safeBuffer{}, diag: &safeBuffer{}, done: make(chan int, 1), before: state, flags: flags}
	t.Cleanup(func() { master.Close(); slave.Close() })
	go io.Copy(s.output, master)
	go func() { s.done <- attach(dir, attachBlock, false, true, slave, outputTTY, s.diag) }()
	return s
}
func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout: " + what)
}
func (s *attachSession) finished(t *testing.T, want int) {
	t.Helper()
	select {
	case code := <-s.done:
		if code != want {
			t.Fatalf("code %d; diagnostic %s", code, s.diag.String())
		}
	case <-time.After(4 * time.Second):
		t.Fatal("attach did not stop")
	}
	after, err := term.GetState(s.slave.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.before, after) {
		t.Fatal("tty state not restored")
	}
	flags, err := unix.FcntlInt(s.slave.Fd(), unix.F_GETFL, 0)
	if err != nil || flags != s.flags {
		t.Fatalf("file flags not restored: before=%d after=%d error=%v", s.flags, flags, err)
	}
	waitFor(t, "alternate screen cleanup", func() bool { return strings.Contains(s.output.String(), "\x1b[?1049l") })
}
func TestAttachPTYControllerBytesDetachAndRestore(t *testing.T) {
	dir, f := newAttachFixture(t)
	s := startAttach(t, dir, false)
	waitFor(t, "controller frame", func() bool { return strings.Contains(s.output.String(), "ATTACH_READY") })
	// Split a UTF-8 rune across two writes and retain literal Ctrl-C input.
	s.master.Write([]byte{0xe7})
	time.Sleep(100 * time.Millisecond)
	s.master.Write([]byte{0x95, 0x8c, 0x03})
	waitFor(t, "lossless input", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return bytes.Equal(f.input, []byte{0xe7, 0x95, 0x8c, 0x03})
	})
	if err := creackpty.Setsize(s.master, &creackpty.Winsize{Cols: 100, Rows: 35}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "controller resize", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, q := range f.requests {
			if q.Operation == "resize" && q.Cols == 100 && q.Rows == 34 {
				return true
			}
		}
		return false
	})
	s.master.Write([]byte{0x1d})
	s.finished(t, 0)
	f.mu.Lock()
	defer f.mu.Unlock()
	release := 0
	for _, q := range f.requests {
		if q.Operation == "release" {
			release++
		}
		if q.Operation == "stop" {
			t.Fatal("detach stopped process")
		}
	}
	if release != 1 {
		t.Fatalf("release count %d", release)
	}
	if _, err := os.Stat(filepath.Join(dir, "lease-"+attachBlock)); !os.IsNotExist(err) {
		t.Fatal("attach persisted a shareable lease")
	}
}
func TestAttachPTYObserverNeverMutates(t *testing.T) {
	dir, f := newAttachFixture(t)
	s := startAttach(t, dir, true)
	waitFor(t, "observer frame", func() bool { return strings.Contains(s.output.String(), "Observer") })
	s.master.Write([]byte("ignored keys"))
	creackpty.Setsize(s.master, &creackpty.Winsize{Cols: 60, Rows: 15})
	time.Sleep(200 * time.Millisecond)
	s.master.Write([]byte{0x1d})
	s.finished(t, 0)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.requests {
		if q.Operation != "screen" {
			t.Fatalf("observer mutated: %s", q.Operation)
		}
	}
}
func TestAttachPTYUncertainInputIsNotReplayed(t *testing.T) {
	dir, f := newAttachFixture(t)
	f.failInput = true
	s := startAttach(t, dir, false)
	waitFor(t, "controller frame", func() bool { return strings.Contains(s.output.String(), "ATTACH_READY") })
	s.master.Write([]byte("one attempt"))
	s.finished(t, 8)
	if !strings.Contains(s.diag.String(), "input outcome is unknown") {
		t.Fatal(s.diag.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, q := range f.requests {
		if q.Operation == "input" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("input attempted %d times", n)
	}
}
func TestAttachPTYDetachCancelsPendingRead(t *testing.T) {
	dir, f := newAttachFixture(t)
	f.hangScreen = true
	s := startAttach(t, dir, true)
	waitFor(t, "pending screen read", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.screens > 1 })
	start := time.Now()
	s.master.Write([]byte{0x1d})
	s.finished(t, 0)
	if time.Since(start) > time.Second {
		t.Fatal("detach waited for network timeout")
	}
}

func TestAttachStopsAtScreenEpochBoundary(t *testing.T) {
	dir, f := newAttachFixture(t)
	s := startAttach(t, dir, true)
	waitFor(t, "first screen epoch", func() bool { return strings.Contains(s.output.String(), "ATTACH_READY") })
	f.mu.Lock()
	f.screenEpoch = "epoch-two"
	f.mu.Unlock()
	s.finished(t, 6)
	if !strings.Contains(s.diag.String(), "rebuilt after a daemon restart") {
		t.Fatal(s.diag.String())
	}
}
func TestAttachRejectsPipesBeforeCallingDaemon(t *testing.T) {
	var out, diag bytes.Buffer
	if code := attach(t.TempDir(), attachBlock, false, false, strings.NewReader(""), &out, &diag); code != 2 || !strings.Contains(diag.String(), "requires a terminal") {
		t.Fatal(code, diag.String())
	}
}

// serveOnSocket starts an unstarted httptest server on the state directory's
// API socket, which is the only place the CLI dials.
func serveOnSocket(t *testing.T, h *httptest.Server, dir string) {
	t.Helper()
	ln, err := ipc.Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	h.Listener.Close()
	h.Listener = ln
	h.Start()
	t.Cleanup(h.Close)
}
