package materialise

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NakliTechie/continuum/fleet"
	"github.com/NakliTechie/continuum/workspace"
)

const maxManagedSpec = 1 << 20
const restartBudget = 3

// ManagedSpec reads one bounded exact source. The hash binds shell authority
// to the bytes reviewed by the operator, not merely a path or parsed shape.
func ManagedSpec(path string) (*fleet.Spec, string, string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, "", "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxManagedSpec {
		return nil, "", "", errors.New("spec must be a regular file no larger than 1 MiB")
	}
	raw, err := os.ReadFile(absolute)
	if err != nil {
		return nil, "", "", err
	}
	hash := sha256.Sum256(raw)
	spec, issues := fleet.ValidateBytes(raw)
	if len(issues) != 0 {
		return nil, "", "", fmt.Errorf("invalid fleet spec: %s", issues[0].Error())
	}
	return spec, absolute, hex.EncodeToString(hash[:]), nil
}

func managedRepo(spec *fleet.Spec, path string) (string, error) {
	repo, err := filepath.Abs(filepath.Join(filepath.Dir(path), spec.Repo))
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(repo)
}

func supervisedServices(spec *fleet.Spec) []fleet.Service {
	var out []fleet.Service
	for _, service := range spec.Workspace.Materialise.Services {
		if service.Supervise {
			out = append(out, service)
		}
	}
	return out
}

func checkManagedStopHook(spec *fleet.Spec) error {
	if len(supervisedServices(spec)) != 0 && (spec.Workspace.Materialise.Hooks == nil || spec.Workspace.Materialise.Hooks.OnStop == "") {
		return errors.New("managed supervised services require hooks.on_stop before shell can run")
	}
	return nil
}

