package shims

import (
	"errors"
	"os/exec"
)

// Custom runs an arbitrary command supplied in spawn.args (args[0] is the
// executable, the rest are its arguments). No activity heuristics.
type Custom struct{}

func (Custom) Name() string { return "custom" }

func (Custom) Spawn(cwd string, args []string, env map[string]string) (*exec.Cmd, error) {
	if len(args) == 0 {
		return nil, errors.New("custom agent requires a command in args")
	}
	return build(args[0], args[1:], cwd, env), nil
}

func (Custom) DetectIdle(buf []byte) bool       { return false }
func (Custom) DetectNeedsInput(buf []byte) bool { return false }
