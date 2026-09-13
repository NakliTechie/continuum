package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLaunchdPlistEscapesAndCarriesArgs(t *testing.T) {
	argv := []string{"serve", "--state", "/home/a&b/<state>", "--listen", "127.0.0.1:58750"}
	plist := launchdPlist("/opt/con\"tinuum", argv, "/home/a&b/serve.log", "/opt/homebrew/bin:/usr/bin")
	for _, want := range []string{
		"<string>com.naklitechie.continuum</string>",
		"<string>/opt/con&quot;tinuum</string>",
		"<string>serve</string>",
		"<string>/home/a&amp;b/&lt;state&gt;</string>",
		"<string>127.0.0.1:58750</string>",
		"<key>KeepAlive</key><true/>",
		"<key>AbandonProcessGroup</key><true/>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "<state>") || strings.Contains(plist, "a&b") {
		t.Fatal("unescaped metacharacter reached the plist")
	}
	if !strings.Contains(plist, "<key>EnvironmentVariables</key>") || !strings.Contains(plist, "/opt/homebrew/bin") {
		t.Fatalf("plist missing a PATH environment so agents like `claude` would not resolve:\n%s", plist)
	}
}

func TestServicePathIncludesInstallTimeAndCommonDirs(t *testing.T) {
	t.Setenv("PATH", "/custom/tool/bin:/usr/bin")
	p := servicePath()
	for _, want := range []string{"/custom/tool/bin", "/usr/bin", "/opt/homebrew/bin", "/bin"} {
		if !strings.Contains(p, want) {
			t.Fatalf("servicePath missing %q: %s", want, p)
		}
	}
}

func TestSystemdUnitQuotesArgs(t *testing.T) {
	argv := []string{"serve", "--state", "/srv/pct$HOME/%weird", "--listen", "127.0.0.1:58750"}
	unit := systemdUnitFile("/usr/bin/continuum", argv, "/usr/bin:/bin")
	if !strings.Contains(unit, `ExecStart="/usr/bin/continuum" "serve" "--state" "/srv/pct$$HOME/%%weird" "--listen" "127.0.0.1:58750"`) {
		t.Fatalf("systemd ExecStart not quoted as expected:\n%s", unit)
	}
	if !strings.Contains(unit, "Restart=always") {
		t.Fatal("missing Restart=always")
	}
	if !strings.Contains(unit, "KillMode=process") {
		t.Fatal("missing KillMode=process — holders in the cgroup would be killed on stop")
	}
	if !strings.Contains(unit, "KillSignal=SIGKILL") {
		t.Fatal("missing KillSignal=SIGKILL — graceful daemon shutdown stops holder-owned blocks")
	}
}

func TestLaunchdHandoffKillsOnlyTheDaemonBeforeBootout(t *testing.T) {
	old := runServiceCommand
	t.Cleanup(func() { runServiceCommand = old })
	var calls []string
	runServiceCommand = func(name string, args ...string) error {
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
		return nil
	}
	var diag strings.Builder
	if code := stopLaunchdForHandoff("gui/501", &diag); code != 0 {
		t.Fatalf("handoff failed: %d %q", code, diag.String())
	}
	want := []string{
		"launchctl print gui/501/com.naklitechie.continuum",
		"launchctl disable gui/501/com.naklitechie.continuum",
		"launchctl kill SIGKILL gui/501/com.naklitechie.continuum",
		"launchctl bootout gui/501/com.naklitechie.continuum",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unsafe launchd handoff order:\n%s", strings.Join(calls, "\n"))
	}
}

func TestLaunchdHandoffRefusesTerminatingFallback(t *testing.T) {
	old := runServiceCommand
	t.Cleanup(func() { runServiceCommand = old })
	var calls []string
	runServiceCommand = func(name string, args ...string) error {
		call := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, call)
		if strings.Contains(call, " kill SIGKILL ") {
			return errors.New("denied")
		}
		return nil
	}
	var diag strings.Builder
	if code := stopLaunchdForHandoff("gui/501", &diag); code != 5 {
		t.Fatalf("unsafe handoff did not fail closed: %d", code)
	}
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, "bootout") {
		t.Fatalf("SIGKILL failure fell back to block-stopping bootout:\n%s", joined)
	}
	if !strings.Contains(joined, "launchctl enable gui/501/com.naklitechie.continuum") {
		t.Fatalf("failed handoff did not restore launchd enablement:\n%s", joined)
	}
}

func TestSystemdInstallReloadsAndRestartsAnExistingUnit(t *testing.T) {
	old := runServiceCommand
	t.Cleanup(func() { runServiceCommand = old })
	var calls []string
	runServiceCommand = func(name string, args ...string) error {
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))
		return nil
	}
	t.Setenv("HOME", t.TempDir())
	var out, diag strings.Builder
	if code := installSystemd("/opt/continuum", []string{"serve", "--state", "/tmp/state", "--listen", "127.0.0.1:58750"}, &out, &diag); code != 0 {
		t.Fatalf("install failed: %d %q", code, diag.String())
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable continuum.service",
		"systemctl --user restart continuum.service",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("systemd install did not reload the running binary:\n%s", strings.Join(calls, "\n"))
	}
}