// RunManaged is the only entrypoint that executes a newly managed spec. It
// atomically provisions and binds exact-hash trust, then runs the engine.
func (e *Engine) RunManaged(specPath, name, trust string) (*Result, error) {
	spec, path, hash, err := ManagedSpec(specPath)
	if err != nil {
		return nil, err
	}
	if trust != hash {
		return nil, fmt.Errorf("spec trust required: review %s and pass --trust %s", path, hash)
	}
	if err := checkManagedStopHook(spec); err != nil {
		return nil, err
	}
	repo, err := managedRepo(spec, path)
	if err != nil {
		return nil, err
	}
	unlock, err := e.lockCacheScope("managed:"+name, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer unlock()
	rec, err := e.Prov.ProvisionManaged(spec, repo, name, path, hash)
	if err != nil {
		return nil, err
	}
	managed := *e
	if _, ok := managed.Prob.(HTTPProber); ok {
		managed.Prob = StrictHTTPProber{}
	}
	return managed.run(spec, repo, name, rec)
}

func (e *Engine) PlanManaged(specPath, name string) (*fleet.Spec, *Result, string, error) {
	spec, path, hash, err := ManagedSpec(specPath)
	if err != nil {
		return nil, nil, "", err
	}
	if err := checkManagedStopHook(spec); err != nil {
		return spec, nil, hash, err
	}
	repo, err := managedRepo(spec, path)
	if err != nil {
		return nil, nil, "", err
	}
	plan := *e
	plan.DryRun = true
	res, err := plan.Run(spec, repo, name)
	return spec, res, hash, err
}

// TickManaged makes one bounded pass over every armed workspace. The daemon
// calls it on a fixed cadence; the per-workspace lock serializes shell effects
// against stop/destroy and other managers.
func (e *Engine) TickManaged() error {
	records, err := e.Prov.Managed()
	if err != nil {
		return err
	}
	for _, rec := range records {
		if !rec.ManagedArmed || rec.State == workspace.StateStopped || rec.State == workspace.StateDestroying {
			continue
		}
		if err := e.tickOne(rec.Name); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) tickOne(name string) error {
	managed := *e
	if _, ok := managed.Prob.(HTTPProber); ok {
		managed.Prob = StrictHTTPProber{}
	}
	e = &managed
	unlock, err := e.lockCacheScope("managed:"+name, 2*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	rec, err := e.Prov.Load(name)
	if err != nil || rec == nil {
		return err
	}
	if !rec.ManagedArmed || rec.State == workspace.StateStopped || rec.State == workspace.StateDestroying {
		return nil
	}
	spec, path, hash, err := ManagedSpec(rec.ManagedSpec)
	if err != nil || path != rec.ManagedSpec || hash != rec.ManagedHash {
		return e.Prov.DisarmManaged(name, "managed spec missing, invalid or changed; review and resume explicitly")
	}
	repo, err := managedRepo(spec, path)
	if err != nil || repo != rec.Repo {
		return e.Prov.DisarmManaged(name, "managed repository identity changed")
	}
	if rec.State != workspace.StateReady && rec.State != workspace.StateUnhealthy {
		return nil
	}
	if rec.State == workspace.StateUnhealthy && rec.Reason != "health: probe failed" && !strings.HasPrefix(rec.Reason, "supervise:") {
		return nil
	}
	for _, sv := range spec.Workspace.Materialise.Services {
		if !sv.Supervise {
			continue
		}
		if _, up := e.superviseService(sv, rec, 0); up {
			continue
		}
		count := rec.RestartCount[sv.Name]
		if count >= restartBudget {
			return e.Prov.SetStateReason(name, workspace.StateUnhealthy, "supervise: restart budget exhausted: "+sv.Name)
		}
		if next, parseErr := time.Parse(time.RFC3339Nano, rec.NextRestart[sv.Name]); parseErr == nil && time.Now().Before(next) {
			return e.Prov.SetStateReason(name, workspace.StateUnhealthy, "supervise: restart backoff: "+sv.Name)
		}
		backoff := time.Duration(5<<count) * time.Second
		if err := e.Prov.SetRestart(name, sv.Name, count+1, time.Now().Add(backoff)); err != nil {
			return err
		}
		if _, err := e.Exec.Run(rec.Path, envSlice(rec.Vars), interpolate(sv.Run, rec.Vars), 60*time.Second); err != nil {
			return e.Prov.SetStateReason(name, workspace.StateUnhealthy, "supervise: restart failed: "+sv.Name)
		}
		if _, up := e.superviseService(sv, rec, e.settle()); !up {
			return e.Prov.SetStateReason(name, workspace.StateUnhealthy, "supervise: service not answering: "+sv.Name)
		}
	}
	for _, p := range spec.Workspace.Materialise.Health {
		// Cap recurrent health checks independently of initial materialisation.
		p.TimeoutS = 5
		if _, ok := e.probe(p, rec); !ok {
			return e.Prov.SetStateReason(name, workspace.StateUnhealthy, "health: probe failed")
		}
	}
	for _, sv := range spec.Workspace.Materialise.Services {
		if sv.Supervise && rec.RestartCount[sv.Name] != 0 {
			if err := e.Prov.SetRestart(name, sv.Name, 0, time.Time{}); err != nil {
				return err
			}
		}
	}
	return e.Prov.SetStateReason(name, workspace.StateReady, "")
}

func (e *Engine) loadTrusted(name string) (*workspace.Record, *fleet.Spec, error) {
	rec, err := e.Prov.Load(name)
	if err != nil || rec == nil {
		return nil, nil, fmt.Errorf("workspace %q is unavailable", name)
	}
	if rec.ManagedHash == "" {
		return nil, nil, errors.New("workspace is not managed")
	}
	spec, path, hash, err := ManagedSpec(rec.ManagedSpec)
	if err != nil || path != rec.ManagedSpec || hash != rec.ManagedHash {
		return nil, nil, errors.New("managed spec changed; refusing repository-authored shell")
	}
	repo, err := managedRepo(spec, path)
	if err != nil || repo != rec.Repo {
		return nil, nil, errors.New("managed repository identity changed")
	}
	return rec, spec, nil
}

// StopManaged never guesses whether a hook ran after a crash. An unresolved
// intent requires an explicit retry-uncertain operator decision.
func (e *Engine) StopManaged(name string, retryUncertain bool) error {
	unlock, err := e.lockCacheScope("managed:"+name, 5*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	rec, spec, err := e.loadTrusted(name)
	if err != nil {
		return err
	}
	if rec.State == workspace.StateStopped || rec.State == workspace.StateDestroying {
		return nil
	}
	services := supervisedServices(spec)
	if len(services) != 0 && (spec.Workspace.Materialise.Hooks == nil || spec.Workspace.Materialise.Hooks.OnStop == "") {
		return errors.New("cannot stop supervised services without hooks.on_stop")
	}
	if h := spec.Workspace.Materialise.Hooks; h != nil && h.OnStop != "" {
		const step = "hooks.on_stop"
		if err := e.Prov.BeginLifecycle(name, step, retryUncertain); err != nil {
			return err
		}
		if _, err := e.Exec.Run(rec.Path, envSlice(rec.Vars), interpolate(h.OnStop, rec.Vars), 60*time.Second); err != nil {
			return fmt.Errorf("on_stop failed; effect uncertain: %w", err)
		}
	}
	for _, service := range services {
		if _, up := e.superviseService(service, rec, 0); up {
			return fmt.Errorf("on_stop returned but supervised service %q is still answering; workspace remains armed for reconciliation", service.Name)
		}
	}
	return e.Prov.MarkStopped(name)
}

// DestroyManaged is explicitly requested, clean-worktree-only teardown. Each
// completed command is checkpointed; an uncertain command is never implicit.
func (e *Engine) DestroyManaged(name string, retryUncertain bool) error {
	if err := e.StopManaged(name, retryUncertain); err != nil {
		return err
	}
	unlock, err := e.lockCacheScope("managed:"+name, 5*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	rec, spec, err := e.loadTrusted(name)
	if err != nil {
		return err
	}
	if err := e.Prov.BeginDestroy(name); err != nil {
		return err
	}
	if td := spec.Workspace.Teardown; td != nil {
		for i := rec.TeardownIndex; i < len(td.Commands); i++ {
			step := fmt.Sprintf("teardown.commands[%d]", i)
			if err := e.Prov.BeginLifecycle(name, step, retryUncertain); err != nil {
				return err
			}
			if _, err := e.Exec.Run(rec.Path, envSlice(rec.Vars), interpolate(td.Commands[i], rec.Vars), 60*time.Second); err != nil {
				return fmt.Errorf("teardown command failed; effect uncertain: %w", err)
			}
			if err := e.Prov.CompleteTeardownCommand(name, step, i); err != nil {
				return err
			}
		}
	}
	if h := spec.Workspace.Materialise.Hooks; h != nil && h.OnDestroy != "" && !rec.DestroyHookDone {
		const step = "hooks.on_destroy"
		if err := e.Prov.BeginLifecycle(name, step, retryUncertain); err != nil {
			return err
		}
		if _, err := e.Exec.Run(rec.Path, envSlice(rec.Vars), interpolate(h.OnDestroy, rec.Vars), 60*time.Second); err != nil {
			return fmt.Errorf("on_destroy failed; effect uncertain: %w", err)
		}
		if err := e.Prov.CompleteDestroyHook(name, step); err != nil {
			return err
		}
	}
	keep := spec.Workspace.Teardown == nil || spec.Workspace.Teardown.KeepBranch
	return e.Prov.RemoveVerified(name, keep)
}
