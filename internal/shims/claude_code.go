package shims

import "os/exec"

// ClaudeCode runs Claude Code.
type ClaudeCode struct {
	// Cmd overrides the executable; empty uses "claude" from PATH.
	Cmd string
}

func (ClaudeCode) Name() string { return "claude-code" }

func (c ClaudeCode) Spawn(cwd string, args []string, env map[string]string) (*exec.Cmd, error) {
	bin := c.Cmd
	if bin == "" {
		bin = "claude"
	}
	return build(bin, args, cwd, env), nil
}

func (ClaudeCode) DetectIdle(buf []byte) bool       { return false }
func (ClaudeCode) DetectNeedsInput(buf []byte) bool { return endsWithPrompt(buf) }
