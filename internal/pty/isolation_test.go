package pty

import (
	"os"
	"testing"
)

// TestMain keeps every test's implicit home away from the installed Menagerie
// directory: a config without CaptureDir once defaulted to ~/.menagerie/sessions,
// and raw `go test` runs left thousands of fake-agent captures there.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "continuum-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("TMPDIR", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