func TestServiceInstallRequiresAFixedPort(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "127.0.0.1:00", "127.0.0.1:", "127.0.0.1:abc"} {
		var out, diag strings.Builder
		if code := serviceInstall(t.TempDir(), addr, "", "", &out, &diag); code != 2 {
			t.Fatalf("non-fixed port %q accepted: code %d", addr, code)
		}
		if !strings.Contains(diag.String(), "fixed --listen") {
			t.Fatalf("%q: missing guidance: %q", addr, diag.String())
		}
	}
}

// The documented `service install --flags` order must parse: Go's flag parser
// stops at the subcommand positional, so the CLI re-parses the rest. A dry run
// writes the unit under a throwaway HOME and skips loading a real service.
func TestServiceInstallParsesFlagsAfterTheSubcommand(t *testing.T) {
	home, state := t.TempDir(), filepath.Join(t.TempDir(), "state")
	t.Setenv("HOME", home)
	t.Setenv("CONTINUUM_SERVICE_DRYRUN", "1")
	var out, diag strings.Builder
	code := Run([]string{"service", "install", "--state", state, "--listen", "127.0.0.1:58750"}, nil, &out, &diag)
	if code != 0 {
		t.Fatalf("documented order failed: code %d, diag %q", code, diag.String())
	}
	if runtime.GOOS == "darwin" {
		plist, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"))
		if err != nil {
			t.Fatalf("no plist written: %v", err)
		}
		if !strings.Contains(string(plist), "127.0.0.1:58750") || !strings.Contains(string(plist), state) {
			t.Fatalf("flags after the subcommand were not parsed into the unit:\n%s", plist)
		}
	}
	if !strings.Contains(out.String(), "dry run") {
		t.Fatalf("dry run did not short-circuit: %q", out.String())
	}
}

func TestSystemdQuotedNeutralisesSpecifiers(t *testing.T) {
	if got := systemdQuoted(`a"b\c%d$e`); got != `a\"b\\c%%d$$e` {
		t.Fatalf("systemdQuoted = %q", got)
	}
}

// Cutover installs the modern daemon adopting a relay's config: the generated
// unit must carry --adopt-relay and the relay's own port. Dry run writes the
// unit under a throwaway HOME and never loads a real service.
func TestServiceCutoverAdoptsTheRelayPortAndConfig(t *testing.T) {
	home := t.TempDir()
	relay := filepath.Join(t.TempDir(), "relay.toml")
	body := "name = \"h\"\nlisten = \"127.0.0.1:7878\"\nregistration_token = \"abcdefabcdefabcdefabcdefabcdefab\"\nallowed_origins = [\"https://menagerie.naklitechie.com\"]\n"
	if err := os.WriteFile(relay, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("HOME", home)
	t.Setenv("CONTINUUM_SERVICE_DRYRUN", "1")
	var out, diag strings.Builder
	code := Run([]string{"service", "cutover", "--state", state, "--adopt-relay", relay}, nil, &out, &diag)
	if code != 0 {
		t.Fatalf("cutover failed: code %d, diag %q, out %q", code, diag.String(), out.String())
	}
	if runtime.GOOS == "darwin" {
		plist, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"<string>--adopt-relay</string>", "<string>" + relay + "</string>", "<string>127.0.0.1:7878</string>"} {
			if !strings.Contains(string(plist), want) {
				t.Fatalf("cutover unit missing %q:\n%s", want, plist)
			}
		}
	}
	if !strings.Contains(out.String(), "adopt") {
		t.Fatalf("cutover did not announce adoption: %q", out.String())
	}
}

// A dry-run cutover must not touch an installed legacy service — it only
// previews. We fake an installed relay plist/unit and confirm it is left in
// place after a dry-run cutover.
func TestDryRunCutoverLeavesTheLegacyServiceAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CONTINUUM_SERVICE_DRYRUN", "1")
	var legacy string
	switch runtime.GOOS {
	case "darwin":
		legacy = filepath.Join(home, "Library", "LaunchAgents", "com.naklitechie.menagerie-relay.plist")
	case "linux":
		legacy = filepath.Join(home, ".config", "systemd", "user", "menagerie-relay.service")
	default:
		t.Skip("service paths are darwin/linux")
	}
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("pretend-installed-relay"), 0o644); err != nil {
		t.Fatal(err)
	}
	relay := filepath.Join(t.TempDir(), "relay.toml")
	if err := os.WriteFile(relay, []byte("listen = \"127.0.0.1:7878\"\nregistration_token = \"abcdefabcdefabcdefabcdefabcdefab\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, diag strings.Builder
	if code := Run([]string{"service", "cutover", "--state", filepath.Join(t.TempDir(), "s"), "--adopt-relay", relay}, nil, &out, &diag); code != 0 {
		t.Fatalf("dry-run cutover failed: %d %q", code, diag.String())
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("dry run removed/renamed the legacy service: %v", err)
	}
	if !strings.Contains(out.String(), "dry run") {
		t.Fatalf("dry run not reported: %q", out.String())
	}
}
