package config

import (
	"path/filepath"
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
