package repoinfo

import (
	"bytes"
	"context"
	"path/filepath"
	"strconv"
	"strings"
)

// MaxFiles bounds the changed-file list in a Git snapshot.
const MaxFiles = 200

const maxPathLen = 1024

// GitFile is one changed path with its porcelain-v2 status letters.
type GitFile struct {
	Path     string `json:"path"`
	Index    string `json:"index"`
	Worktree string `json:"worktree"`
}

// GitSnapshot is the protocol's git_status_result body (without envelope fields).
type GitSnapshot struct {
	State      string    `json:"state"`
	Branch     string    `json:"branch,omitempty"`
	Detached   bool      `json:"detached"`
	Unborn     bool      `json:"unborn"`
	Head       string    `json:"head,omitempty"`
	Upstream   string    `json:"upstream,omitempty"`
	Ahead      *int      `json:"ahead,omitempty"`
	Behind     *int      `json:"behind,omitempty"`
	Files      []GitFile `json:"files"`
	FilesTotal int       `json:"files_total"`
	Truncated  bool      `json:"truncated"`
}

// gitEnv removes inherited GIT_* variables (a GIT_DIR or GIT_INDEX_FILE from
// the daemon's environment would point the read elsewhere) and turns off
// optional locks and prompts, so status never rewrites the index or asks for
// credentials.
func (r *Reader) gitEnv() []string {
	env := withoutPrefix(r.baseEnv(), "GIT_")
	return append(env,
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_CONFIG_NOSYSTEM=1",
		"LC_ALL=C",
	)
}

// GitStatus reads a bounded, read-only snapshot of dir. dir must be the
// directory the runtime recorded at spawn; "" answers no_directory.
func (r *Reader) GitStatus(ctx context.Context, dir string) GitSnapshot {
	if dir == "" || !filepath.IsAbs(dir) {
		return GitSnapshot{State: StateNoDirectory}
	}
	if !r.acquire() {
		return GitSnapshot{State: StateBusy}
	}
	defer r.release()
	git := r.GitPath
	if git == "" {
		git = "git"
	}
	env := r.gitEnv()
	args := []string{"-c", "core.fsmonitor=false"}
	args = append(args, r.filterOverrides(ctx, git, dir, env)...)
	args = append(args,
		"-c", "core.untrackedCache=false",
		"-c", "status.showUntrackedFiles=normal",
		"-c", "diff.external=",
		"--no-pager", "--no-optional-locks",
		"status", "--porcelain=v2", "--branch", "-z", "--untracked-files=normal", "--ignore-submodules=dirty")
	res := r.run(ctx, gitDeadline, dir, env, git, args...)
	switch {
	case res.timedOut:
		return GitSnapshot{State: StateTimeout}
	case res.startErr != nil:
		return GitSnapshot{State: StateUnavailable}
	case res.code != 0:
		if strings.Contains(strings.ToLower(res.stderr), "not a git repository") {
			return GitSnapshot{State: StateNotRepository}
		}
		return GitSnapshot{State: StateUnavailable}
	}
	out := res.stdout
	if res.overflow {
		// The last -z record may end mid-path; drop it.
		if i := bytes.LastIndexByte(out, 0); i >= 0 {
			out = out[:i]
		} else {
			out = nil
		}
	}
	snap := parsePorcelainV2(out)
	if res.overflow {
		snap.Truncated = true
	}
	return snap
}

