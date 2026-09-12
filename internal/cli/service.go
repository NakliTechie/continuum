package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

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

// service installs, removes, or reports the always-on Continuum daemon.
func service(dir, addr, origin string, args []string, out, diag io.Writer) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "install":
		return serviceInstall(dir, addr, origin, out, diag)
	case "uninstall":
		return serviceUninstall(out, diag)
	case "status":
		return serviceStatus(dir, out, diag)
	default:
		fmt.Fprintln(diag, "usage: continuum service [install|uninstall|status] [--state DIR] [--listen 127.0.0.1:PORT] [--origin URL]")
		return 2
	}
}

func serviceInstall(dir, addr, origin string, out, diag io.Writer) int {
	// A background daemon on a random port is useless: Menagerie could never
	// reconnect after a restart. Require a concrete loopback port.
	if _, port, err := splitHostPortLoose(addr); err != nil || port == "" || port == "0" {
		fmt.Fprintln(diag, "service install needs a fixed --listen 127.0.0.1:PORT so Menagerie can reconnect after a restart")
		return 2
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintln(diag, "cannot resolve --state to an absolute path")
		return 2
	}
	// Materialise state and credentials before backgrounding, so the operator
	// token exists to hand back and the first boot has nothing to create.
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
	plist := launchdPlist(bin, argv, logPath)
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		fmt.Fprintln(diag, err)
		return 5
	}
	fmt.Fprintf(out, "Wrote %s\n", plistPath)
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain+"/"+launchdLabel).Run() // clean reload
	if err := exec.Command("launchctl", "bootstrap", domain, plistPath).Run(); err != nil {
		_ = exec.Command("launchctl", "unload", plistPath).Run()
		if err2 := exec.Command("launchctl", "load", "-w", plistPath).Run(); err2 != nil {
			fmt.Fprintf(out, "Could not load the service automatically (%v).\nLoad it yourself with:\n  launchctl bootstrap %s %q\n", err, domain, plistPath)
			return 0
		}
	}
	fmt.Fprintln(out, "Loaded launchd agent", launchdLabel)
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
	unit := systemdUnitFile(bin, argv)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		fmt.Fprintln(diag, err)
		return 5
	}
	fmt.Fprintf(out, "Wrote %s\n", unitPath)
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	if err := exec.Command("systemctl", "--user", "enable", "--now", systemdUnit).Run(); err != nil {
		fmt.Fprintf(out, "Could not enable the service automatically (%v).\nEnable it yourself with:\n  systemctl --user enable --now %s\n", err, systemdUnit)
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
		_ = exec.Command("launchctl", "bootout", domain+"/"+launchdLabel).Run()
		_ = exec.Command("launchctl", "unload", plistPath).Run()
		if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(diag, err)
			return 5
		}
		fmt.Fprintln(out, "Removed launchd agent", launchdLabel)
	case "linux":
		home, _ := os.UserHomeDir()
		unitPath := filepath.Join(home, ".config", "systemd", "user", systemdUnit)
		_ = exec.Command("systemctl", "--user", "disable", "--now", systemdUnit).Run()
		if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(diag, err)
			return 5
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
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
		if o, err := exec.Command("launchctl", "print", domain).CombinedOutput(); err != nil {
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
		o, _ := exec.Command("systemctl", "--user", "is-active", systemdUnit).CombinedOutput()
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
func launchdPlist(bin string, argv []string, logPath string) string {
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
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchdLabel, progArgs.String(), xmlEscape(logPath), xmlEscape(logPath))
}

// systemdUnitFile renders the --user unit. Every argument is systemd-quoted so
// a path with %, $, backslash or quotes is neither expanded nor able to break
// out of ExecStart.
func systemdUnitFile(bin string, argv []string) string {
	execStart := `"` + systemdQuoted(bin) + `"`
	for _, a := range argv {
		execStart += ` "` + systemdQuoted(a) + `"`
	}
	return fmt.Sprintf(`[Unit]
Description=Continuum daemon
After=network.target

[Service]
ExecStart=%s
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
`, execStart)
}
