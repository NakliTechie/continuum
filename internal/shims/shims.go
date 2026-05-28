// Package shims adapts specific coding agents to the relay's PTY model.
//
// A shim builds the command to exec and provides naive activity heuristics.
// All per-agent differences live here on the relay — never in the browser
// (hard NOT #6).
package shims

import (
	"os"
	"os/exec"
	"strings"
)

// Shim builds an agent's command and detects naive activity signals.
// Interface per HANDOFF-v1.0.md §5.
type Shim interface {
	Name() string
	Spawn(cwd string, args []string, env map[string]string) (*exec.Cmd, error)
	DetectIdle(buf []byte) bool       // naive in v1.0, refined in v1.1
	DetectNeedsInput(buf []byte) bool // naive in v1.0, refined in v1.1
}

// NewRegistry returns the shims implemented in this build, keyed by agent id.
// `commands` maps an agent id to its configured executable (empty => shim
// default).
func NewRegistry(commands map[string]string) map[string]Shim {
	return map[string]Shim{
		"mini":        Mini{Cmd: commands["mini"]},
		"claude-code": ClaudeCode{Cmd: commands["claude-code"]},
		"custom":      Custom{},
	}
}

// build is the common exec.Cmd assembly for shims.
func build(name string, args []string, cwd string, env map[string]string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Dir = cwd
	cmd.Env = mergeEnv(env)
	return cmd
}

// mergeEnv inherits the relay's environment, ensures TERM is set, then applies
// caller overrides.
func mergeEnv(env map[string]string) []string {
	out := append([]string{}, os.Environ()...)
	if _, ok := env["TERM"]; !ok && !hasEnv(out, "TERM") {
		out = append(out, "TERM=xterm-256color")
	}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func hasEnv(environ []string, key string) bool {
	p := key + "="
	for _, e := range environ {
		if strings.HasPrefix(e, p) {
			return true
		}
	}
	return false
}

// endsWithPrompt is a conservative needs-input heuristic: trailing output looks
// like a prompt. False negatives are fine; false positives are annoying.
func endsWithPrompt(buf []byte) bool {
	s := strings.TrimRight(string(buf), " \t\r\n")
	if s == "" {
		return false
	}
	for _, suffix := range []string{">", "?", ":", "$", "#", "❯"} {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}
