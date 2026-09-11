package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateTokenUniqueNonEmpty(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == "" || b == "" {
		t.Fatal("token is empty")
	}
	if a == b {
		t.Fatal("tokens are not unique")
	}
	if len(a) < 40 { // 32 bytes base64url (unpadded) ≈ 43 chars
		t.Fatalf("token unexpectedly short: %d chars", len(a))
	}
}

func TestOriginAllowed(t *testing.T) {
	c := &Config{AllowedOrigins: []string{"https://menagerie.naklitechie.com", "null"}}
	for _, ok := range []string{"https://menagerie.naklitechie.com", "null"} {
		if !c.OriginAllowed(ok) {
			t.Errorf("expected origin %q to be allowed", ok)
		}
	}
	for _, bad := range []string{"https://evil.example", "", "http://menagerie.naklitechie.com"} {
		if c.OriginAllowed(bad) {
			t.Errorf("expected origin %q to be denied", bad)
		}
	}
}

// The default allowlist must admit both places the app is served from: its own
// site, and the same-origin mirror NakliOS hosts. Dropping either silently breaks
// one of the two front doors with a 403 that the browser reports as a bare
// WebSocket close.
func TestDefaultOriginsAdmitBothFrontDoors(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []string{"https://menagerie.naklitechie.com", "https://naklios.dev"} {
		if !c.OriginAllowed(o) {
			t.Errorf("default allowlist rejects %q", o)
		}
	}
	for _, bad := range []string{"null", "https://evil.example", "http://naklios.dev", "https://naklios.dev.evil.example"} {
		if c.OriginAllowed(bad) {
			t.Errorf("default allowlist admits %q", bad)
		}
	}
}

func TestOriginAllowedLocalhost(t *testing.T) {
	// A loopback-bound relay auto-allows localhost / 127.0.0.1 / [::1] origins…
	lo := &Config{Listen: "127.0.0.1:7878", AllowedOrigins: []string{"https://menagerie.naklitechie.com"}}
	for _, ok := range []string{"http://localhost:8077", "http://127.0.0.1:3000", "https://localhost:5173", "http://[::1]:9000"} {
		if !lo.OriginAllowed(ok) {
			t.Errorf("loopback relay should allow local origin %q", ok)
		}
	}
	// …but never a real site (even one that starts with "localhost"), nor null/empty.
	for _, bad := range []string{"https://evil.example", "http://localhost.evil.com", "http://notlocalhost", "null", ""} {
		if lo.OriginAllowed(bad) {
			t.Errorf("loopback relay should still deny %q", bad)
		}
	}
	// A relay exposed on 0.0.0.0 does NOT auto-allow localhost origins.
	if (&Config{Listen: "0.0.0.0:7878"}).OriginAllowed("http://localhost:8077") {
		t.Error("exposed relay must not auto-allow localhost origins")
	}
	// Opt-out disables it even on loopback.
	no := false
	if (&Config{Listen: "127.0.0.1:7878", AllowLocalhostOrigins: &no}).OriginAllowed("http://localhost:8077") {
		t.Error("allow_localhost_origins=false must disable the convenience")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.toml")
	want, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.RegistrationToken != want.RegistrationToken {
		t.Error("registration_token did not round-trip")
	}
	if got.Listen != want.Listen {
		t.Error("listen did not round-trip")
	}
	if len(got.Agents) != len(want.Agents) {
		t.Errorf("agents count: got %d want %d", len(got.Agents), len(want.Agents))
	}
}

// Rotation replaces the token line only: keys this version does not model,
// comments and the operator's formatting survive.
func TestRotateTokenKeepsUnknownKeysAndComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.toml")
	original := "# operator note\nname = \"box\"\nregistration_token = \"old-token\"\nfuture_key = 42\n\n[agents.claude]\ncommand = \"claude\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := RotateToken(path, cfg, `new"token\x`); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	for _, keep := range []string{"# operator note", "name = \"box\"", "future_key = 42", "[agents.claude]", "command = \"claude\""} {
		if !strings.Contains(got, keep) {
			t.Fatalf("rotation dropped %q:\n%s", keep, got)
		}
	}
	if strings.Contains(got, "old-token") {
		t.Fatalf("old token survived:\n%s", got)
	}
	fresh, err := Load(path)
	if err != nil || fresh.RegistrationToken != `new"token\x` {
		t.Fatalf("rotated token does not load back: %q %v", fresh.RegistrationToken, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("rotated file mode %v", info.Mode())
	}

	// A same-named key inside a table is a different key and stays untouched;
	// a multi-line token cannot be swapped in place and falls back to a full
	// rewrite that still loads.
	tabled := "registration_token = \"top\"\n\n[future]\nregistration_token = \"nested\"\n"
	if err := os.WriteFile(path, []byte(tabled), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load(path)
	if err := RotateToken(path, cfg, "rotated"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), "registration_token = \"nested\"") || strings.Contains(string(b), "\"top\"") {
		t.Fatalf("table key changed or top-level key kept:\n%s", b)
	}
	// A decoy key line inside an earlier multi-line string must not be the
	// one rewritten; the parser check catches what the line scan cannot.
	decoy := "future_note = \"\"\"\nregistration_token = \"decoy\"\n\"\"\"\nregistration_token = \"old\"\nname = \"box\"\n"
	if err := os.WriteFile(path, []byte(decoy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load(path)
	if err := RotateToken(path, cfg, "rotated3"); err != nil {
		t.Fatal(err)
	}
	if fresh, err := Load(path); err != nil || fresh.RegistrationToken != "rotated3" || fresh.Name != "box" {
		t.Fatalf("decoy line fooled rotation: %+v %v", fresh, err)
	}
	multi := "registration_token = \"\"\"\nold\n\"\"\"\nname = \"box\"\n"
	if err := os.WriteFile(path, []byte(multi), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load(path)
	if err := RotateToken(path, cfg, "rotated2"); err != nil {
		t.Fatal(err)
	}
	if fresh, err := Load(path); err != nil || fresh.RegistrationToken != "rotated2" || fresh.Name != "box" {
		t.Fatalf("multi-line fallback did not produce a loadable config: %+v %v", fresh, err)
	}
}
