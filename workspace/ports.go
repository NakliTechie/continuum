package workspace

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// lockFile serialises allocate-and-bind across every process using this relay
// home. Two workspaces materialising concurrently must not receive the same
// port, and a bind test that is not held under a lock is a race with a window
// exactly as wide as the caller's next few instructions.
const lockFile = "ports.lock"

// withPortLock runs fn while holding an exclusive advisory lock on the relay
// home's port lock file. The lock is per open file description, so each call
// opens its own descriptor and goroutines in one process serialise the same way
// separate processes do.
func withPortLock(home string, fn func() error) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(home, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking %s: %w", lockFile, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// freePort returns the first port in [low, high] that is not already recorded to
// another workspace and that actually accepts a bind right now. Never trust a
// static table: a port can be held by anything on the box, not just by us.
func freePort(low, high int, taken map[int]bool) (int, error) {
	for p := low; p <= high; p++ {
		if taken[p] {
			continue
		}
		if bindable(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in range [%d, %d]", low, high)
}

// bindable reports whether nothing on the box holds the port. Loopback alone
// is not enough: with SO_REUSEADDR, which Go sets, macOS lets 127.0.0.1:p bind
// while a wildcard listener (Docker's published ports, most dev servers) holds
// 0.0.0.0:p. The wildcard and the IPv6 loopback are probed as well; an
// unsupported family counts as free.
func bindable(p int) bool {
	for _, addr := range []string{fmt.Sprintf("127.0.0.1:%d", p), fmt.Sprintf(":%d", p), fmt.Sprintf("[::1]:%d", p)} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			if isUnsupported(err) {
				continue
			}
			return false
		}
		_ = ln.Close()
	}
	return true
}

func isUnsupported(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "address family not supported") || strings.Contains(msg, "cannot assign requested address") || strings.Contains(msg, "protocol not available")
}
