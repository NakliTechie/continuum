package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/NakliTechie/continuum/internal/config"
)

// A programmatic config that omits CaptureDir must capture nothing, so an
// embedder or test cannot write into the legacy home directory by omission.
func TestProgrammaticConfigCapturesNothing(t *testing.T) {
	cfg := &config.Config{RegistrationToken: "t", Tmux: "off", Agents: map[string]config.Agent{"custom": {}}}
	New(cfg)
	if cfg.CaptureDir == nil || *cfg.CaptureDir != "" {
		t.Fatalf("programmatic config must disable capture, got %v", cfg.CaptureDir)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".menagerie")); !os.IsNotExist(err) {
		t.Fatalf("test home must not contain .menagerie: %v", err)
	}
}
