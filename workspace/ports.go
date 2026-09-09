package workspace

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
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
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue // in use by something outside our record
		}
		_ = ln.Close()
		return p, nil
	}
	return 0, fmt.Errorf("no free port in range [%d, %d]", low, high)
}

// listenOn is a test seam: it binds a port so a test can prove the allocator
// skips ports held outside its own record.
func listenOn(port int) (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
}
