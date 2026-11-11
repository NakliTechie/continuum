package repoinfo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testEnv isolates git from the developer's config and from any GIT_* variable
// the test runner inherited.
func testEnv(t *testing.T) []string {
	home := t.TempDir()
	return append(withoutPrefix(os.Environ(), "GIT_", "HOME", "XDG_CONFIG_HOME"),
		"HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
}

func sh(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T, env []string) string {
	dir := t.TempDir()
	sh(t, env, dir, "init", "-q", "-b", "main")
	return dir
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func reader(env []string) *Reader { r := New(2); r.Env = env; return r }

func TestGitStatusStates(t *testing.T) {
	env := testEnv(t)
	ctx := context.Background()

	t.Run("no directory", func(t *testing.T) {
		for _, d := range []string{"", "relative/path"} {
			if got := reader(env).GitStatus(ctx, d).State; got != StateNoDirectory {
				t.Fatalf("%q: state %q", d, got)
			}
		}
	})

	t.Run("not a repository", func(t *testing.T) {
		got := reader(env).GitStatus(ctx, t.TempDir())
		if got.State != StateNotRepository || len(got.Files) != 0 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("unborn with untracked file", func(t *testing.T) {
		dir := newRepo(t, env)
		write(t, dir, "a.txt", "x")
		got := reader(env).GitStatus(ctx, dir)
		if got.State != StateOK || !got.Unborn || got.Head != "" || got.Branch != "main" {
			t.Fatalf("got %+v", got)
		}
		if got.FilesTotal != 1 || got.Files[0] != (GitFile{Path: "a.txt", Index: "?", Worktree: "?"}) {
			t.Fatalf("files %+v", got.Files)
		}
	})

	t.Run("clean, dirty, staged, renamed", func(t *testing.T) {
		dir := newRepo(t, env)
		write(t, dir, "a.txt", "1")
		write(t, dir, "b.txt", "1")
		write(t, dir, "c.txt", "keep me long enough to be detected as a rename\n")
		sh(t, env, dir, "add", ".")
		sh(t, env, dir, "commit", "-qm", "one")
		clean := reader(env).GitStatus(ctx, dir)
		if clean.State != StateOK || clean.FilesTotal != 0 || clean.Unborn || len(clean.Head) != 7 || clean.Upstream != "" || clean.Ahead != nil {
			t.Fatalf("clean %+v", clean)
		}
		write(t, dir, "a.txt", "2")
		write(t, dir, "b.txt", "2")
		sh(t, env, dir, "add", "b.txt")
		sh(t, env, dir, "mv", "c.txt", "d.txt")
		got := reader(env).GitStatus(ctx, dir)
		want := map[string]GitFile{
			"a.txt": {Path: "a.txt", Index: ".", Worktree: "M"},
			"b.txt": {Path: "b.txt", Index: "M", Worktree: "."},
			"d.txt": {Path: "d.txt", Index: "R", Worktree: "."},
		}
		if got.FilesTotal != 3 {
			t.Fatalf("files %+v", got.Files)
		}
		for _, f := range got.Files {
			if want[f.Path] != f {
				t.Fatalf("file %+v, want %+v", f, want[f.Path])
			}
		}
	})

	t.Run("detached", func(t *testing.T) {
		dir := newRepo(t, env)
		write(t, dir, "a.txt", "1")
		sh(t, env, dir, "add", ".")
		sh(t, env, dir, "commit", "-qm", "one")
		sh(t, env, dir, "checkout", "-q", "--detach")
		got := reader(env).GitStatus(ctx, dir)
		if !got.Detached || got.Branch != "" || len(got.Head) != 7 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("upstream ahead and behind", func(t *testing.T) {
		origin := newRepo(t, env)
		write(t, origin, "a.txt", "1")
		sh(t, env, origin, "add", ".")
		sh(t, env, origin, "commit", "-qm", "one")
		clone := t.TempDir()
		sh(t, env, clone, "clone", "-q", origin, ".")
		write(t, origin, "a.txt", "2")
		sh(t, env, origin, "commit", "-qam", "two")
		sh(t, env, clone, "fetch", "-q")
		write(t, clone, "b.txt", "1")
		sh(t, env, clone, "add", ".")
		sh(t, env, clone, "commit", "-qm", "local")
		got := reader(env).GitStatus(ctx, clone)
		if got.Upstream != "origin/main" || got.Ahead == nil || *got.Ahead != 1 || *got.Behind != 1 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("file limit", func(t *testing.T) {
		dir := newRepo(t, env)
		for i := 0; i < MaxFiles+25; i++ {
			write(t, dir, fmt.Sprintf("f%03d.txt", i), "x")
		}
		got := reader(env).GitStatus(ctx, dir)
		if got.FilesTotal != MaxFiles+25 || len(got.Files) != MaxFiles || !got.Truncated {
			t.Fatalf("total %d listed %d truncated %v", got.FilesTotal, len(got.Files), got.Truncated)
		}
	})

	t.Run("read does not write the index", func(t *testing.T) {
		dir := newRepo(t, env)
		write(t, dir, "a.txt", "1")
		sh(t, env, dir, "add", ".")
		sh(t, env, dir, "commit", "-qm", "one")
		idx := filepath.Join(dir, ".git", "index")
		before, _ := os.Stat(idx)
		// Touch the file so a normal `git status` would refresh the index.
		write(t, dir, "a.txt", "1")
		reader(env).GitStatus(ctx, dir)
		after, _ := os.Stat(idx)
		if !after.ModTime().Equal(before.ModTime()) {
			t.Fatal("git_status rewrote the index")
		}
	})

	t.Run("inherited GIT_DIR is ignored", func(t *testing.T) {
		dir := newRepo(t, env)
		other := newRepo(t, env)
		write(t, dir, "mine.txt", "x")
		got := reader(append(append([]string{}, env...), "GIT_DIR="+filepath.Join(other, ".git"))).GitStatus(ctx, dir)
		if got.FilesTotal != 1 || got.Files[0].Path != "mine.txt" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("busy", func(t *testing.T) {
		r := New(1)
		r.Env = env
		r.sem <- struct{}{}
		if got := r.GitStatus(ctx, t.TempDir()).State; got != StateBusy {
			t.Fatalf("state %q", got)
		}
	})

	t.Run("missing git binary", func(t *testing.T) {
		r := reader(env)
		r.GitPath = filepath.Join(t.TempDir(), "no-git")
		if got := r.GitStatus(ctx, t.TempDir()).State; got != StateUnavailable {
			t.Fatalf("state %q", got)
		}
	})
}
