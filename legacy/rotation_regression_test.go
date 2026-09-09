package legacy

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/server"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestOvernightRotateRevokesOldRegistration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.toml")
	old := "disposable-audit-registration-token"
	cfg := &config.Config{Name: "audit", Listen: "127.0.0.1:0", Tmux: "off", RegistrationToken: old}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(loaded).Handler())
	defer ts.Close()
	// Suppress the disposable new credential from the test report.
	before := os.Stdout
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wr
	cmdToken(path, []string{"rotate"})
	os.Stdout = before
	wr.Close()
	io.Copy(io.Discard, rd)
	rd.Close()
	fresh, err := config.Load(path)
	if err != nil || fresh.RegistrationToken == old {
		t.Fatal("rotation did not update disk")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	var f map[string]any
	if err := wsjson.Read(ctx, c, &f); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, c, map[string]any{"type": "register", "registration_token": old}); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Read(ctx, c, &f); err != nil {
		t.Fatal(err)
	}
	if f["type"] == "registered" {
		t.Fatal("old credential still registers after token rotate updates the config")
	}
}
