package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/journal"
)

// The modern API is not a browser surface: any Origin header is refused before
// the bearer is examined, so a page on any origin cannot drive /v1 even with a
// leaked token.
func TestModernRefusesEveryBrowserOrigin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	noCapture := ""
	s := New(&config.Config{RegistrationToken: "operator", Agents: map[string]config.Agent{"custom": {}}, Tmux: "off", CaptureDir: &noCapture, Listen: "127.0.0.1:0"})
	m := s.EnableModern(store, "viewer")
	h := httptest.NewServer(m)
	defer h.Close()
	for _, origin := range []string{"https://evil.test", "http://127.0.0.1:5173", "http://localhost:3000", "null"} {
		raw, _ := json.Marshal(api.Request{Operation: "status"})
		r, _ := http.NewRequest("POST", h.URL, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer operator")
		r.Header.Set("Origin", origin)
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		var v api.Response
		_ = json.NewDecoder(res.Body).Decode(&v)
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden || v.Class != "access_denied" || v.Code != "origin" {
			t.Fatalf("origin %q: status %d response %+v", origin, res.StatusCode, v)
		}
	}
}

// The legacy WebSocket adapter gates on the origin allowlist before any frame is
// exchanged: a disallowed browser origin is refused at the upgrade with 403, an
// allowed one and a non-browser client (no Origin) reach the hello frame.
func TestLegacyUpgradeRejectsDisallowedOrigin(t *testing.T) {
	s := New(&config.Config{RegistrationToken: "operator", Tmux: "off", Listen: "127.0.0.1:0", AllowedOrigins: []string{"https://app.example"}})
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	url := "ws" + h.URL[len("http"):]
	dial := func(origin string) (*websocket.Conn, *http.Response, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		opts := &websocket.DialOptions{}
		if origin != "" {
			opts.HTTPHeader = http.Header{"Origin": []string{origin}}
		}
		return websocket.Dial(ctx, url, opts)
	}
	for _, origin := range []string{"https://evil.test", "null"} {
		c, res, err := dial(origin)
		if err == nil {
			c.CloseNow()
			t.Fatalf("origin %q was accepted", origin)
		}
		if res == nil || res.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %q: want 403, got %v", origin, res)
		}
	}
	for _, origin := range []string{"https://app.example", ""} {
		c, _, err := dial(origin)
		if err != nil {
			t.Fatalf("origin %q refused: %v", origin, err)
		}
		var hello map[string]any
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, data, err := c.Read(ctx)
		cancel()
		c.CloseNow()
		if err != nil || json.Unmarshal(data, &hello) != nil || hello["type"] != "hello" {
			t.Fatalf("origin %q: expected hello, got %s %v", origin, data, err)
		}
	}
}
