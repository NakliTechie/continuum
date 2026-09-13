package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/journal"
)

// The modern daemon's always-on service uses its own label, distinct from the
// legacy `menagerie-relay` service. Installing or removing one never touches
// the other: a Continuum service and a Menagerie relay can coexist, and a
// cutover between them is a separate, deliberate operation.
const (
	launchdLabel = "com.naklitechie.continuum"
	systemdUnit  = "continuum.service"
)

// Kept behind two small seams so service lifecycle ordering can be proved
// without registering or stopping the user's real launchd/systemd service.
var (
	runServiceCommand = func(name string, args ...string) error {
		return exec.Command(name, args...).Run()
	}
	outputServiceCommand = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}
)

// service installs, removes, or reports the always-on Continuum daemon.
func service(dir, addr, origin, adoptRelay string, args []string, out, diag io.Writer) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "install":
		return serviceInstall(dir, addr, origin, adoptRelay, out, diag)
	case "cutover":
		return serviceCutover(dir, addr, origin, adoptRelay, out, diag)
	case "uninstall":
		return serviceUninstall(out, diag)
	case "status":
		return serviceStatus(dir, out, diag)
	default:
		fmt.Fprintln(diag, "usage: continuum service [install|cutover|uninstall|status] [--state DIR] [--listen 127.0.0.1:PORT] [--origin URL] [--adopt-relay PATH]")
		return 2
	}
}

func serviceInstall(dir, addr, origin, adoptRelay string, out, diag io.Writer) int {
	// When adopting a relay, its config supplies the port unless one was given.
	if adoptRelay != "" && portNum(portOf(addr)) <= 0 {
		if p, err := relayListen(adoptRelay); err == nil {
			addr = p
		}
	}
	// A background daemon on a random port is useless: Menagerie could never
	// reconnect after a restart. Require a concrete loopback port.
	if _, port, err := splitHostPortLoose(addr); err != nil || portNum(port) <= 0 {
		fmt.Fprintln(diag, "service install needs a fixed --listen 127.0.0.1:PORT so Menagerie can reconnect after a restart")
		return 2
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(diag, "cannot resolve --state to an absolute path")
		return 2
	}
	// Materialise state and credentials before backgrounding, so the operator
	// token exists to hand back and the first boot has nothing to create. Skip
	// it when adopting a relay (the daemon writes the adopted token on its first
	// serve) or when the state already exists — re-installing over a running
	// service must not fight it for the state lock.
	if adoptRelay == "" && !fileExists(filepath.Join(abs, "operator.token")) {
		store, err := journal.Open(abs)
		if err != nil {
			fmt.Fprintln(diag, err)
			return 5
		}
		_ = store.Close()
		if _, err := credential(abs, "operator.token"); err != nil {
			fmt.Fprintln(diag, "could not create operator credential")
			return 5
		}
		_, _ = credential(abs, "observer.token")
	} else if err := os.MkdirAll(abs, 0o700); err != nil {
		fmt.Fprintln(diag, err)
		return 5
	}

	bin, err := selfPath()
	if err != nil {
		fmt.Fprintln(diag, err)
		return 5
	}
	logPath := filepath.Join(abs, "serve.log")
	for what, p := range map[string]string{"binary": bin, "state": abs, "log": logPath, "origin": origin} {
		if strings.ContainsAny(p, "\n\r") {
			fmt.Fprintf(diag, "refusing to install: %s path contains a newline: %q\n", what, p)
			return 2
		}
	}
	argv := []string{"serve", "--state", abs, "--listen", addr}
	if origin != "" {
		argv = append(argv, "--origin", origin)
	}
	if adoptRelay != "" {
		absRelay, err := filepath.Abs(adoptRelay)
		if err != nil {
			fmt.Fprintln(diag, "cannot resolve --adopt-relay path")
			return 2
		}
		if strings.ContainsAny(absRelay, "\n\r") {
			fmt.Fprintln(diag, "refusing to install: relay path contains a newline")
			return 2
		}
		argv = append(argv, "--adopt-relay", absRelay)
	}
	switch runtime.GOOS {
	case "darwin":
		if code := installLaunchd(bin, argv, logPath, out, diag); code != 0 {
			return code
		}
	case "linux":
		if code := installSystemd(bin, argv, out, diag); code != 0 {
			return code
		}
	default:
		fmt.Fprintf(diag, "service install supports macOS and Linux; run `continuum serve` yourself on %s\n", runtime.GOOS)
		return 9
	}
	fmt.Fprintf(out, "\nThe Continuum daemon will start at login and restart if it exits.\nAPI socket: %s\nMenagerie relay: ws://%s (operator token: %s)\n", filepath.Join(abs, "v1.sock"), addr, filepath.Join(abs, "operator.token"))
	return 0
}

