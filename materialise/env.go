// Package materialise executes a spec's materialise block against a provisioned
// workspace, in the one fixed order the handoff specifies:
//
//	ports -> files -> commands -> services -> health -> escape
//
// The order is not configurable: a file that needs a port must be able to read
// it, and a command that needs a file must run after it.
package materialise

import (
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FileSystem is the small surface the engine needs. It is an interface so the
// dry run (C3) can hand it a fake that records writes instead of performing
// them — and so the test suite can assert "no filesystem writes occurred".
type FileSystem interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, b []byte, perm fs.FileMode) error
	MkdirAll(path string, perm fs.FileMode) error
	Stat(path string) (fs.FileInfo, error)
}

// Executor runs one shell command line in a directory with an environment.
type Executor interface {
	Run(dir string, env []string, cmdline string, timeout time.Duration) ([]byte, error)
}

// Dialer answers whether something is listening. Supervision uses it, and a
// test can substitute one rather than binding real ports.
type Dialer func(addr string, timeout time.Duration) error

// TCPDial is the real dialer.
func TCPDial(addr string, timeout time.Duration) error {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	return c.Close()
}

// Prober answers whether a health check passes.
type Prober interface {
	HTTP(url string, timeout time.Duration) error
}

// OSFileSystem is the real filesystem.
type OSFileSystem struct{}

func (OSFileSystem) ReadFile(p string) ([]byte, error) { return os.ReadFile(p) }
func (OSFileSystem) WriteFile(p string, b []byte, perm fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, perm)
}
func (OSFileSystem) MkdirAll(p string, perm fs.FileMode) error { return os.MkdirAll(p, perm) }
func (OSFileSystem) Stat(p string) (fs.FileInfo, error)        { return os.Stat(p) }

// ShellExecutor runs command lines through `sh -c`, which is what a declared
// `run` string means. A timeout of 0 means no timeout.
type ShellExecutor struct{}

func (ShellExecutor) Run(dir string, env []string, cmdline string, timeout time.Duration) ([]byte, error) {
	cmd := exec.Command("sh", "-c", cmdline)
	cmd.Dir = dir
	cmd.Env = env
	if timeout <= 0 {
		return cmd.CombinedOutput()
	}
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
		return out, err
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return out, fmt.Errorf("timed out after %s", timeout)
	}
}

// interpolate replaces ${VAR} with the workspace's variable set. Unknown
// references never reach here — the validator refuses them — so anything left
// unresolved is a bug worth surfacing rather than silently blanking.
func interpolate(s string, vars map[string]string) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, "${"+k+"}", v)
	}
	return s
}

// envSlice renders the variable set as KEY=VALUE. Declared variables only: the
// relay's own environment is not inherited, so a workspace behaves the same on
// every box.
func envSlice(vars map[string]string) []string {
	out := make([]string, 0, len(vars)+1)
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	// PATH is the one exception: without it `sh -c` cannot find any program at
	// all, which would make every declared command fail identically.
	out = append(out, "PATH="+os.Getenv("PATH"))
	return out
}
