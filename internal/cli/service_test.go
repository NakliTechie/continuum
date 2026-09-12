package cli

import (
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
}

func TestServiceInstallRequiresAFixedPort(t *testing.T) {
	var out, diag strings.Builder
	// no port
	if code := serviceInstall(t.TempDir(), "127.0.0.1:0", "", &out, &diag); code != 2 {
		t.Fatalf("random port accepted: code %d", code)
	}
	if !strings.Contains(diag.String(), "fixed --listen") {
		t.Fatalf("missing guidance: %q", diag.String())
	}
}

func TestSystemdQuotedNeutralisesSpecifiers(t *testing.T) {
	if got := systemdQuoted(`a"b\c%d$e`); got != `a\"b\\c%%d$$e` {
		t.Fatalf("systemdQuoted = %q", got)
	}
}
