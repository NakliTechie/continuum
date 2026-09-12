package pty

import (
	"os"

	"golang.org/x/sys/unix"
)

// SetUTF8 marks the terminal's input as UTF-8 (IUTF8) so canonical-mode
// erase removes whole characters, matching what tmux and terminal emulators
// set for a fresh PTY. Every client of this runtime sends UTF-8.
func SetUTF8(ptmx *os.File) error {
	raw, err := ptmx.SyscallConn()
	if err != nil {
		return err
	}
	var ioErr error
	err = raw.Control(func(fd uintptr) {
		t, e := unix.IoctlGetTermios(int(fd), getTermios)
		if e != nil {
			ioErr = e
			return
		}
		t.Iflag |= unix.IUTF8
		ioErr = unix.IoctlSetTermios(int(fd), setTermios, t)
	})
	if err != nil {
		return err
	}
	return ioErr
}
