package cli

import (
	"fmt"
	"strings"
)

// Shell hints use literal single-quoted arguments, including embedded quotes.
// Go's %q uses double quotes and would still permit shell substitution.
func shellArgument(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
func clientCommand(operation, dir, block string) string {
	result := "continuum " + operation + " --state " + shellArgument(dir)
	if block != "" {
		result += " --block " + shellArgument(block)
	}
	return result
}
func attachViewport(cols, rows int) (viewport, error) {
	if cols < 40 || rows < 3 {
		return viewport{}, fmt.Errorf("attach needs at least 40 columns by 3 rows to show controls; enlarge the terminal or use screen --json")
	}
	return terminalViewport(cols, rows)
}

const attachHelp = `Usage: continuum attach --block ID [--state ABSOLUTE_DIR] [--observer | --takeover]

Reconnect to a running screen-v1 terminal. Ctrl-] detaches and leaves the process running.
  continuum open --terminal screen-v1 -- /bin/sh
  continuum attach --block BLOCK_ID

Options (after the subcommand):
  --block ID           full block ID returned by open or status
  --state DIRECTORY    private absolute state directory used by serve
  --observer           watch without taking control, typing or resizing
  --takeover           explicitly replace the current controller
  -h, --help           show this help

Attach requires TTY stdin/stdout, cursor controls, and at least 40 columns by 3 rows.
TERM=dumb: use screen or events. NO_COLOR: render application rows in monochrome.
Use screen --json for pipes. Full TUI/Unicode compatibility remains experimental.
`
