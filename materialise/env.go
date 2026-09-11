// Package materialise executes a spec's materialise block against a provisioned
// workspace, in the one fixed order the handoff specifies:
//
//	ports -> files -> commands -> services -> health -> escape
//
// The order is not configurable: a file that needs a port must be able to read
// it, and a command that needs a file must run after it.
package materialise

import (
	"bytes"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// FileSystem is the small surface the engine needs: read a source, write a
// file that must stay inside a workspace root. It is an interface so the test
// suite can substitute a recorder and assert "no filesystem writes occurred".
type FileSystem interface {
	ReadFile(path string) ([]byte, error)
	WriteFileWithin(root, relative string, b []byte, perm fs.FileMode) error
	// Resolve returns the path with every symlink followed, so a source policy
	// judges where a file really is rather than where a link says it is.
	Resolve(path string) (string, error)
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
func (OSFileSystem) Resolve(p string) (string, error)  { return filepath.EvalSymlinks(p) }

// ShellExecutor runs command lines through `sh -c`, which is what a declared
// `run` string means. A timeout of 0 means no timeout.
type ShellExecutor struct{}

func (ShellExecutor) Run(dir string, env []string, cmdline string, timeout time.Duration) ([]byte, error) {
	cmd := exec.Command("sh", "-c", cmdline)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if timeout <= 0 {
		err := <-done
		return output.Bytes(), err
	}
	select {
	case err := <-done:
		return output.Bytes(), err
	case <-time.After(timeout):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return output.Bytes(), fmt.Errorf("timed out after %s", timeout)
	}
}

func (OSFileSystem) WriteFileWithin(root, relative string, b []byte, perm fs.FileMode) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	if err = r.MkdirAll(filepath.Dir(relative), 0755); err != nil {
		return err
	}
	f, err := r.OpenFile(relative, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	ce := f.Close()
	if err != nil {
		return err
	}
	return ce
}

// interpolate replaces ${VAR} with the workspace's variable set. Unknown
// references never reach here — the validator refuses them — so anything left
// unresolved is a bug worth surfacing rather than silently blanking.
func interpolate(s string, vars map[string]string) string {
	// One pass over the input, never over substituted text: a value that
	// itself contains ${...} is data, and map order must not decide whether
	// it expands.
	var out strings.Builder
	for {
		start := strings.Index(s, "${")
		if start < 0 {
			break
		}
		end := strings.Index(s[start:], "}")
		if end < 0 {
			break
		}
		name := s[start+2 : start+end]
		v, ok := vars[name]
		out.WriteString(s[:start])
		if ok {
			out.WriteString(v)
		} else {
			out.WriteString(s[start : start+end+1])
		}
		s = s[start+end+1:]
	}
	out.WriteString(s)
	return out.String()
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
