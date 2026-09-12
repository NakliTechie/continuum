package ipc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestListenIsPrivateAndDialable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	target, err := Resolve(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("socket must exist and be private: %v %v", info, err)
	}
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Write([]byte("hi"))
			c.Close()
		}
	}()
	c, err := Dial(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 2)
	if _, err := c.Read(b); err != nil || string(b) != "hi" {
		t.Fatalf("dial: %q %v", b, err)
	}
	c.Close()
	if Absent(dir) {
		t.Fatal("socket reported absent while listening")
	}
}

// A crashed daemon leaves its socket behind; the next serve removes it. A live
// daemon is refused rather than displaced.
func TestListenClearsStaleAndRefusesLive(t *testing.T) {
	dir := t.TempDir()
	first, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(dir); err != ErrServing {
		t.Fatalf("live daemon must be refused, got %v", err)
	}
	// Simulate a crash: the listening socket goes away without unlinking.
	first.Listener.(*net.UnixListener).SetUnlinkOnClose(false)
	first.Listener.Close()
	if stale, _ := Resolve(Path(dir)); stale == "" {
		t.Fatal("resolve failed on our own socket")
	} else if _, err := os.Lstat(stale); err != nil {
		t.Fatalf("stale socket should still be on disk: %v", err)
	}
	second, err := Listen(dir)
	if err != nil {
		t.Fatalf("stale socket must be cleared: %v", err)
	}
	second.Close()
	if !Absent(dir) {
		t.Fatal("close must remove the socket")
	}
}

// State directories deeper than sockaddr_un allows get a short private alias
// behind a symlink; clients dial the alias and the daemon cleans both up.
func TestLongStatePathUsesAnAlias(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("d", 120))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	if target, err := Resolve(Path(dir)); err != nil || ln.alias == "" || len(target) > maxPath {
		t.Fatalf("expected a short alias, got %q %v", target, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Dial(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	alias := ln.alias
	ln.Close()
	if _, err := os.Stat(alias); !os.IsNotExist(err) {
		t.Fatalf("alias directory not removed: %v", err)
	}
	if !Absent(dir) {
		t.Fatal("symlink not removed")
	}
}

// A symlink at the socket path that points anywhere but an alias this package
// created is neither followed by clients nor has its target removed by the
// daemon: the link itself is replaced and the foreign socket is untouched.
func TestForeignSymlinkIsNeitherFollowedNorRemoved(t *testing.T) {
	foreignDir, err := os.MkdirTemp("/tmp", "foreign-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(foreignDir)
	foreign := filepath.Join(foreignDir, "other.sock")
	fl, err := net.Listen("unix", foreign)
	if err != nil {
		t.Fatal(err)
	}
	defer fl.Close()
	dir := t.TempDir()
	if err := os.Symlink(foreign, Path(dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(context.Background(), dir); err != ErrForeignLink {
		t.Fatalf("client must refuse a foreign link, got %v", err)
	}
	ln, err := Listen(dir)
	if err != nil {
		t.Fatalf("daemon must replace a foreign link: %v", err)
	}
	defer ln.Close()
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign socket was removed: %v", err)
	}
	if info, err := os.Lstat(Path(dir)); err != nil || info.Mode()&os.ModeSymlink != 0 && !isAlias(mustReadlink(t, Path(dir))) {
		t.Fatalf("socket path is still a foreign link: %v %v", info, err)
	}
	if c, err := Dial(context.Background(), dir); err != nil {
		t.Fatalf("the replaced socket must be dialable: %v", err)
	} else {
		c.Close()
	}
	// And a crafted "continuum-ipc-" directory outside a temp root is not an alias.
	crafted := filepath.Join(t.TempDir(), "continuum-ipc-x", SocketName)
	if isAlias(crafted) {
		t.Fatal("alias shape accepted outside a temp root")
	}
}

func mustReadlink(t *testing.T, p string) string {
	t.Helper()
	target, err := os.Readlink(p)
	if err != nil {
		t.Fatal(err)
	}
	return target
}
