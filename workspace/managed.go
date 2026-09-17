package workspace

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Managed metadata lives beside the provisioner's record and uses its lock.
// It is deliberately independent of the relay's session registry.
func (p *Provisioner) update(name string, change func(*Record) error) error {
	return withPortLock(p.Home, func() error {
		rs, err := loadRecords(p.Home)
		if err != nil {
			return err
		}
		r := rs.Workspaces[name]
		if r == nil {
			return fmt.Errorf("no such workspace %q", name)
		}
		if err := change(r); err != nil {
			return err
		}
		r.UpdatedAt = now()
		return saveRecords(p.Home, rs)
	})
}

func (p *Provisioner) ConfigureManaged(name, specPath, hash string) error {
	if specPath == "" || hash == "" {
		return errors.New("managed spec path and hash required")
	}
	return p.update(name, func(r *Record) error {
		if r.ManagedHash != "" && r.ManagedHash != hash {
			return errors.New("workspace is bound to a different spec; stop and reconcile it before changing source")
		}
		r.ManagedSpec, r.ManagedHash, r.ManagedArmed = specPath, hash, true
		return nil
	})
}

func (p *Provisioner) ArmManaged(name, hash string) error {
	return p.update(name, func(r *Record) error {
		if r.ManagedHash == "" || r.ManagedHash != hash {
			return errors.New("managed spec hash does not match")
		}
		if r.State == StateStopped || r.State == StateDestroying {
			return errors.New("stopped workspaces cannot resume supervision")
		}
		r.ManagedArmed = true
		return nil
	})
}

func (p *Provisioner) DisarmManaged(name, reason string) error {
	return p.update(name, func(r *Record) error {
		r.ManagedArmed = false
		r.State = StateUnhealthy
		r.Reason = reason
		return nil
	})
}

// SuspendManaged is called whenever the owning daemon starts. No uncertain
// service command is automatically repeated across an unclean daemon epoch.
func (p *Provisioner) SuspendManaged() error {
	return withPortLock(p.Home, func() error {
		rs, err := loadRecords(p.Home)
		if err != nil {
			return err
		}
		changed := false
		for _, r := range rs.Workspaces {
			if r.ManagedArmed {
				r.ManagedArmed = false
				r.UpdatedAt = now()
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return saveRecords(p.Home, rs)
	})
}

func (p *Provisioner) Managed() ([]Record, error) {
	rs, err := loadRecords(p.Home)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(rs.Workspaces))
	for _, r := range rs.Workspaces {
		if r.ManagedHash != "" {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (p *Provisioner) SetRestart(name, service string, count int, next time.Time) error {
	if count < 0 || count > 3 {
		return errors.New("restart budget out of range")
	}
	return p.update(name, func(r *Record) error {
		if r.RestartCount == nil {
			r.RestartCount = map[string]int{}
		}
		if r.NextRestart == nil {
			r.NextRestart = map[string]string{}
		}
		r.RestartCount[service] = count
		if next.IsZero() {
			delete(r.NextRestart, service)
		} else {
			r.NextRestart[service] = next.UTC().Format(time.RFC3339Nano)
		}
		return nil
	})
}

func (p *Provisioner) MarkStopped(name string) error {
	return p.update(name, func(r *Record) error {
		r.ManagedArmed = false
		r.State, r.Reason = StateStopped, ""
		if r.StoppedAt == "" {
			r.StoppedAt = now()
		}
		r.LifecycleIntent = ""
		return nil
	})
}

func (p *Provisioner) BeginLifecycle(name, step string, retryUncertain bool) error {
	return p.update(name, func(r *Record) error {
		if r.LifecycleIntent != "" && !retryUncertain {
			return fmt.Errorf("%s may have run; explicit reconciliation required", r.LifecycleIntent)
		}
		r.LifecycleIntent = step
		r.ManagedArmed = false
		return nil
	})
}

func (p *Provisioner) CompleteLifecycle(name, step string) error {
	return p.update(name, func(r *Record) error {
		if r.LifecycleIntent != step {
			return errors.New("lifecycle intent changed")
		}
		r.LifecycleIntent = ""
		return nil
	})
}

func (p *Provisioner) CompleteTeardownCommand(name, step string, index int) error {
	return p.update(name, func(r *Record) error {
		if r.LifecycleIntent != step || r.TeardownIndex != index {
			return errors.New("teardown progress changed")
		}
		r.TeardownIndex++
		r.LifecycleIntent = ""
		return nil
	})
}

func (p *Provisioner) CompleteDestroyHook(name, step string) error {
	return p.update(name, func(r *Record) error {
		if r.LifecycleIntent != step {
			return errors.New("destroy hook intent changed")
		}
		r.DestroyHookDone = true
		r.LifecycleIntent = ""
		return nil
	})
}

// RemoveVerified only removes this provisioner's clean, identity-checked git
// worktree. A failed removal keeps the record for explicit reconciliation.
func (p *Provisioner) RemoveVerified(name string, keepBranch bool) error {
	return withPortLock(p.Home, func() error {
		rs, err := loadRecords(p.Home)
		if err != nil {
			return err
		}
		r := rs.Workspaces[name]
		if r == nil {
			return fmt.Errorf("no such workspace %q", name)
		}
		if r.State != StateDestroying {
			return errors.New("workspace is not in destroying state")
		}
		exists, err := validateExistingWorktree(r.Repo, r.Path, r.Branch)
		if err != nil {
			return err
		}
		if exists {
			cmd := workspaceGit(r.Path, "status", "--porcelain", "--untracked-files=all")
			out, err := cmd.Output()
			if err != nil {
				return err
			}
			if strings.TrimSpace(string(out)) != "" {
				return errors.New("workspace has uncommitted or untracked files; refusing destruction")
			}
			cmd = workspaceGit(r.Repo, "worktree", "remove", "--", r.Path)
			if out, err = cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("worktree removal failed: %w: %s", err, strings.TrimSpace(string(out)))
			}
		}
		if !keepBranch {
			cmd := workspaceGit(r.Repo, "branch", "-d", "--", r.Branch)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("branch deletion refused: %w: %s", err, strings.TrimSpace(string(out)))
			}
		}
		delete(rs.Workspaces, name)
		return saveRecords(p.Home, rs)
	})
}

func (p *Provisioner) BeginDestroy(name string) error {
	return p.update(name, func(r *Record) error {
		if r.State != StateStopped && r.State != StateDestroying {
			return errors.New("stop workspace before destruction")
		}
		r.State = StateDestroying
		r.ManagedArmed = false
		return nil
	})
}
