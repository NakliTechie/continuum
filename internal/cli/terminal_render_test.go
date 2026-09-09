package cli

import (
	"strings"
	"testing"

	"github.com/NakliTechie/continuum/internal/terminal"
	"github.com/charmbracelet/x/ansi"
)

func TestTerminalRowOnlyAllowsTextAndSGR(t *testing.T) {
	row := "\x1b[31mred\x1b[0m\x1b[6n\x1b]52;c;clipboard\a\x1b]8;;javascript:evil\x1b\\safe\x1b]8;;\x1b\\\r\n\t\x1b[2J"
	got := displayRow(row, 80)
	if !strings.Contains(got, "\x1b[31m") || ansi.Strip(got) != "redsafe" {
		t.Fatalf("row %q", got)
	}
	for _, bad := range []string{"\x1b[6n", "\x1b]", "\x1b[2J", "\r", "\n", "\t"} {
		if strings.Contains(got, bad) {
			t.Fatalf("forwarded %q", bad)
		}
	}
}
func TestTerminalFrameCropsWithoutResizingOrMovingCursorIntoFooter(t *testing.T) {
	frame := terminal.Snapshot{Cols: 90, Rows: 30, Cursor: terminal.Cursor{Visible: true, X: 89, Y: 29}, ANSI: []string{"abcdefgh界tail"}, Modes: terminal.Modes{CursorKeys: true, BracketedPaste: true}}
	size, err := terminalViewport(10, 4)
	if err != nil {
		t.Fatal(err)
	}
	got := renderTerminal(frame, size, "Observer • Ctrl-] detach", true)
	if strings.Contains(got, "tail") || strings.Contains(got, "\x1b[?25h") || strings.Contains(got, "\x1b[?1h") || strings.Contains(got, "\x1b[?2004h") {
		t.Fatalf("invalid cropped observer frame %q", got)
	}
	if frame.Cols != 90 || frame.Rows != 30 {
		t.Fatal("renderer changed source geometry")
	}
	if !strings.Contains(got, "\x1b[4;1H") {
		t.Fatal("footer missing")
	}
}
func TestTerminalViewportAndWideCellClipping(t *testing.T) {
	for _, dims := range [][2]int{{1, 24}, {80, 2}} {
		if _, err := terminalViewport(dims[0], dims[1]); err == nil {
			t.Fatal(dims)
		}
	}
	size, err := terminalViewport(300, 200)
	if err != nil || size.cols > 240 || size.rows > 100 || size.cols*size.rows > 19200 {
		t.Fatal(size, err)
	}
	if got := ansi.Strip(displayRow("ab界c", 3)); got != "ab" {
		t.Fatalf("split a wide cell: %q", got)
	}
}
func TestTerminalControllerModesAndCleanup(t *testing.T) {
	frame := terminal.Snapshot{Cursor: terminal.Cursor{Visible: true, Style: 2, X: 1, Y: 1}, Modes: terminal.Modes{CursorKeys: true, BracketedPaste: true}}
	got := renderTerminal(frame, viewport{80, 23}, "Control • Ctrl-] detach", false)
	for _, want := range []string{"\x1b[?1h", "\x1b[?2004h", "\x1b[2;2H", "\x1b[?25h"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q", want)
		}
	}
	for _, want := range []string{"\x1b[?1l", "\x1b[?2004l", "\x1b[?7h", "\x1b[?25h", "\x1b[?1049l"} {
		if !strings.Contains(leaveTerminal, want) {
			t.Fatalf("cleanup missing %q", want)
		}
	}
}
