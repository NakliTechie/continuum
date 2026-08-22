// Package shims adapts specific coding agents to the relay's PTY model.
//
// A shim builds the command to exec and provides naive activity heuristics.
// All per-agent differences live here on the relay — never in the browser
// (hard NOT #6).
package shims

import (
	"os"
	"os/exec"
	"regexp"
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

func lastNonEmptyLine(buf []byte) string {
	lines := strings.Split(string(buf), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return strings.ToLower(t)
		}
	}
	return ""
}

// LooksLikeNeedsInput is a generic, conservative "the agent is waiting on you"
// heuristic applied to every session (not just mini/claude). It deliberately
// ignores bare shell prompts ($ # >) — those are idle-ready, not a question —
// and only fires on explicit confirmation / choice / question prompts.
func LooksLikeNeedsInput(buf []byte) bool {
	last := lastNonEmptyLine(buf)
	if last == "" {
		return false
	}
	for _, p := range []string{
		"(y/n)", "[y/n]", "(yes/no)", "y/n?", "yes/no", "[y/n/a]", "(y/n/a)",
		"press enter", "press any key", "continue?", "proceed?", "overwrite?",
		"do you want", "are you sure", "confirm",
	} {
		if strings.Contains(last, p) {
			return true
		}
	}
	// inquirer-style selector (caret can lead the line, often after ANSI color
	// codes, so match anywhere) or a trailing question.
	return strings.Contains(last, "❯") || strings.Contains(last, "›") || strings.HasSuffix(last, "?")
}

// ansiRE matches ANSI/VT escape sequences (colors, cursor moves) so the loop
// detector compares the visible text, not the styling.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// LooksLikeStalled flags a "stuck in a loop" signature: within the recent output
// lines, one non-trivial line repeats many times — an agent re-printing the same
// error / retrying the same step. Digit runs are collapsed so a timestamped or
// counter-bearing line still matches; ANSI styling is stripped; short lines
// (prompts, spinners, dots) are ignored. Conservative: needs several repeats.
func LooksLikeStalled(recent []string) bool {
	const window, minLen, minRepeat = 24, 12, 6
	if len(recent) < minRepeat {
		return false
	}
	start := 0
	if len(recent) > window {
		start = len(recent) - window
	}
	counts := make(map[string]int)
	for _, raw := range recent[start:] {
		l := normalizeLoopLine(raw)
		if len(l) < minLen {
			continue // ignore trivial lines
		}
		counts[l]++
		if counts[l] >= minRepeat {
			return true
		}
	}
	return false
}

// normalizeLoopLine strips ANSI, trims, and collapses digit runs to "#" so that
// otherwise-identical lines differing only by a timestamp/counter still match.
func normalizeLoopLine(l string) string {
	l = strings.TrimSpace(ansiRE.ReplaceAllString(l, ""))
	var b strings.Builder
	inDigits := false
	for _, r := range l {
		if r >= '0' && r <= '9' {
			if !inDigits {
				b.WriteByte('#')
			}
			inDigits = true
			continue
		}
		inDigits = false
		b.WriteRune(r)
	}
	return b.String()
}

// LooksLikeRateLimited flags provider rate-limit / quota messages in output.
func LooksLikeRateLimited(buf []byte) bool {
	s := strings.ToLower(string(buf))
	for _, p := range []string{
		"rate limit", "rate-limit", "too many requests", "retry-after",
		"retry after", "quota exceeded", "usage limit", "overloaded",
		"you've hit your", "try again later",
	} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// MergeEnv exposes the standard environment assembly (inherit + TERM default +
// overrides) for non-PTY spawn paths that share the same rules.
func MergeEnv(env map[string]string) []string { return mergeEnv(env) }
