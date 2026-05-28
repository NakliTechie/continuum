package shims

import "os/exec"

// Mini runs mini-swe-agent.
type Mini struct {
	// Cmd overrides the executable; empty uses "mini" from PATH.
	Cmd string
}

func (Mini) Name() string { return "mini" }

func (m Mini) Spawn(cwd string, args []string, env map[string]string) (*exec.Cmd, error) {
	bin := m.Cmd
	if bin == "" {
		bin = "mini"
	}
	return build(bin, args, cwd, env), nil
}

func (Mini) DetectIdle(buf []byte) bool       { return false }
func (Mini) DetectNeedsInput(buf []byte) bool { return endsWithPrompt(buf) }
