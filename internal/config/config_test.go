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