func installLaunchd(bin string, argv []string, logPath string, out, diag io.Writer) int {
	home, _ := os.UserHomeDir()
	plistDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(plistDir, 0o755); err != nil {
		fmt.Fprintln(diag, err)
		return 5
	}
	plistPath := filepath.Join(plistDir, launchdLabel+".plist")
	plist := launchdPlist(bin, argv, logPath, servicePath())
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		fmt.Fprintln(diag, err)
		return 5
	}
	fmt.Fprintf(out, "Wrote %s\n", plistPath)
	if dryRun() {
		fmt.Fprintln(out, "(dry run: not loaded)")
		return 0
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if code := stopLaunchdForHandoff(domain, diag); code != 0 {
		return code
	}
	if err := runServiceCommand("launchctl", "enable", domain+"/"+launchdLabel); err != nil {
		fmt.Fprintln(diag, "could not enable the Continuum launchd agent")
		return 5
	}
	if err := runServiceCommand("launchctl", "bootstrap", domain, plistPath); err != nil {
		fmt.Fprintf(diag, "could not load the service automatically: %v\n", err)
		return 5
	}
	fmt.Fprintln(out, "Loaded launchd agent", launchdLabel)
	return 0
}

// launchd's ordinary bootout sends SIGTERM. `continuum serve` deliberately
// interprets SIGTERM like foreground Ctrl-C and stops its work, so an update
// first kills only the supervised daemon process. Holder processes live in
// separate sessions and are adopted by the replacement daemon. The job is
// disabled around the kill/bootout so KeepAlive cannot race the replacement.
func stopLaunchdForHandoff(domain string, diag io.Writer) int {
	target := domain + "/" + launchdLabel
	if err := runServiceCommand("launchctl", "print", target); err != nil {
		return 0 // no loaded job: this is a fresh install
	}
	// Disable first so KeepAlive cannot race the bootout by starting another
	// old daemon after the daemon-only kill.
	if err := runServiceCommand("launchctl", "disable", target); err != nil {
		fmt.Fprintln(diag, "could not disable the previous Continuum launchd agent")
		return 5
	}
	if err := runServiceCommand("launchctl", "kill", "SIGKILL", target); err != nil {
		_ = runServiceCommand("launchctl", "enable", target)
		fmt.Fprintln(diag, "could not stop only the Continuum daemon; refusing a service reload that could stop its blocks")
		return 5
	}
	if err := runServiceCommand("launchctl", "bootout", target); err != nil {
		_ = runServiceCommand("launchctl", "enable", target)
		fmt.Fprintln(diag, "could not unload the previous Continuum launchd agent")
		return 5
	}
	return 0
}

func installSystemd(bin string, argv []string, out, diag io.Writer) int {
	home, _ := os.UserHomeDir()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		fmt.Fprintln(diag, err)
		return 5
	}
	unitPath := filepath.Join(unitDir, systemdUnit)
	unit := systemdUnitFile(bin, argv, servicePath())
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		fmt.Fprintln(diag, err)
		return 5
	}
	fmt.Fprintf(out, "Wrote %s\n", unitPath)
	if dryRun() {
		fmt.Fprintln(out, "(dry run: not enabled)")
		return 0
	}
	_ = runServiceCommand("systemctl", "--user", "daemon-reload")
	if err := runServiceCommand("systemctl", "--user", "enable", systemdUnit); err != nil {
		fmt.Fprintf(out, "Could not enable the service automatically (%v).\nEnable it yourself with:\n  systemctl --user enable %s\n", err, systemdUnit)
		return 0
	}
	// Always restart after daemon-reload. `enable --now` leaves an already
	// active process running the old executable, which is not an upgrade.
	// KillSignal=SIGKILL + KillMode=process replaces only the daemon; holders
	// and their PTY children remain in the cgroup for the new daemon to adopt.
	if err := runServiceCommand("systemctl", "--user", "restart", systemdUnit); err != nil {
		fmt.Fprintf(out, "Could not start/restart the service automatically (%v).\nStart it yourself with:\n  systemctl --user restart %s\n", err, systemdUnit)
		return 0
	}
	fmt.Fprintln(out, "Enabled systemd --user unit", systemdUnit)
	fmt.Fprintln(out, "Tip: to keep it running after logout / across reboots on a headless box:\n  loginctl enable-linger \"$USER\"")
	return 0
}

