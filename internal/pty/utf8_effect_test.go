package pty

import (
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"testing"

	creackpty "github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// SetUTF8 must actually flip IUTF8 on the terminal, and start()/Held wire it in
// — otherwise a canonical-mode line editor erases one byte of a multibyte
// character instead of the whole rune. This proves the bit is set (the wiring
// itself is exercised end to end by the server's multibyte-input test).
func TestSetUTF8FlipsIUTF8(t *testing.T) {
	ptmx, err := creackpty.Start(exec.Command("/bin/cat"))
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()

	if err := SetUTF8(ptmx); err != nil {
		t.Fatalf("SetUTF8: %v", err)
	}
	var iutf8 bool
	raw, err := ptmx.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Control(func(fd uintptr) {
		term, e := unix.IoctlGetTermios(int(fd), getTermios)
		if e != nil {
			t.Fatalf("get termios: %v", e)
		}
		iutf8 = term.Iflag&unix.IUTF8 != 0
	}); err != nil {
		t.Fatal(err)
	}
	if !iutf8 {
		t.Fatal("IUTF8 not set after SetUTF8")
	}
}

// A held/screen session created through the package's own constructors inherits
// the flag, proving the call sites are wired (not dead code).
func TestStartTerminalSetsIUTF8(t *testing.T) {
	noCapture := ""
	sess, err := StartTerminal("iutf8"+randHex(t), "custom", exec.Command("/bin/cat"), TerminalOptions{Cols: 80, Rows: 24}, &noCapture)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Kill()
	var iutf8 bool
	raw, err := sess.ptmx.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	_ = raw.Control(func(fd uintptr) {
		if term, e := unix.IoctlGetTermios(int(fd), getTermios); e == nil {
			iutf8 = term.Iflag&unix.IUTF8 != 0
		}
	})
	if !iutf8 {
		t.Fatal("StartTerminal did not set IUTF8 on the PTY")
	}
}

func randHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
