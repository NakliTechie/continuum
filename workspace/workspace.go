// Package workspace provisions one agent's working copy: the git worktree, the
// ports it may bind, and the variable set every later materialisation stage
// interpolates against. It is relay-side (D2) — the browser cannot touch a
// filesystem on another box, and the relay already can.
package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/NakliTechie/menagerie/relay-go/fleet"
)

// Provisioner owns one relay home (where the record and the port lock live).
type Provisioner struct {
	// Home is the relay's own state directory, typically ~/.menagerie.
	Home string
	// Root is where worktrees are created, typically <Home>/workspaces.
	Root string
}

// New returns a Provisioner rooted at the relay's home directory.
func New(home string) *Provisioner {
	return &Provisioner{Home: home, Root: filepath.Join(home, "workspaces")}
}

// Provision creates (or re-adopts) the workspace called name for spec, against
// the repository at repoRoot. It is safe to call twice: an existing record is
// reused rather than duplicated, which is what makes recovery cheap (D3).
func (p *Provisioner) Provision(spec *fleet.Spec, repoRoot, name string) (*Record, error) {
	if issues := fleet.Validate(spec); len(issues) > 0 {
		return nil, fmt.Errorf("spec is invalid: %s", issues[0].Error())
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("workspace name is required")
	}

	branch := spec.Workspace.BranchPrefix + name
	path := filepath.Join(p.Root, name)

	var rec *Record
	err := withPortLock(p.Home, func() error {
		rs, err := loadRecords(p.Home)
		if err != nil {
			return err
		}
		existing := rs.Workspaces[name]

		// Ports already handed to live workspaces are off the table, so a second
		// workspace never re-allocates the first one's port.
		taken := map[int]bool{}
		for n, w := range rs.Workspaces {
			if n == name {
				continue
			}
			for _, port := range w.Ports {
				taken[port] = true
			}
		}

		ports := map[string]int{}
		for _, decl := range spec.Workspace.Materialise.Ports {
			// D3: re-materialising reuses what was already allocated.
			if existing != nil {
				if had, ok := existing.Ports[decl.Name]; ok {
					ports[decl.Name] = had
					taken[had] = true
					continue
				}
			}
			port, err := freePort(decl.Range[0], decl.Range[1], taken)
			if err != nil {
				return fmt.Errorf("allocating %s: %w", decl.Name, err)
			}
			ports[decl.Name] = port
			taken[port] = true
		}

		rec = &Record{
			Name: name, Repo: repoRoot, Branch: branch, Path: path,
			Ports: ports, Vars: vars(name, branch, repoRoot, ports),
			State: StateProvisioning, CreatedAt: now(), UpdatedAt: now(),
		}
		if existing != nil {
			rec.CreatedAt = existing.CreatedAt
			rec.State = existing.State
		}
		rs.Workspaces[name] = rec
		return saveRecords(p.Home, rs)
	})
	if err != nil {
		return nil, err
	}

	if err := p.ensureWorktree(repoRoot, path, branch); err != nil {
		return nil, err
	}
	return rec, nil
}

// vars is the complete interpolation set: the allocated ports plus the three
// builtins. Nothing inherits from the relay's own environment — a workspace that
// silently picked up the relay's variables would behave differently on every box.
func vars(name, branch, repoRoot string, ports map[string]int) map[string]string {
	out := map[string]string{
		"WORKSPACE": name,
		"BRANCH":    branch,
		"REPO_ROOT": repoRoot,
	}
	for k, v := range ports {
		out[k] = fmt.Sprint(v)
	}
	return out
}

// ensureWorktree creates the worktree if it is missing and leaves it alone if it
// is already there, so Provision converges instead of failing on a second call.
func (p *Provisioner) ensureWorktree(repoRoot, path, branch string) error {
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	args := []string{"worktree", "add", path}
	if branchExists(repoRoot, branch) {
		args = append(args, branch)
	} else {
		args = append(args, "-b", branch)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func branchExists(repoRoot, branch string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = repoRoot
	return cmd.Run() == nil
}

// Load returns the recorded workspace, or nil when it was never provisioned.
func (p *Provisioner) Load(name string) (*Record, error) {
	rs, err := loadRecords(p.Home)
	if err != nil {
		return nil, err
	}
	return rs.Workspaces[name], nil
}

// SetState records a workspace's lifecycle state. A failed health probe leaves a
// workspace `unhealthy`, never `ready`.
func (p *Provisioner) SetState(name, state string) error {
	return withPortLock(p.Home, func() error {
		rs, err := loadRecords(p.Home)
		if err != nil {
			return err
		}
		w, ok := rs.Workspaces[name]
		if !ok {
			return fmt.Errorf("no such workspace %q", name)
		}
		w.State, w.UpdatedAt = state, now()
		return saveRecords(p.Home, rs)
	})
}
