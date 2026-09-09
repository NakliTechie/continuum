// A deterministic local PTY application for the screen integration contract.
package main

import (
	"fmt"
	"github.com/charmbracelet/x/term"
	"io"
	"os"
	"time"
)

func main() {
	timer := time.AfterFunc(5*time.Second, func() { os.Exit(97) })
	defer timer.Stop()
	if _, err := term.MakeRaw(os.Stdin.Fd()); err != nil {
		os.Exit(96)
	}
	cols, rows, err := term.GetSize(os.Stdin.Fd())
	if err != nil || cols != 90 || rows != 30 {
		os.Exit(95)
	}
	fmt.Print("\x1b[3;9H\x1b[6n\x1b[5n")
	want := "\x1b[3;9R\x1b[0n"
	b := make([]byte, len(want))
	if _, err := io.ReadFull(os.Stdin, b); err != nil || string(b) != want {
		os.Exit(94)
	}
	fmt.Print("QUERY_OK")
	want = "finish"
	b = make([]byte, len(want))
	if _, err := io.ReadFull(os.Stdin, b); err != nil || string(b) != want {
		os.Exit(93)
	}
	fmt.Print(" FINAL_OK")
}