func serviceUninstall(out, diag io.Writer) int {
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		if code := stopLaunchdForHandoff(domain, diag); code != 0 {
			return code
		}
		_ = runServiceCommand("launchctl", "unload", plistPath)
		if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(diag, err)
			return 5
		}
		fmt.Fprintln(out, "Removed launchd agent", launchdLabel)
	case "linux":
		home, _ := os.UserHomeDir()
		unitPath := filepath.Join(home, ".config", "systemd", "user", systemdUnit)
		_ = runServiceCommand("systemctl", "--user", "disable", "--now", systemdUnit)
		if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(diag, err)
			return 5
		}
		_ = runServiceCommand("systemctl", "--user", "daemon-reload")
		fmt.Fprintln(out, "Removed systemd unit", systemdUnit)
	default:
		fmt.Fprintln(diag, "service uninstall supports macOS and Linux")
		return 9
	}
	fmt.Fprintln(out, "The daemon's state directory and its running processes are left untouched.")
	return 0
}

func serviceStatus(dir string, out, diag io.Writer) int {
	switch runtime.GOOS {
	case "darwin":
		domain := fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel)
		if o, err := outputServiceCommand("launchctl", "print", domain); err != nil {
			fmt.Fprintln(out, "Service not loaded. Install it with `continuum service install`.")
		} else {
			fmt.Fprintln(out, "launchd agent", launchdLabel, "is loaded.")
			for _, line := range strings.Split(string(o), "\n") {
				if s := strings.TrimSpace(line); strings.HasPrefix(s, "state = ") || strings.HasPrefix(s, "pid = ") {
					fmt.Fprintln(out, "  "+s)
				}
			}
		}
	case "linux":
		o, _ := outputServiceCommand("systemctl", "--user", "is-active", systemdUnit)
		fmt.Fprintf(out, "systemd unit %s: %s", systemdUnit, string(o))
	default:
		fmt.Fprintln(diag, "service status supports macOS and Linux")
		return 9
	}
	return 0
}

// selfPath resolves this executable's real path for embedding in a unit file.
func selfPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot resolve own path: %w", err)
	}
	if abs, err := filepath.EvalSymlinks(p); err == nil {
		return abs, nil
	}
	return p, nil
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

func xmlEscape(s string) string { return xmlEscaper.Replace(s) }

// systemdQuoted renders a value for a double-quoted systemd argument: backslash
// and quote are escaped; % and $ — systemd specifiers/variables — are doubled.
func systemdQuoted(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`).Replace(s)
}

// launchdPlist renders the agent plist. Every argument is XML-escaped so a path
// containing &, <, > or quotes cannot malform the document or inject keys.
func launchdPlist(bin string, argv []string, logPath, path string) string {
	var progArgs strings.Builder
	for _, a := range append([]string{bin}, argv...) {
		progArgs.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>AbandonProcessGroup</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>%s</string></dict>
</dict>
</plist>
`, launchdLabel, progArgs.String(), xmlEscape(logPath), xmlEscape(logPath), xmlEscape(path))
}

// systemdUnitFile renders the --user unit. Every argument is systemd-quoted so
// a path with %, $, backslash or quotes is neither expanded nor able to break
// out of ExecStart.
func systemdUnitFile(bin string, argv []string, path string) string {
	execStart := `"` + systemdQuoted(bin) + `"`
	for _, a := range argv {
		execStart += ` "` + systemdQuoted(a) + `"`
	}
	return fmt.Sprintf(`[Unit]
Description=Continuum daemon
After=network.target

[Service]
ExecStart=%s
Environment="PATH=%s"
Restart=always
RestartSec=2
# The daemon's holder subprocesses keep PTY blocks alive across a restart; they
# live in this unit's cgroup, so only the main process may be signalled on stop.
KillMode=process
KillSignal=SIGKILL

[Install]
WantedBy=default.target
`, execStart, systemdQuoted(path))
}

// portNum parses a decimal port, returning 0 for anything not a positive
// integer (so ":0", ":00", "" and non-numeric ports are all rejected).
func portNum(p string) int {
	n, err := strconv.Atoi(p)
	if err != nil || n <= 0 || n > 65535 {
		return 0
	}
	return n
}

// dryRun writes the unit file but skips loading it — for previewing an install
// and for tests that must not register a real service.
func dryRun() bool { return os.Getenv("CONTINUUM_SERVICE_DRYRUN") != "" }

func portOf(addr string) string {
	_, port, err := splitHostPortLoose(addr)
	if err != nil {
		return ""
	}
	return port
}

// relayListen reads the listen address from a menagerie-relay config.
func relayListen(path string) (string, error) {
	c, err := loadRelay(path)
	if err != nil {
		return "", err
	}
	return c.Listen, nil
}

