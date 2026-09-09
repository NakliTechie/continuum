package materialise

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NakliTechie/menagerie/relay-go/fleet"
	"github.com/NakliTechie/menagerie/relay-go/workspace"
)

// Step is one recorded action. A run returns its steps in execution order, which
// is what the dry run prints and what the tests assert against.
type Step struct {
	Stage   string `json:"stage"`
	Action  string `json:"action"`
	Detail  string `json:"detail"`
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Result is one materialisation's outcome.
type Result struct {
	Workspace *workspace.Record `json:"workspace"`
	Steps     []Step            `json:"steps"`
	State     string            `json:"state"`
	Failed    []string          `json:"failed_probes,omitempty"`
}

// Engine materialises workspaces for one relay home.
type Engine struct {
	Prov *workspace.Provisioner
	FS   FileSystem
	Exec Executor
	Prob Prober
	// DryRun records what would happen without starting services or running the
	// escape hatch. Commands and probes are recorded, never executed.
	DryRun bool
}

// New returns an Engine backed by the real filesystem, shell and network.
func New(prov *workspace.Provisioner) *Engine {
	return &Engine{Prov: prov, FS: OSFileSystem{}, Exec: ShellExecutor{}, Prob: HTTPProber{}}
}

// Run executes the materialise block in its fixed order. It is idempotent (D3):
// allocated ports are reused, commands whose cache_key is unchanged are skipped,
// and services already up are left alone.
func (e *Engine) Run(spec *fleet.Spec, repoRoot, name string) (*Result, error) {
	if issues := fleet.Validate(spec); len(issues) > 0 {
		return nil, fmt.Errorf("spec is invalid: %s", issues[0].Error())
	}
	res := &Result{}

	// --- 1. ports (via the provisioner, which owns allocation and the record) ---
	rec, err := e.Prov.Provision(spec, repoRoot, name)
	if err != nil {
		return nil, err
	}
	res.Workspace = rec
	for _, p := range spec.Workspace.Materialise.Ports {
		res.Steps = append(res.Steps, Step{Stage: "ports", Action: "allocate",
			Detail: fmt.Sprintf("%s=%d", p.Name, rec.Ports[p.Name])})
	}
	if !e.DryRun {
		_ = e.Prov.SetState(name, workspace.StateMaterialising)
	}
	m := spec.Workspace.Materialise

	// --- 2. files ---
	for i, f := range m.Files {
		step, err := e.materialiseFile(f, rec, repoRoot)
		if err != nil {
			return res, fmt.Errorf("files[%d]: %w", i, err)
		}
		res.Steps = append(res.Steps, step)
	}

	// --- 3. commands (cache_key skipping is per repo, not per workspace) ---
	for i, c := range m.Commands {
		step, err := e.runCommand(c, rec, repoRoot)
		if err != nil {
			return res, fmt.Errorf("commands[%d]: %w", i, err)
		}
		res.Steps = append(res.Steps, step)
	}

	// --- 4. services ---
	for i, sv := range m.Services {
		step, err := e.startService(sv, rec, repoRoot)
		if err != nil {
			return res, fmt.Errorf("services[%d]: %w", i, err)
		}
		res.Steps = append(res.Steps, step)
	}

	// --- 5. health: probes gate completion; a failure is unhealthy, not ready ---
	for _, p := range m.Health {
		step, ok := e.probe(p, rec)
		res.Steps = append(res.Steps, step)
		if !ok {
			res.Failed = append(res.Failed, step.Detail)
		}
	}

	// --- 6. escape, last, with every variable exported ---
	if m.Escape != "" {
		step := Step{Stage: "escape", Action: "run", Detail: m.Escape}
		if e.DryRun {
			step.Skipped, step.Reason = true, "dry run"
		} else if _, err := e.Exec.Run(rec.Path, envSlice(rec.Vars), m.Escape, 10*time.Minute); err != nil {
			return res, fmt.Errorf("escape: %w", err)
		}
		res.Steps = append(res.Steps, step)
	}

	res.State = workspace.StateReady
	if len(res.Failed) > 0 {
		res.State = workspace.StateUnhealthy
	}
	if !e.DryRun {
		_ = e.Prov.SetState(name, res.State)
	}
	return res, nil
}

// materialiseFile copies or renders one file. Both forms refuse to write outside
// the workspace root — the validator rejects the obvious spellings, and this is
// the enforcement that does not depend on having been validated.
func (e *Engine) materialiseFile(f fleet.File, rec *workspace.Record, repoRoot string) (Step, error) {
	dest := filepath.Join(rec.Path, f.To)
	if !within(rec.Path, dest) {
		return Step{}, fmt.Errorf("destination %q escapes the workspace root", f.To)
	}
	src := f.From
	if src == "" {
		src = f.Template
	}
	if !filepath.IsAbs(src) {
		src = filepath.Join(repoRoot, src)
	}
	b, err := e.FS.ReadFile(src)
	if err != nil {
		return Step{}, fmt.Errorf("reading %s: %w", src, err)
	}
	action := "copy"
	if f.Template != "" {
		action = "render"
		b = []byte(interpolate(string(b), rec.Vars))
	}
	if err := e.FS.WriteFile(dest, b, 0o600); err != nil {
		return Step{}, err
	}
	return Step{Stage: "files", Action: action, Detail: f.To}, nil
}

// runCommand honours cache_key: the command is skipped when the named file's
// hash is unchanged since the last successful materialise in this repo. The
// cache is keyed by repo and command, never by workspace — that is what turns a
// three-minute install into a no-op on workspaces 2 through N.
func (e *Engine) runCommand(c fleet.Command, rec *workspace.Record, repoRoot string) (Step, error) {
	line := interpolate(c.Run, rec.Vars)
	step := Step{Stage: "commands", Action: "run", Detail: line}
	if c.CacheKey != "" {
		hash, err := e.hashFile(filepath.Join(repoRoot, c.CacheKey))
		if err == nil {
			hit, err := e.cacheHit(repoRoot, c.Run, hash)
			if err == nil && hit {
				step.Skipped, step.Reason = true, "cache_key "+c.CacheKey+" unchanged"
				return step, nil
			}
			defer func() { _ = e.cachePut(repoRoot, c.Run, hash) }()
		}
	}
	if e.DryRun {
		step.Skipped, step.Reason = true, "dry run"
		return step, nil
	}
	if out, err := e.Exec.Run(rec.Path, envSlice(rec.Vars), line, 30*time.Minute); err != nil {
		return step, fmt.Errorf("%s: %w: %s", line, err, strings.TrimSpace(string(out)))
	}
	return step, nil
}

// startService starts a service once per repo — unless port_var is set, which
// makes it once per workspace, because a per-workspace port implies a
// per-workspace instance. A service already up is left alone (D3).
func (e *Engine) startService(sv fleet.Service, rec *workspace.Record, repoRoot string) (Step, error) {
	scope := repoRoot + "|" + sv.Name
	if sv.PortVar != "" {
		scope += "|" + rec.Name
	}
	line := interpolate(sv.Run, rec.Vars)
	step := Step{Stage: "services", Action: "start", Detail: sv.Name}
	up, err := e.serviceUp(scope)
	if err == nil && up {
		step.Skipped, step.Reason = true, "already running"
		return step, nil
	}
	if e.DryRun {
		step.Skipped, step.Reason = true, "dry run"
		return step, nil
	}
	if out, err := e.Exec.Run(rec.Path, envSlice(rec.Vars), line, 10*time.Minute); err != nil {
		return step, fmt.Errorf("%s: %w: %s", sv.Name, err, strings.TrimSpace(string(out)))
	}
	_ = e.serviceMark(scope)
	return step, nil
}

// probe gates completion. Returns the step and whether it passed.
func (e *Engine) probe(p fleet.Probe, rec *workspace.Record) (Step, bool) {
	timeout := time.Duration(p.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	target := interpolate(p.URL+p.Run, rec.Vars)
	step := Step{Stage: "health", Action: p.Probe, Detail: target}
	if e.DryRun {
		step.Skipped, step.Reason = true, "dry run"
		return step, true
	}
	var err error
	if p.Probe == "http" {
		err = e.Prob.HTTP(interpolate(p.URL, rec.Vars), timeout)
	} else {
		_, err = e.Exec.Run(rec.Path, envSlice(rec.Vars), interpolate(p.Run, rec.Vars), timeout)
	}
	if err != nil {
		step.Reason = err.Error()
		return step, false
	}
	return step, true
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (e *Engine) hashFile(path string) (string, error) {
	b, err := e.FS.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// --- the two small on-disk caches -------------------------------------------

type cacheSet struct {
	Commands map[string]string `json:"commands"`
	Services map[string]bool   `json:"services"`
}

func (e *Engine) cachePath() string { return filepath.Join(e.Prov.Home, "materialise-cache.json") }

func (e *Engine) loadCache() *cacheSet {
	cs := &cacheSet{Commands: map[string]string{}, Services: map[string]bool{}}
	b, err := os.ReadFile(e.cachePath())
	if err != nil || len(b) == 0 {
		return cs
	}
	_ = json.Unmarshal(b, cs)
	if cs.Commands == nil {
		cs.Commands = map[string]string{}
	}
	if cs.Services == nil {
		cs.Services = map[string]bool{}
	}
	return cs
}

func (e *Engine) saveCache(cs *cacheSet) error {
	b, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(e.Prov.Home, 0o700); err != nil {
		return err
	}
	return os.WriteFile(e.cachePath(), append(b, '\n'), 0o600)
}

func (e *Engine) cacheHit(repoRoot, run, hash string) (bool, error) {
	return e.loadCache().Commands[repoRoot+"|"+run] == hash, nil
}

func (e *Engine) cachePut(repoRoot, run, hash string) error {
	cs := e.loadCache()
	cs.Commands[repoRoot+"|"+run] = hash
	return e.saveCache(cs)
}

func (e *Engine) serviceUp(scope string) (bool, error) { return e.loadCache().Services[scope], nil }

func (e *Engine) serviceMark(scope string) error {
	cs := e.loadCache()
	cs.Services[scope] = true
	return e.saveCache(cs)
}