// filterOverrides blanks every content filter driver that the repository's
// own configuration (local, worktree or command scope) defines, because
// `git status` runs a clean filter to re-check a file whose stat data is out
// of date. Drivers from the user's global configuration (git-lfs and the
// like) are the machine owner's choice and stay on. Listing config runs no
// filter.
func (r *Reader) filterOverrides(ctx context.Context, git, dir string, env []string) []string {
	res := r.run(ctx, gitDeadline, dir, env, git, "config", "--null", "--show-scope", "--name-only", "--get-regexp", `^filter\.`)
	if res.code != 0 || res.timedOut {
		return nil
	}
	parts := bytes.Split(res.stdout, []byte{0})
	seen := map[string]bool{}
	var out []string
	for i := 0; i+1 < len(parts); i += 2 {
		scope, name := string(parts[i]), string(parts[i+1])
		if scope == "global" || scope == "system" {
			continue
		}
		key := strings.TrimPrefix(name, "filter.")
		dot := strings.LastIndexByte(key, '.')
		if dot <= 0 {
			continue
		}
		driver := key[:dot]
		if seen[driver] {
			continue
		}
		seen[driver] = true
		out = append(out, "-c", "filter."+driver+".clean=", "-c", "filter."+driver+".smudge=", "-c", "filter."+driver+".process=", "-c", "filter."+driver+".required=false")
	}
	return out
}

// parsePorcelainV2 parses `git status --porcelain=v2 --branch -z` output.
func parsePorcelainV2(out []byte) GitSnapshot {
	snap := GitSnapshot{State: StateOK, Files: []GitFile{}}
	recs := bytes.Split(out, []byte{0})
	add := func(path, x, y string) {
		snap.FilesTotal++
		if len(snap.Files) >= MaxFiles {
			snap.Truncated = true
			return
		}
		path, _ = cut(path, maxPathLen)
		snap.Files = append(snap.Files, GitFile{Path: path, Index: x, Worktree: y})
	}
	for i := 0; i < len(recs); i++ {
		rec := string(recs[i])
		if rec == "" {
			continue
		}
		switch {
		case strings.HasPrefix(rec, "# branch.oid "):
			oid := strings.TrimPrefix(rec, "# branch.oid ")
			if oid == "(initial)" {
				snap.Unborn = true
			} else if len(oid) >= 7 {
				snap.Head = oid[:7]
			}
		case strings.HasPrefix(rec, "# branch.head "):
			h := strings.TrimPrefix(rec, "# branch.head ")
			if h == "(detached)" {
				snap.Detached = true
			} else {
				snap.Branch = h
			}
		case strings.HasPrefix(rec, "# branch.upstream "):
			snap.Upstream = strings.TrimPrefix(rec, "# branch.upstream ")
		case strings.HasPrefix(rec, "# branch.ab "):
			var a, b int
			for _, f := range strings.Fields(strings.TrimPrefix(rec, "# branch.ab ")) {
				n, err := strconv.Atoi(f[1:])
				if err != nil {
					continue
				}
				if f[0] == '+' {
					a = n
				} else if f[0] == '-' {
					b = n
				}
			}
			snap.Ahead, snap.Behind = &a, &b
		case strings.HasPrefix(rec, "1 "):
			// 1 XY sub mH mI mW hH hI path
			f := strings.SplitN(rec, " ", 9)
			if len(f) == 9 && len(f[1]) == 2 {
				add(f[8], f[1][:1], f[1][1:])
			}
		case strings.HasPrefix(rec, "2 "):
			// 2 XY sub mH mI mW hH hI Xscore path, then the original path as its own record
			f := strings.SplitN(rec, " ", 10)
			if len(f) == 10 && len(f[1]) == 2 {
				add(f[9], f[1][:1], f[1][1:])
			}
			i++ // skip the original path
		case strings.HasPrefix(rec, "u "):
			// u XY sub m1 m2 m3 mW h1 h2 h3 path
			f := strings.SplitN(rec, " ", 11)
			if len(f) == 11 && len(f[1]) == 2 {
				add(f[10], f[1][:1], f[1][1:])
			}
		case strings.HasPrefix(rec, "? "):
			add(strings.TrimPrefix(rec, "? "), "?", "?")
		}
	}
	if snap.Upstream == "" {
		snap.Ahead, snap.Behind = nil, nil
	}
	if snap.Detached {
		snap.Branch = ""
	}
	return snap
}
