// Package tmux backs relay sessions with tmux so an agent survives the relay
// process restarting: the agent runs inside a detached `menagerie-<id>` tmux
// session (owned by the persistent tmux server), and the relay merely attaches
// a PTY to it. On restart the relay re-discovers those sessions and re-attaches.
//
// All tmux invocations are argv-based (no shell), except the agent command
// itself, which is shell-quoted into a single string tmux runs via `sh -c`.
package tmux

import (
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// NamePrefix marks the tmux sessions Menagerie owns.
const NamePrefix = "menagerie-"

// agentOption is the tmux user-option we stash the agent id in.
const agentOption = "@menagerie_agent"

// Available reports whether a tmux binary is on PATH.
func Available() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// SessionName maps a Menagerie session id to its tmux session name.
func SessionName(id string) string { return NamePrefix + id }

// IDFromName returns the Menagerie id for a `menagerie-<id>` tmux session.
func IDFromName(name string) (string, bool) {
	if strings.HasPrefix(name, NamePrefix) {
		return name[len(NamePrefix):], true
	}
	return "", false
}

// Session is a discovered tmux session Menagerie owns.
type Session struct {
	Name    string
	Agent   string
	Created time.Time
}

// Create starts a detached tmux session named `name` running argv in cwd with
// env, then tunes it to be a transparent, always-alive host for one agent:
// no status bar, no prefix key (so every keystroke reaches the agent), and it
// stays alive while unattached.
func Create(name, cwd string, env, argv []string) error {
	args := []string{"new-session", "-d", "-s", name, "-x", "200", "-y", "50"}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	for _, e := range env {
		// A relay running inside tmux would otherwise nest; drop those.
		if strings.HasPrefix(e, "TMUX=") || strings.HasPrefix(e, "TMUX_PANE=") {
			continue
		}
		args = append(args, "-e", e)
	}
	args = append(args, shellJoin(argv))
	if err := exec.Command("tmux", args...).Run(); err != nil {
		return err
	}
	// Best-effort tuning — failures here don't fail the spawn.
	setOption(name, "status", "off")
	setOption(name, "prefix", "None")
	setOption(name, "prefix2", "None")
	setOption(name, "window-size", "latest") // follow the attached client's size
	return nil
}

// SetAgent tags a session with its Menagerie agent id.
func SetAgent(name, agent string) { setOption(name, agentOption, agent) }

// AttachCmd builds the `tmux attach` command the relay runs on a PTY. Killing
// this process only detaches; the session (and agent) live on — use Kill to end it.
func AttachCmd(name string) *exec.Cmd {
	cmd := exec.Command("tmux", "attach-session", "-t", name)
	cmd.Env = append(cmd.Environ(), "TERM=xterm-256color")
	return cmd
}

// Kill ends the session and the agent inside it.
func Kill(name string) error {
	return exec.Command("tmux", "kill-session", "-t", name).Run()
}

// Exists reports whether the session is still alive.
func Exists(name string) bool {
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

// IsMenagerie reports whether this is a session Menagerie itself created.
func (s Session) IsMenagerie() bool { return strings.HasPrefix(s.Name, NamePrefix) }

// List returns every tmux session currently alive (the caller decides which to
// adopt). It lists names only (no delimiter to mangle), then queries each
// session's Menagerie agent tag + created time.
func List() []Session {
	out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		return nil // no tmux server / no sessions
	}
	var res []Session
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		s := Session{Name: name, Agent: getOption(name, agentOption)}
		if c := display(name, "#{session_created}"); c != "" {
			if sec, err := strconv.ParseInt(c, 10, 64); err == nil {
				s.Created = time.Unix(sec, 0)
			}
		}
		res = append(res, s)
	}
	return res
}

func setOption(name, key, val string) {
	_ = exec.Command("tmux", "set-option", "-t", name, key, val).Run()
}

func getOption(name, key string) string {
	out, err := exec.Command("tmux", "show-options", "-t", name, "-qv", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func display(name, format string) string {
	out, err := exec.Command("tmux", "display-message", "-t", name, "-p", format).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// shellJoin renders argv as a single POSIX-shell command string.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\r'\"\\$`&|;<>()*?[]#~=!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
