package acp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Opt-in real-provider evidence. The child receives no cloud API credentials,
// uses a temporary home/cwd, and is pinned to an explicitly named local model.
func TestRealLocalOmpACP(t *testing.T) {
	if os.Getenv("CONTINUUM_REAL_ACP") != "1" {
		t.Skip("set CONTINUUM_REAL_ACP=1 with a local Ollama model")
	}
	model := os.Getenv("CONTINUUM_REAL_MODEL")
	if !strings.HasPrefix(model, "ollama/") {
		t.Fatal("real ACP test requires an explicit ollama/ model")
	}
	binary, err := exec.LookPath("omp")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cmd := exec.Command(binary, "--model", model, "--no-extensions", "--no-skills", "--no-rules", "--no-tools", "acp")
	cmd.Dir = home
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"OLLAMA_HOST=http://127.0.0.1:11434",
		"LANG=en_US.UTF-8",
		"NO_COLOR=1",
	}
	// A fresh OMP home has no model catalog yet. Discover only the local
	// Ollama provider, then refuse to start ACP unless the exact selector is
	// present; this also prevents a fuzzy cloud-model fallback.
	catalog := exec.Command(binary, "models", "ollama", "--json")
	catalog.Env, catalog.Dir = cmd.Env, home
	raw, err := catalog.Output()
	if err != nil {
		t.Fatalf("local Ollama discovery failed: %v", err)
	}
	var listing struct {
		Models []struct {
			Selector string `json:"selector"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range listing.Models {
		found = found || item.Selector == model
	}
	if !found {
		t.Fatalf("local Ollama model %q is not installed", model)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	noCapture := ""
	s, err := StartContext(ctx, "local-omp", "omp", home, cmd, &noCapture)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	defer func() {
		s.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("local ACP child did not exit after kill")
		}
	}()
	go s.Run(func(int) { close(done) })
	var mu sync.Mutex
	var updates []byte
	s.SetOnUpdate(func(raw json.RawMessage) {
		mu.Lock()
		updates = append(updates, raw...)
		mu.Unlock()
	})
	response, err := s.Prompt("Reply with the single word READY. Do not use tools.")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-response:
		if result == nil || result.Error != nil || !json.Valid(result.Result) {
			t.Fatalf("local ACP prompt failed: %+v", result)
		}
		mu.Lock()
		seen := string(updates)
		mu.Unlock()
		if !strings.Contains(strings.ToUpper(seen), "READY") {
			t.Fatalf("local ACP turn returned without expected content: %q", seen)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("local ACP prompt did not finish")
	}
}
