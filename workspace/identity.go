package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func workspaceGit(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE":
			continue
		}
		cmd.Env = append(cmd.Env, env)
	}
	return cmd
}

func worktreePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if os.IsNotExist(err) {
		return absolute, nil
	}
	return resolved, err
}

func sameWorktreePath(a, b string) bool {
	left, e1 := worktreePath(a)
	right, e2 := worktreePath(b)
	return e1 == nil && e2 == nil && left == right
}

func gitIdentity(dir string, args ...string) (string, error) {
	out, err := workspaceGit(dir, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("inspect worktree %q: %w: %s", dir, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Check live Git identity before any recorded workspace is reused. A .git
// marker alone does not establish the repository, branch, or registered path.
func validateExistingWorktree(repo, path, branch string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(path, ".git")); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	actual, err := gitIdentity(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return true, err
	}
	expected, err := gitIdentity(repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return true, err
	}
	if !sameWorktreePath(actual, expected) {
		return true, fmt.Errorf("workspace %q belongs to a different Git repository", path)
	}
	actualBranch, err := gitIdentity(path, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return true, err
	}
	if actualBranch != "refs/heads/"+branch {
		return true, fmt.Errorf("workspace %q is on branch %q, requested %q", path, actualBranch, branch)
	}
	top, err := gitIdentity(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return true, err
	}
	if !sameWorktreePath(top, path) {
		return true, fmt.Errorf("workspace %q is not the Git worktree root", path)
	}
	listing, err := gitIdentity(repo, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return true, err
	}
	candidate, candidateBranch := "", ""
	for _, field := range strings.Split(listing, "\x00") {
		if strings.HasPrefix(field, "worktree ") {
			candidate = strings.TrimPrefix(field, "worktree ")
			candidateBranch = ""
		}
		if strings.HasPrefix(field, "branch ") {
			candidateBranch = strings.TrimPrefix(field, "branch ")
		}
		if candidate != "" && candidateBranch == "refs/heads/"+branch && sameWorktreePath(candidate, path) {
			return true, nil
		}
	}
	return true, fmt.Errorf("workspace %q is not registered with the requested repository and branch", path)
}
