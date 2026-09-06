package shims

import (
	"errors"
	"os/exec"
)

// Generic runs a configured coding-agent CLI: exec the command, hand it the
// spawn's args, attach a PTY. Every agent except "custom" is one of these —
// per-agent behaviour beyond the executable name lives in configuration, not in
// code, so a new agent is a registry row (config.KnownAgents) and never a shim.
type Generic struct {
	// ID is the agent id the browser shows.
	ID string
	// Cmd is the executable to run (PATH lookup). Empty falls back to ID.
	Cmd string
}

func (g Generic) Name() string { return g.ID }

func (g Generic) Spawn(cwd string, args []string, env map[string]string) (*exec.Cmd, error) {
	bin := g.Cmd
	if bin == "" {
		bin = g.ID
	}
	if bin == "" {
		return nil, errors.New("agent has no command configured")
	}
	return build(bin, args, cwd, env), nil
}
