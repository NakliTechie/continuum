package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/protocol"
)

// Protocol 1.5: hello advertises the repository capabilities; git_status and
// session_pr need the session token and read the recorded spawn directory;
// github_inbox runs on registration authority; no read changes session status.
func TestRepoReadsOverProtocol(t *testing.T) {
	cfg := &config.Config{RegistrationToken: "t", Tmux: "off", Agents: map[string]config.Agent{"custom": {}}}
	s := New(cfg)
	ghDir := t.TempDir()
	gh := filepath.Join(ghDir, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\ncase \"$*\" in\n*search*) echo '[]';;\n*\"pr view\"*) echo 'no pull requests found for branch \"main\"' >&2; exit 1;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.repo.GHPath = gh

	h := s.hello()
	for _, c := range []string{protocol.CapSessionGit, protocol.CapGitHubInbox, protocol.CapSessionPR} {
		if !slices.Contains(h.Capabilities, c) {
			t.Fatalf("hello capabilities %v lack %s", h.Capabilities, c)
		}
	}
	if h.ProtocolVersion != "1.3" {
		t.Fatalf("protocol_version %q: 1.5 reads are capability-gated, the version stays at the highest fully implemented", h.ProtocolVersion)
	}

	repo := t.TempDir()
	env := append(os.Environ(), "HOME="+t.TempDir())
	for _, args := range [][]string{{"init", "-q", "-b", "main"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	frames := make(chan map[string]any, 64)
	cn := &conn{srv: s, ctx: context.Background(), registered: true, sink: func(b []byte) error {
		var f map[string]any
		_ = json.Unmarshal(b, &f)
		frames <- f
		return nil
	}}
	next := func(typ string) map[string]any {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case f := <-frames:
				if f["type"] == typ {
					return f
				}
			case <-deadline:
				t.Fatalf("no %s frame", typ)
			}
		}
	}
	cn.handleSpawnPTY(protocol.Spawn{Agent: "custom", Args: []string{"/bin/cat"}, Cwd: repo})
	sp := next("spawned")
	id, tok := sp["session_id"].(string), sp["session_token"].(string)
	t.Cleanup(func() { s.killEntry(id) })
	e := s.entry(id)
	e.setStatus(protocol.StatusDone)

	send := func(v any) {
		b, _ := json.Marshal(v)
		var env protocol.Envelope
		_ = json.Unmarshal(b, &env)
		cn.dispatch(env, b)
	}

	send(protocol.RepoRequest{Type: protocol.TypeGitStatus, SessionID: id, SessionToken: tok, RequestID: "g-1"})
	g := next("git_status_result")
	if g["request_id"] != "g-1" || g["session_id"] != id || g["state"] != "ok" || g["unborn"] != true || g["branch"] != "main" || g["files_total"].(float64) != 1 {
		t.Fatalf("git_status_result %v", g)
	}

	send(protocol.RepoRequest{Type: protocol.TypeGitStatus, SessionID: id, SessionToken: "wrong", RequestID: "g-2"})
	if f := next("error"); f["code"] != protocol.ErrInvalidToken {
		t.Fatalf("bad token answered %v", f)
	}
	send(protocol.RepoRequest{Type: protocol.TypeGitStatus, SessionID: id, SessionToken: tok})
	if f := next("error"); f["code"] != "bad_message" {
		t.Fatalf("missing request_id answered %v", f)
	}

	send(protocol.RepoRequest{Type: protocol.TypeSessionPR, SessionID: id, SessionToken: tok, RequestID: "p-1"})
	if p := next("session_pr_result"); p["state"] != "no_pr" || p["request_id"] != "p-1" {
		t.Fatalf("session_pr_result %v", p)
	}
	send(protocol.RepoRequest{Type: protocol.TypeGitHubInbox, RequestID: "i-1"})
	in := next("github_inbox_result")
	if in["state"] != "ok" || in["review_requested"] == nil || in["assigned"] == nil {
		t.Fatalf("github_inbox_result %v", in)
	}
	send(protocol.RepoRequest{Type: protocol.TypeGitHubItem, RequestID: "i-2", Kind: "issue", Repo: "--help/x", Number: 1})
	if it := next("github_item_result"); it["state"] != "not_found" {
		t.Fatalf("github_item_result %v", it)
	}

	if st := e.currentStatus(); st != protocol.StatusDone {
		t.Fatalf("a repository read changed status to %q", st)
	}

	// An unregistered connection gets nothing.
	cold := &conn{srv: s, ctx: context.Background(), sink: cn.sink}
	b, _ := json.Marshal(protocol.RepoRequest{Type: protocol.TypeGitHubInbox, RequestID: "i-3"})
	cold.dispatch(protocol.Envelope{Type: protocol.TypeGitHubInbox}, b)
	if f := next("error"); f["code"] != protocol.ErrAuthFailed {
		t.Fatalf("unregistered inbox answered %v", f)
	}
}

func TestRepoReadWithoutRecordedDirectory(t *testing.T) {
	cfg := &config.Config{RegistrationToken: "t", Tmux: "off", Agents: map[string]config.Agent{"custom": {}}}
	s := New(cfg)
	if absDir("relative") != "" || absDir("") != "" || absDir("/a/../b") != "/b" {
		t.Fatal("absDir")
	}
	got := s.repo.GitStatus(context.Background(), "")
	if got.State != "no_directory" {
		t.Fatalf("state %q", got.State)
	}
}
