package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/NakliTechie/menagerie/relay-go/internal/config"
)

const launchdLabel = "com.naklitechie.menagerie-relay"

// isInteractive reports whether stdout is a terminal. We only copy the token to
// the clipboard and print it when a human is watching — never in a service
// context (keeps the token out of log files).
func isInteractive() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

// copyToClipboard best-effort copies s to the OS clipboard. Returns false if no
// clipboard tool is available (e.g. a headless/SSH box) — the caller still prints
// the token in that case.
func copyToClipboard(s string) bool {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "linux":
		for _, c := range [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "--clipboard", "--input"}} {
			if _, err := exec.LookPath(c[0]); err == nil {
				cmd = exec.Command(c[0], c[1:]...)
				break
			}
		}
	}
	if cmd == nil {
		return false
	}
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run() == nil
}

// announceToken prints the registration token and, when possible, drops it on
// the clipboard so the user just pastes it into Menagerie.
func announceToken(tok string) {
	if copyToClipboard(tok) {
		fmt.Println("✓ Registration token copied to your clipboard.")
		fmt.Println("  In Menagerie, paste it into the localhost relay card and hit Connect.")
	} else {
		fmt.Println("Registration token — paste into Menagerie (localhost relay card → Connect):")
	}
	fmt.Printf("  %s\n\n", tok)
}

// ensureConfig loads the config, creating a default one on first run so a fresh
// user only ever needs a single command.
func ensureConfig(path string) *config.Config {
	if !config.Exists(path) {
		cfg, err := config.Default()
		if err != nil {
			fatal(err)
		}
		if err := config.Save(path, cfg); err != nil {
			fatal(err)
		}
		fmt.Printf("First run — created %s (relay %q, listening %s).\n", path, cfg.Name, cfg.Listen)
		return cfg
	}
	cfg, err := config.Load(path)
	if err != nil {
		fatal(err)
	}
	return cfg
}

func cmdService(path string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "install":
		serviceInstall(path)
	case "uninstall":
		serviceUninstall()
	case "status":
		serviceStatus()
	default:
		fatalf("usage: menagerie-relay service [install|uninstall|status]")
	}
}

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		fatalf("cannot resolve own path: %v", err)
	}
	abs, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return abs
}

func menagerieDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	return filepath.Join(home, ".menagerie")
}

func serviceInstall(path string) {
	cfg := ensureConfig(path) // make sure there's a token before we background it
	bin := selfPath()
	logPath := filepath.Join(menagerieDir(), "relay.log")

	switch runtime.GOOS {
	case "darwin":
		installLaunchd(bin, logPath)
	case "linux":
		installSystemd(bin)
	default:
		fatalf("`service install` supports macOS and Linux; run `menagerie-relay serve` yourself on %s", runtime.GOOS)
	}
	fmt.Println("\nThe relay will now start at login and restart if it ever exits.")
	if isInteractive() {
		fmt.Println()
		announceToken(cfg.RegistrationToken)
	}
}

func installLaunchd(bin, logPath string) {
	home, _ := os.UserHomeDir()
	plistDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(plistDir, 0o755); err != nil {
		fatal(err)
	}
	plistPath := filepath.Join(plistDir, launchdLabel+".plist")
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchdLabel, bin, logPath, logPath)
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("Wrote %s\n", plistPath)

	domain := fmt.Sprintf("gui/%d", os.Getuid())
	// bootout first so a re-install reloads cleanly; ignore "not loaded" errors.
	_ = exec.Command("launchctl", "bootout", domain+"/"+launchdLabel).Run()
	if err := exec.Command("launchctl", "bootstrap", domain, plistPath).Run(); err != nil {
		// Fall back to the older verb, then to manual instructions.
		_ = exec.Command("launchctl", "unload", plistPath).Run()
		if err2 := exec.Command("launchctl", "load", "-w", plistPath).Run(); err2 != nil {
			fmt.Printf("Could not load the service automatically (%v).\nLoad it yourself with:\n  launchctl bootstrap %s %q\n", err, domain, plistPath)
			return
		}
	}
	fmt.Println("Loaded launchd agent", launchdLabel)
}

func installSystemd(bin string) {
	home, _ := os.UserHomeDir()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		fatal(err)
	}
	unitPath := filepath.Join(unitDir, "menagerie-relay.service")
	unit := fmt.Sprintf(`[Unit]
Description=Menagerie relay
After=network.target

[Service]
ExecStart=%s serve
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
`, bin)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("Wrote %s\n", unitPath)

	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	if err := exec.Command("systemctl", "--user", "enable", "--now", "menagerie-relay.service").Run(); err != nil {
		fmt.Printf("Could not enable the service automatically (%v).\nEnable it yourself with:\n  systemctl --user enable --now menagerie-relay.service\n", err)
		return
	}
	fmt.Println("Enabled systemd --user unit menagerie-relay.service")
	fmt.Println("Tip: to keep it running after you log out / across reboots on a headless box:")
	fmt.Println("  loginctl enable-linger \"$USER\"")
}

func serviceUninstall() {
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		_ = exec.Command("launchctl", "bootout", domain+"/"+launchdLabel).Run()
		_ = exec.Command("launchctl", "unload", plistPath).Run()
		if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
			fatal(err)
		}
		fmt.Println("Removed launchd agent", launchdLabel)
	case "linux":
		home, _ := os.UserHomeDir()
		unitPath := filepath.Join(home, ".config", "systemd", "user", "menagerie-relay.service")
		_ = exec.Command("systemctl", "--user", "disable", "--now", "menagerie-relay.service").Run()
		if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
			fatal(err)
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		fmt.Println("Removed systemd unit menagerie-relay.service")
	default:
		fatalf("`service uninstall` supports macOS and Linux")
	}
}

func serviceStatus() {
	switch runtime.GOOS {
	case "darwin":
		domain := fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel)
		out, err := exec.Command("launchctl", "print", domain).CombinedOutput()
		if err != nil {
			fmt.Println("Service not loaded. Install it with `menagerie-relay service install`.")
			return
		}
		// surface the state line if present
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "state =") || strings.Contains(line, "pid =") {
				fmt.Println(strings.TrimSpace(line))
			}
		}
	case "linux":
		out, _ := exec.Command("systemctl", "--user", "is-active", "menagerie-relay.service").CombinedOutput()
		fmt.Print(string(out))
	default:
		fatalf("`service status` supports macOS and Linux")
	}
}
