package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/NakliTechie/continuum/api"
)

type remoteKey struct{}
type remoteTarget struct{ host, binary string }

func validRemoteHost(host string) bool {
	if host == "" || len(host) > 255 || strings.HasPrefix(host, "-") {
		return false
	}
	for _, r := range host {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-@:%[]", r)) {
			return false
		}
	}
	return true
}

func (r remoteTarget) command(dir string, observer bool) string {
	cmd := shellArgument(r.binary) + " rpc --state " + shellArgument(dir)
	if observer {
		cmd += " --observer"
	}
	return cmd
}

// boundedOutput never grows beyond its ceiling. Returning an error aborts the
// copy; CommandContext's deadline/WaitDelay also bound a stuck SSH child.
type boundedOutput struct {
	buffer   bytes.Buffer // named: embedding Buffer promotes ReadFrom, bypassing Write's limit
	limit    int
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		b.overflow = true
		return 0, errors.New("remote response exceeds limit")
	}
	return b.buffer.Write(p)
}
func responseLimit(op string) int {
	switch op {
	case "screen":
		return 32 << 20
	case "events":
		return 16 << 20
	default:
		return 1 << 20
	}
}

func remoteCall(ctx context.Context, target remoteTarget, dir string, observer bool, q api.Request) api.Response {
	fail := func(code, message string) api.Response {
		class := "unreachable"
		if !readOperation(q.Operation) {
			class = "indeterminate"
			message += "; request may have executed, retry only with the same --request-id and arguments"
		}
		return api.Error(q.RequestID, class, code, message, "status")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	body, err := json.Marshal(q)
	if err != nil || len(body) > 128<<10 {
		return api.Error(q.RequestID, "invalid_request", "request_size", "request exceeds 128 KiB", "help")
	}
	cmd := exec.CommandContext(ctx, "ssh", "-T", "-oBatchMode=yes", "-oStrictHostKeyChecking=yes", "-oConnectTimeout=8", "-oClearAllForwardings=yes", "-oForwardAgent=no", "-oForwardX11=no", "--", target.host, target.command(dir, observer))
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(body)
	stdout := &boundedOutput{limit: responseLimit(q.Operation)}
	stderr := &boundedOutput{limit: 16 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err = cmd.Run(); err != nil {
		// Do not print SSH stderr: it may contain untrusted terminal escapes or
		// sensitive remote configuration. The caller can diagnose SSH separately.
		return fail("ssh_transport", "SSH bridge failed; check the host key, key authentication and remote binary/state")
	}
	var v api.Response
	dec := json.NewDecoder(&stdout.buffer)
	if stdout.overflow || stderr.overflow || dec.Decode(&v) != nil || dec.Decode(new(any)) != io.EOF || v.Version != 1 || v.Class == "" || (v.RequestID != q.RequestID && (v.Class == "ok" || v.RequestID != "")) {
		return fail("ssh_response", "remote bridge returned an invalid or oversized envelope")
	}
	return v
}

// rpc is deliberately a one-shot stdio bridge, not an authority-bearing remote
// listener. SSH owns authentication; local /v1 still enforces the selected role.
func bridge(ctx context.Context, dir string, observer bool, in io.Reader, out io.Writer) int {
	body, err := io.ReadAll(io.LimitReader(in, (128<<10)+1))
	var q api.Request
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err != nil || len(body) > 128<<10 || dec.Decode(&q) != nil || dec.Decode(new(any)) != io.EOF || q.Operation == "" {
		_ = json.NewEncoder(out).Encode(api.Error("", "invalid_request", "request", "one bounded JSON request required", "help"))
		return 0
	}
	if err = json.NewEncoder(out).Encode(call(ctx, dir, observer, q)); err != nil {
		return 5
	}
	// Envelope class carries the API exit status; nonzero means bridge failure.
	return 0
}

func leaseDirectory(ctx context.Context, state string) (string, error) {
	target, remote := ctx.Value(remoteKey{}).(remoteTarget)
	if !remote {
		return state, nil
	}
	key := sha256.Sum256([]byte(target.host + "\x00" + target.binary + "\x00" + state))
	base := defaultState()
	if !filepath.IsAbs(base) {
		return "", errors.New("no private local config directory for remote leases")
	}
	// Refuse permissive directories and symlinks at every cache-owned level.
	for _, path := range []string{base, filepath.Join(base, "remotes"), filepath.Join(base, "remotes", hex.EncodeToString(key[:]))} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return "", err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return "", errors.New("remote lease cache must be a private directory, not a symlink")
		}
		base = path
	}
	return base, nil
}

func requireCapability(ctx context.Context, dir string, observer bool, capability string) api.Response {
	v := call(ctx, dir, observer, api.Request{Operation: "status"})
	if v.Class != "ok" {
		return v
	}
	var status struct {
		Capabilities []string `json:"capabilities"`
	}
	if json.Unmarshal(v.Result, &status) == nil {
		for _, c := range status.Capabilities {
			if c == capability {
				return v
			}
		}
	}
	return api.Error("", "unsupported", capability, "daemon does not advertise "+capability+"; upgrade the owning host first", "contract")
}
