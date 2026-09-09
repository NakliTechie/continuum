package cli

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/NakliTechie/continuum/internal/terminal"
	"github.com/charmbracelet/x/ansi"
)

const enterTerminal = "\x1b[?1049h\x1b[?25l\x1b[?7l\x1b[?6l\x1b[r\x1b[2J\x1b[H"
const leaveTerminal = "\x1b[0 q\x1b[0m\x1b[?1l\x1b[?2004l\x1b[?7h\x1b[?25h\x1b[?1049l"

type viewport struct{ cols, rows int }

func terminalViewport(cols, rows int) (viewport, error) {
	if cols < 2 || rows < 3 {
		return viewport{}, fmt.Errorf("attach needs a terminal at least 2 columns by 3 rows")
	}
	cols = min(cols, terminal.MaxCols)
	rows = min(rows-1, terminal.MaxRows, terminal.MaxCells/cols)
	return viewport{cols, rows}, nil
}

// displayRow accepts only printable graphemes and SGR from a screen frame. Even
// a malformed server frame cannot pass terminal queries, clipboard OSC, margins
// or application cursor controls into the user's outer terminal.
func displayRow(row string, cols int) string {
	var out strings.Builder
	state := byte(0)
	for len(row) > 0 {
		seq, width, n, next := ansi.DecodeSequence(row, state, nil)
		if n == 0 {
			break
		}
		row = row[n:]
		state = next
		if width > 0 {
			out.WriteString(strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return '\uFFFD'
				}
				return r
			}, seq))
		} else if strings.HasPrefix(seq, "\x1b[") && strings.HasSuffix(seq, "m") {
			safe := true
			for _, c := range seq[2 : len(seq)-1] {
				if !((c >= '0' && c <= '9') || c == ';' || c == ':') {
					safe = false
					break
				}
			}
			if safe {
				out.WriteString(seq)
			}
		}
	}
	return ansi.Truncate(out.String(), cols, "") + "\x1b[0m"
}
func renderTerminal(frame terminal.Snapshot, size viewport, footer string, observer bool) string {
	var b strings.Builder
	b.WriteString("\x1b[?25l\x1b[?7l\x1b[?6l\x1b[r")
	for y := 0; y < size.rows; y++ {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[0m\x1b[2K", y+1)
		if y < len(frame.ANSI) {
			b.WriteString(displayRow(frame.ANSI[y], size.cols))
		}
	}
	fmt.Fprintf(&b, "\x1b[%d;1H\x1b[0;7m", size.rows+1)
	plainFooter := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, footer)
	b.WriteString(ansi.Truncate(plainFooter, size.cols, ""))
	b.WriteString("\x1b[K\x1b[0m")
	// Only these two input modes are supported by the initial keyboard client.
	// Mouse/focus/extended keyboard negotiation stays disabled and documented.
	if !observer && frame.Modes.CursorKeys {
		b.WriteString("\x1b[?1h")
	} else {
		b.WriteString("\x1b[?1l")
	}
	if !observer && frame.Modes.BracketedPaste {
		b.WriteString("\x1b[?2004h")
	} else {
		b.WriteString("\x1b[?2004l")
	}
	if frame.Cursor.Visible && frame.Cursor.X >= 0 && frame.Cursor.X < size.cols && frame.Cursor.Y >= 0 && frame.Cursor.Y < size.rows {
		style := frame.Cursor.Style*2 + 1
		if !frame.Cursor.Blink {
			style++
		}
		if style < 1 || style > 6 {
			style = 1
		}
		fmt.Fprintf(&b, "\x1b[%d q\x1b[%d;%dH\x1b[?25h", style, frame.Cursor.Y+1, frame.Cursor.X+1)
	}
	return b.String()
}