// serviceCutover replaces an installed menagerie-relay service with the modern
// Continuum daemon, adopting the relay's token, port, origins and agents so a
// client that already trusts the relay reconnects with no change. Any installed
// legacy service is stopped and its unit backed up before the new one loads.
func serviceCutover(dir, addr, origin, adoptRelay string, out, diag io.Writer) int {
	if adoptRelay == "" {
		def, err := defaultRelayPath()
		if err != nil {
			fmt.Fprintln(diag, "cutover needs --adopt-relay PATH (no default relay config found)")
			return 2
		}
		adoptRelay = def
	}
	if _, err := loadRelay(adoptRelay); err != nil {
		fmt.Fprintf(diag, "cutover: cannot read relay config %s: %v\n", adoptRelay, err)
		return 2
	}
	fmt.Fprintf(out, "Cutover: the Continuum daemon will adopt %s (its port, token, origins and agents).\n", adoptRelay)
	if code := stopLegacyRelayService(out, diag); code != 0 {
		return code
	}
	return serviceInstall(dir, addr, origin, adoptRelay, out, diag)
}

// stopLegacyRelayService stops and removes an installed menagerie-relay service
// if one exists, backing up its unit file for rollback. Absence is not an error.
// A stop that leaves the service still running aborts the cutover so two daemons
// never fight for the adopted port. Under dry run it only reports what it would do.
func stopLegacyRelayService(out, diag io.Writer) int {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		plist := filepath.Join(home, "Library", "LaunchAgents", "com.naklitechie.menagerie-relay.plist")
		if _, err := os.Stat(plist); err != nil {
			fmt.Fprintln(out, "No installed menagerie-relay launchd agent; installing the Continuum service fresh.")
			return 0
		}
		if dryRun() {
			fmt.Fprintf(out, "(dry run) would stop and back up %s\n", plist)
			return 0
		}
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		_ = runServiceCommand("launchctl", "bootout", domain+"/com.naklitechie.menagerie-relay")
		if runServiceCommand("launchctl", "print", domain+"/com.naklitechie.menagerie-relay") == nil {
			fmt.Fprintln(diag, "could not stop the menagerie-relay agent; aborting cutover (it may still hold the port). Stop it yourself and retry.")
			return 5
		}
		bak := plist + ".cutover-bak-" + time.Now().Format("2006-01-02")
		if err := os.Rename(plist, bak); err != nil {
			fmt.Fprintf(diag, "could not back up the relay agent: %v\n", err)
			return 5
		}
		fmt.Fprintf(out, "Stopped and backed up the menagerie-relay agent to %s\n", bak)
	case "linux":
		unit := filepath.Join(home, ".config", "systemd", "user", "menagerie-relay.service")
		if _, err := os.Stat(unit); err != nil {
			fmt.Fprintln(out, "No installed menagerie-relay systemd unit; installing the Continuum service fresh.")
			return 0
		}
		if dryRun() {
			fmt.Fprintf(out, "(dry run) would stop and back up %s\n", unit)
			return 0
		}
		_ = runServiceCommand("systemctl", "--user", "disable", "--now", "menagerie-relay.service")
		if o, _ := outputServiceCommand("systemctl", "--user", "is-active", "menagerie-relay.service"); strings.TrimSpace(string(o)) == "active" {
			fmt.Fprintln(diag, "could not stop the menagerie-relay unit; aborting cutover (it may still hold the port). Stop it yourself and retry.")
			return 5
		}
		bak := unit + ".cutover-bak-" + time.Now().Format("2006-01-02")
		if err := os.Rename(unit, bak); err != nil {
			fmt.Fprintf(diag, "could not back up the relay unit: %v\n", err)
			return 5
		}
		_ = runServiceCommand("systemctl", "--user", "daemon-reload")
		fmt.Fprintf(out, "Stopped and backed up the menagerie-relay unit to %s\n", bak)
	}
	return 0
}

func loadRelay(path string) (*config.Config, error) { return config.Load(path) }

// defaultRelayPath is the standard menagerie-relay config location.
func defaultRelayPath() (string, error) {
	p, err := config.DefaultPath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

// servicePath returns the PATH baked into the service unit: the install-time
// PATH (so an agent command like `claude` resolves as it does in the operator's
// shell) with common tool directories appended as a floor for a sparse
// login/launchd environment.
func servicePath() string {
	seen := map[string]bool{}
	var dirs []string
	add := func(list string) {
		for _, d := range strings.Split(list, ":") {
			if d = strings.TrimSpace(d); d != "" && !strings.ContainsAny(d, "\n\r") && !seen[d] {
				seen[d] = true
				dirs = append(dirs, d)
			}
		}
	}
	add(os.Getenv("PATH"))
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".local", "bin"))
	}
	add("/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin")
	return strings.Join(dirs, ":")
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
