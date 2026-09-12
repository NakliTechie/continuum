// Package ipc names and opens the daemon's local API socket. The modern API
// lives on a Unix socket inside the private state directory: filesystem
// permissions are the credential boundary a local-first runtime already has,
// and no stale address file can ever point a client's bearer at a port that
// something else has since bound. The path carries the contract version so an
// older CLI meets no socket rather than a daemon it cannot talk to.
package ipc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// SocketName is the modern API socket inside a state directory.
const SocketName = "v1.sock"

// maxPath keeps the socket address under the shortest sun_path limit in use
// (104 bytes on macOS); longer state directories get a short private alias.
const maxPath = 96

// Path is the socket the CLI dials for the given state directory.
func Path(dir string) string { return filepath.Join(dir, SocketName) }

// ErrServing reports that another daemon already answers on the socket.
var ErrServing = errors.New("another daemon is serving this state directory")

// Listener is the daemon's socket plus whatever it had to create to bind it.
type Listener struct {
	net.Listener
	path  string // the address clients dial, inside the state directory
	alias string // a short private directory when the state path is too long
}

// Listen binds the state directory's API socket. A socket left behind by a
// crashed daemon is removed when nothing answers on it; a live daemon is
// refused. The socket file is private to the user.
func Listen(dir string) (*Listener, error) {
	path := Path(dir)
	if err := clearStale(path); err != nil {
		return nil, err
	}
	target, alias := path, ""
	if len(path) > maxPath {
		// Bind at a short private path and leave a symlink where clients look;
		// connect follows the link. The alias directory is 0700.
		d, err := shortTempDir()
		if err != nil {
			return nil, err
		}
		alias, target = d, filepath.Join(d, SocketName)
		if err := os.Symlink(target, path); err != nil {
			os.RemoveAll(d)
			return nil, err
		}
	}
	old := syscall.Umask(0o077)
	ln, err := net.Listen("unix", target)
	syscall.Umask(old)
	if err != nil {
		if alias != "" {
			os.Remove(path)
			os.RemoveAll(alias)
		}
		return nil, err
	}
	if err := os.Chmod(target, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return &Listener{Listener: ln, path: path, alias: alias}, nil
}

// Close stops listening and removes the socket (and alias) it created.
func (l *Listener) Close() error {
	err := l.Listener.Close()
	if l.alias != "" {
		os.Remove(l.path)
		os.RemoveAll(l.alias)
	}
	return err
}

// ErrForeignLink reports a symlink at the socket path that this package did
// not create. connect(2) follows symlinks, so such a link is never dialed: a
// client refuses, and the next daemon start replaces it.
var ErrForeignLink = errors.New("state socket path is a symlink this daemon did not create")

// Dial connects to the state directory's API socket. A socket address must
// fit sockaddr_un as written, so when the daemon left a symlink to a short
// alias, the alias is what gets dialed.
func Dial(ctx context.Context, dir string) (net.Conn, error) {
	target, err := resolve(Path(dir))
	if err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", target)
}

// resolve follows only the symlink shape this package creates: a link to
// `<alias>/v1.sock` inside a `continuum-ipc-*` directory directly under a temp
// root. Any other link is refused rather than followed.
func resolve(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	target, err := os.Readlink(path)
	if err != nil || !isAlias(target) {
		return "", ErrForeignLink
	}
	return target, nil
}

// isAlias reports whether target is a socket path this package would create.
func isAlias(target string) bool {
	if filepath.Base(target) != SocketName || !filepath.IsAbs(target) {
		return false
	}
	dir := filepath.Dir(target)
	if !strings.HasPrefix(filepath.Base(dir), "continuum-ipc-") {
		return false
	}
	parent := filepath.Dir(dir)
	return parent == "/tmp" || parent == filepath.Clean(os.TempDir())
}

// Absent reports whether no socket exists for the state directory at all,
// which tells a client the daemon was never started (or exited cleanly).
func Absent(dir string) bool {
	_, err := os.Lstat(Path(dir))
	return os.IsNotExist(err)
}

// clearStale removes a socket nobody answers on and refuses a live one.
func clearStale(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 && info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	target, err := resolve(path)
	if err != nil {
		// A foreign link is never followed, not even to ask whether a daemon
		// answers; it is replaced and its target left alone.
		return os.Remove(path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if c, err := (&net.Dialer{}).DialContext(ctx, "unix", target); err == nil {
		c.Close()
		return ErrServing
	}
	if target != path {
		os.Remove(target) // an alias this package created, behind the link
		os.RemoveAll(filepath.Dir(target))
	}
	return os.Remove(path)
}

// shortTempDir makes a private directory whose path is short enough for a
// socket even when TMPDIR itself is long (test harnesses redirect it deep).
func shortTempDir() (string, error) {
	for _, base := range []string{"/tmp", os.TempDir()} {
		if d, err := os.MkdirTemp(base, "continuum-ipc-"); err == nil {
			if len(filepath.Join(d, SocketName)) <= maxPath {
				return d, nil
			}
			os.RemoveAll(d)
		}
	}
	return "", errors.New("no directory short enough for a socket path")
}
