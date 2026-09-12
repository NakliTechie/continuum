package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLaunchdPlistEscapesAndCarriesArgs(t *testing.T) {
	argv := []string{"serve", "--state", "/home/a&b/<state>", "--listen", "127.0.0.1:58750"}
	plist := launchdPlist("/opt/con\"tinuum", argv, "/home/a&b/serve.log")
	for _, want := range []string{
		"<string>com.naklitechie.continuum</string>",
		"<string>/opt/con&quot;tinuum</string>",
		"<string>serve</string>",
		"<string>/home/a&amp;b/&lt;state&gt;</string>",
		"<string>127.0.0.1:58750</string>",
		"<key>KeepAlive</key><true/>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "<state>") || strings.Contains(plist, "a&b") {
		t.Fatal("unescaped metacharacter reached the plist")
	}
}

func TestSystemdUnitQuotesArgs(t *testing.T) {
	argv := []string{"serve", "--state", "/srv/pct$HOME/%weird", "--listen", "127.0.0.1:58750"}
	unit := systemdUnitFile("/usr/bin/continuum", argv)
	if !strings.Contains(unit, `ExecStart="/usr/bin/continuum" "serve" "--state" "/srv/pct$$HOME/%%weird" "--listen" "127.0.0.1:58750"`) {
		t.Fatalf("systemd ExecStart not quoted as expected:\n%s", unit)
	}
	if !strings.Contains(unit, "Restart=always") {
		t.Fatal("missing Restart=always")
	}
	if !strings.Contains(unit, "KillMode=process") {
		t.Fatal("missing KillMode=process — holders in the cgroup would be killed on stop")
	}
}

func TestServiceInstallRequiresAFixedPort(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "127.0.0.1:00", "127.0.0.1:", "127.0.0.1:abc"} {
		var out, diag strings.Builder
		if code := serviceInstall(t.TempDir(), addr, "", &out, &diag); code != 2 {
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
