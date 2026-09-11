package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A session's environment must reach the agent without ever appearing on the
// tmux client's argv, and exact targets must not address a sibling session.
// Runs against a private tmux server (TMUX_TMPDIR) so the user's is untouched.
func TestCreateDeliversEnvPrivatelyAndTargetsExactly(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// Unix socket paths are short; t.TempDir() names are not.
	sock, err := os.MkdirTemp("/tmp", "ct-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sock) })
	t.Setenv("TMUX_TMPDIR", sock)
	os.Unsetenv("TMUX")
	envDir := t.TempDir() // where Create's private environment file lands
	t.Setenv("TMPDIR", envDir)
	out := filepath.Join(t.TempDir(), "env.txt")
	name := SessionName("envtest01")
	// PATH deliberately lacks rm: the wrapper must not depend on the sourced environment to clean up.
	env := []string{"CONTINUUM_SECRET=s3cr3t value", "PATH=/nonexistent", "TMUX=nested", "BAD-NAME=x"}
	if err := Create(name, t.TempDir(), env, []string{"/bin/sh", "-c", "/usr/bin/env > " + out + "; /bin/sleep 30"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Kill(name); _ = exec.Command("tmux", "kill-server").Run() })
	var got string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if b, err := os.ReadFile(out); err == nil && strings.Contains(string(b), "CONTINUUM_SECRET=") {
			got = string(b)
			break
		}
	}
	if !strings.Contains(got, "CONTINUUM_SECRET=s3cr3t value\n") {
		t.Fatalf("environment did not reach the agent:\n%s", got)
	}
	if strings.Contains(got, "TMUX=nested") || strings.Contains(got, "BAD-NAME") {
		t.Fatalf("nesting or unassignable variables leaked through:\n%s", got)
	}
	// The private file is gone once sourced.
	if matches, _ := filepath.Glob(filepath.Join(envDir, "continuum-env-*")); len(matches) != 0 {
		t.Fatalf("environment file left behind: %v", matches)
	}
	// Smoke check that no live process carries the secret on its argv; the
	// guarantee itself comes from never placing env on argv at all.
	ps, err := exec.Command("ps", "-axo", "args").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ps), "s3cr3t") {
		t.Fatal("secret visible on a process argv")
	}
	// Exact targeting: a session whose name extends ours must not be hit.
	sibling := name + "-sibling"
	if err := exec.Command("tmux", "new-session", "-d", "-s", sibling, "sleep 30").Run(); err != nil {
		t.Fatal(err)
	}
	if !Exists(name) || !Exists(sibling) {
		t.Fatal("both sessions should exist")
	}
	if err := Kill(name); err != nil {
		t.Fatal(err)
	}
	if Exists(name) {
		t.Fatal("killed session still reported")
	}
	if !Exists(sibling) {
		t.Fatal("exact kill removed the sibling")
	}
	if Exists(name[:len(name)-2]) {
		t.Fatal("a prefix must not match any session")
	}
}
