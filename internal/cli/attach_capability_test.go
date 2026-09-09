package cli

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/api"
	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

func TestAttachCapabilityRefusalHasNoEffects(t *testing.T) {
	for _, missing := range []string{"terminal_screen_v1", "terminal_input_base64", "control_renewal"} {
		t.Run(missing, func(t *testing.T) {
			dir, f := newAttachFixture(t)
			f.mu.Lock()
			caps := []string{}
			for _, c := range f.capabilities {
				if c != missing {
					caps = append(caps, c)
				}
			}
			f.capabilities = caps
			f.mu.Unlock()
			s := startAttach(t, dir, false)
			select {
			case code := <-s.done:
				if code != api.Exit("unsupported") {
					t.Fatalf("exit %d: %s", code, s.diag.String())
				}
			case <-time.After(time.Second):
				t.Fatal("capability preflight did not finish")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.requests) != 1 || f.requests[0].Operation != "status" {
				t.Fatalf("preflight had effects: %+v", f.requests)
			}
			if !strings.Contains(s.diag.String(), missing) {
				t.Fatalf("missing actionable capability: %s", s.diag.String())
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
				t.Fatalf("entered screen before capability check: %q", s.output.String())
			}
		})
	}
}

func TestAttachScreenOnlyObserver(t *testing.T) {
	dir, f := newAttachFixture(t)
	f.mu.Lock()
	f.capabilities = []string{"terminal_screen_v1"}
	f.mu.Unlock()
	s := startAttach(t, dir, true)
	waitFor(t, "screen-only observer", func() bool { return strings.Contains(s.output.String(), "Observer") })
	s.master.Write([]byte{0x1d})
	s.finished(t, 0)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.requests {
		if q.Operation != "screen" {
			t.Fatalf("observer requested %s", q.Operation)
		}
	}
}
