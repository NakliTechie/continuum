package workspace

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestO5ExistingWorktreeIdentity(t *testing.T) {
	for _, kind := range []string{"matching_identity_positive", "different_repo", "different_branch"} {
		t.Run(kind, func(t *testing.T) {
			repo := testRepo(t)
			p := New(t.TempDir())
			path := filepath.Join(p.Root, "w1")
			actualRepo, actualBranch := repo, "agent/w1"
			if kind == "different_repo" {
				actualRepo = testRepo(t)
			}
			if kind == "different_branch" {
				actualBranch = "unrelated-branch"
			}
			if err := p.ensureWorktree(actualRepo, path, actualBranch); err != nil {
				t.Fatal(err)
			}
			err := p.ensureWorktree(repo, path, "agent/w1")
			cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
			cmd.Dir = path
			common, cerr := cmd.Output()
			if cerr != nil {
				t.Fatal(cerr)
			}
			cmd = exec.Command("git", "symbolic-ref", "--short", "HEAD")
			cmd.Dir = path
			branch, cerr := cmd.Output()
			if cerr != nil {
				t.Fatal(cerr)
			}
			t.Logf("requested_repo=%s actual_common_dir=%s requested_branch=agent/w1 actual_branch=%s error=%v", repo, strings.TrimSpace(string(common)), strings.TrimSpace(string(branch)), err)
			if kind == "matching_identity_positive" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("accepted an existing worktree with a different repository or branch")
			}
		})
	}
}
