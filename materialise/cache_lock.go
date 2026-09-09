package materialise

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// A separate open description per call serializes both processes and goroutines.
// Service-scope locks cover admission/effect/marker; the short cache-write lock
// covers the shared JSON read-modify-write, preserving unrelated scope entries.
func (e *Engine) lockCacheScope(scope string, timeout time.Duration) (func(), error) {
	root := filepath.Join(e.Prov.Home, "materialise-locks")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(scope))
	f, err := os.OpenFile(filepath.Join(root, fmt.Sprintf("%x.lock", sum)), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN && err != syscall.EINTR {
			f.Close()
			return nil, fmt.Errorf("materialise lock: %w", err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("materialise lock timed out; another run still owns this scope")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
