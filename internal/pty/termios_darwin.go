package pty

import "golang.org/x/sys/unix"

const getTermios, setTermios = unix.TIOCGETA, unix.TIOCSETA
