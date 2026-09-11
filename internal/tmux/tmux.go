// Package tmux backs relay sessions with tmux so an agent survives the relay
// process restarting: the agent runs inside a detached `menagerie-<id>` tmux
// session (owned by the persistent tmux server), and the relay merely attaches
// a PTY to it. On restart the relay re-discovers those sessions and re-attaches.
//
// All tmux invocations are argv-based (no shell), except the agent command
// itself, which is shell-quoted into a single string tmux runs via `sh -c`.
package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
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
	// The environment reaches the agent through a private file its shell
	// sources and removes, never through tmux's -e flags: process arguments
	// are readable by every user on the host, and the relay's environment
	// (plus anything a client passed in spawn.env) is not.
	envFile, err := writeEnvFile(env)
	if err != nil {
		return err
	}
	args = append(args, envWrapper(envFile, argv))
	if out, err := exec.Command("tmux", args...).CombinedOutput(); err != nil {
		_ = os.Remove(envFile)
		return fmt.Errorf("tmux new-session: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Best-effort tuning — failures here don't fail the spawn.
	setOption(name, "status", "off")
	setOption(name, "prefix", "None")
	setOption(name, "prefix2", "None")
	setOption(name, "window-size", "latest") // follow the attached client's size
	return nil
}

// envName matches the variable names a POSIX shell can assign; anything else
// (tmux's -e accepted it, a sourced file cannot) is dropped.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// writeEnvFile persists env as `export NAME='value'` lines in a 0600 file.
func writeEnvFile(env []string) (string, error) {
	f, err := os.CreateTemp("", "continuum-env-")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, e := range env {
		name, value, ok := strings.Cut(e, "=")
		// A relay running inside tmux would otherwise nest; drop those.
		if !ok || name == "TMUX" || name == "TMUX_PANE" || !envName.MatchString(name) {
			continue
		}
		b.WriteString("export " + name + "=" + shellQuote(value) + "\n")
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// envWrapper renders the shell line tmux runs: source the private environment,
// remove it, then replace the shell with the agent. rm is resolved here, on the
// relay's PATH, because the sourced file may have replaced PATH with one that
// cannot find it.
func envWrapper(envFile string, argv []string) string {
	rm, err := exec.LookPath("rm")
	if err != nil {
		rm = "/bin/rm"
	}
	q := shellQuote(envFile)
	return ". " + q + "; " + shellQuote(rm) + " -f " + q + "; exec " + shellJoin(argv)
}

// target names a session exactly. Without the leading "=", tmux falls back to
// prefix and fnmatch matching and "-t dev" can address "dev2".
func target(name string) string { return "=" + name }

// SetAgent tags a session with its Menagerie agent id.
func SetAgent(name, agent string) { setOption(name, agentOption, agent) }

// AttachCmd builds the `tmux attach` command the relay runs on a PTY. Killing
// this process only detaches; the session (and agent) live on — use Kill to end it.
func AttachCmd(name string) *exec.Cmd {
	cmd := exec.Command("tmux", "attach-session", "-t", target(name))
	cmd.Env = append(cmd.Environ(), "TERM=xterm-256color")
	return cmd
}

// Kill ends the session and the agent inside it.
func Kill(name string) error {
	return exec.Command("tmux", "kill-session", "-t", target(name)).Run()
}

// Exists reports whether the session is still alive.
func Exists(name string) bool {
	return exec.Command("tmux", "has-session", "-t", target(name)).Run() == nil
}

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
	_ = exec.Command("tmux", "set-option", "-t", target(name), key, val).Run()
}

func getOption(name, key string) string {
	out, err := exec.Command("tmux", "show-options", "-t", target(name), "-qv", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func display(name, format string) string {
	out, err := exec.Command("tmux", "display-message", "-t", target(name), "-p", format).Output()
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
	if !strings.ContainsAny(s, " \t\n\r'\"\\$`&|;<>()*?[]{},^#~=!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
