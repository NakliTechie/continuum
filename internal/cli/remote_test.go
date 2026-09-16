package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/api"
)

func TestRemoteQuotingAndValidation(t *testing.T) {
	target := remoteTarget{"owner@example", "/tmp/remote binary ' $(false)"}
	dir := "/tmp/state ' ; $(false)"
	// Ask a POSIX shell to parse the command without running its executable.
	cmd := exec.Command("/bin/sh", "-c", "set -- "+target.command(dir, true)+"; printf '%s\\n' \"$@\"")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := target.binary + "\nrpc\n--state\n" + dir + "\n--observer\n"
	if string(out) != want {
		t.Fatalf("quoted argv = %q, want %q", out, want)
	}
	for _, host := range []string{"-oProxyCommand=evil", "a b", "a;echo evil", "a\n", ""} {
		if validRemoteHost(host) {
			t.Fatalf("accepted %q", host)
		}
	}
	for _, host := range []string{"user@192.0.2.1", "alias", "user@[::1]"} {
		if !validRemoteHost(host) {
			t.Fatalf("refused %q", host)
		}
	}
	for _, args := range [][]string{
		{"status", "--host", "example"}, {"status", "--host", "-evil", "--state", "/tmp/none"},
		{"serve", "--host", "example", "--state", "/tmp/none"},
		{"service", "install", "--host", "example", "--state", "/tmp/none"},
		{"compact", "--host", "example", "--state", "/tmp/none"},
		{"rpc", "--host", "example", "--state", "/tmp/none"},
		{"status", "--remote-binary", "evil"}, {"service", "install", "--browse-root", "/tmp"},
		{"open", "--host", "example", "--state", "/tmp/none", "--", "/bin/cat"},
	} {
		var out, diag bytes.Buffer
		if code := Run(args, strings.NewReader(""), &out, &diag); code != 2 {
			t.Fatalf("%v: %d %s %s", args, code, &out, &diag)
		}
	}
}

func TestRemoteLeaseCacheIsPrivateAndSeparated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	target := remoteTarget{"owner@host", "continuum"}
	ctx := context.WithValue(context.Background(), remoteKey{}, target)
	cache, err := leaseDirectory(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if cache == dir {
		t.Fatal("remote lease saved beside a local state")
	}
	info, err := os.Stat(cache)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("unsafe cache: %v %v", info, err)
	}
	target.host = "other-host"
	other, err := leaseDirectory(context.WithValue(context.Background(), remoteKey{}, target), dir)
	if err != nil || other == cache {
		t.Fatal("host lease caches overlap", err)
	}
	if err := os.Chmod(other, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := leaseDirectory(context.WithValue(context.Background(), remoteKey{}, target), dir); err == nil {
		t.Fatal("permissive cache accepted")
	}
}

func TestBridgeAndCapabilityNegotiation(t *testing.T) {
	dir, _ := newAttachFixture(t)
	var out bytes.Buffer
	if code := bridge(context.Background(), dir, false, strings.NewReader(`{"operation":"status"}`), &out); code != 0 {
		t.Fatal(code)
	}
	var r api.Response
	if json.Unmarshal(out.Bytes(), &r) != nil || r.Class != "ok" {
		t.Fatal(out.String())
	}
	if r := requireCapability(context.Background(), dir, false, "directories_v1"); r.Class != "unsupported" {
		t.Fatal(r)
	}
	for _, payload := range []string{`{} {}`, `{"operation":"status","unknown":1}`, strings.Repeat("x", 128<<10+1)} {
		out.Reset()
		bridge(context.Background(), dir, false, strings.NewReader(payload), &out)
		if json.Unmarshal(out.Bytes(), &r) != nil || r.Class != "invalid_request" {
			t.Fatal(out.String())
		}
	}
}

func fakeSSH(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}
func TestSSHResponseBoundsCancellationAndAmbiguity(t *testing.T) {
	target := remoteTarget{"host", "continuum"}
	ctx := context.Background()
	t.Run("failure", func(t *testing.T) {
		fakeSSH(t, "exit 255\n")
		for _, op := range []string{"status", "directories", "open"} {
			r := remoteCall(ctx, target, "/remote/state", false, api.Request{Operation: op, RequestID: "once"})
			want := "unreachable"
			if op == "open" {
				want = "indeterminate"
			}
			if r.Class != want || r.RequestID != "once" {
				t.Fatal(r)
			}
		}
	})
	t.Run("multiple", func(t *testing.T) {
		fakeSSH(t, `printf '%s\n' '{"schema_version":1,"class":"ok"}' '{}'`)
		if r := remoteCall(ctx, target, "/remote/state", false, api.Request{Operation: "status"}); r.Code != "ssh_response" {
			t.Fatal(r)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		fakeSSH(t, "exec sleep 30\n")
		c, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		if r := remoteCall(c, target, "/remote/state", false, api.Request{Operation: "open", RequestID: "once"}); r.Class != "indeterminate" {
			t.Fatal(r)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("SSH cancellation stalled")
		}
	})
	t.Run("bounds", func(t *testing.T) {
		b := &boundedOutput{limit: 3}
		_, _ = b.Write([]byte("abc"))
		if _, err := b.Write([]byte("d")); err == nil || !b.overflow || b.buffer.Len() != 3 {
			t.Fatal("response bound failed")
		}
		// os/exec copies from pipes: no promoted ReaderFrom may bypass Write.
		b = &boundedOutput{limit: 3}
		if _, err := io.Copy(b, io.LimitReader(strings.NewReader("12345"), 5)); err == nil || !b.overflow || b.buffer.Len() > 3 {
			t.Fatal("io.Copy bypassed response bound")
		}
	})
}
