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

func TestRotationCheckerActualTokenCommandPositive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.toml")
	cfg := &config.Config{Name: "checker", Listen: "127.0.0.1:0", Tmux: "off", RegistrationToken: "checker-old", Agents: map[string]config.Agent{"custom": {}}}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(loaded).Handler())
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	old, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer old.CloseNow()
	var f map[string]any
	if err := wsjson.Read(ctx, old, &f); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, old, map[string]any{"type": "register", "registration_token": "checker-old"}); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Read(ctx, old, &f); err != nil || f["type"] != "registered" {
		t.Fatal("startup registration failed")
	}
	if err := wsjson.Read(ctx, old, &f); err != nil || f["type"] != "sessions" {
		t.Fatal("startup inventory failed")
	}
	stdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	cmdToken(path, []string{"rotate"})
	os.Stdout = stdout
	writer.Close()
	output, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "Processes keep running") || !strings.Contains(string(output), "before their next command or output") {
		t.Fatal("rotation message omits operational boundary")
	}
	fresh, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.RegistrationToken == "" || fresh.RegistrationToken == "checker-old" || fresh.Name != cfg.Name || fresh.Listen != cfg.Listen || len(fresh.Agents) != 1 {
		t.Fatal("token command did not preserve configuration and replace authority")
	}
	current, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer current.CloseNow()
	if err := wsjson.Read(ctx, current, &f); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, current, map[string]any{"type": "register", "registration_token": fresh.RegistrationToken}); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Read(ctx, current, &f); err != nil || f["type"] != "registered" {
		t.Fatal("token emitted by actual rotate command did not register")
	}
	if err := wsjson.Write(ctx, old, map[string]any{"type": "resume", "session_id": "checker"}); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Read(ctx, old, &f); err == nil || ctx.Err() != nil {
		t.Fatal("existing old connection did not close before response")
	}
}
