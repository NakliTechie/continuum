package materialise

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/NakliTechie/continuum/fleet"
	"github.com/NakliTechie/continuum/workspace"
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
	// Dial reports whether something is listening; supervision uses it.
	Dial Dialer
	// Settle is how long a freshly started service gets to bind its port before
	// supervision calls it dead. Zero means the package default.
	Settle time.Duration
	// DryRun records what would happen without starting services or running the
	// escape hatch. Commands and probes are recorded, never executed.
	DryRun bool
}

// New returns an Engine backed by the real filesystem, shell and network.
func New(prov *workspace.Provisioner) *Engine {
	return &Engine{Prov: prov, FS: OSFileSystem{}, Exec: ShellExecutor{}, Prob: HTTPProber{}, Dial: TCPDial}
}

// Run executes the materialise block in its fixed order. It is idempotent (D3):
// allocated ports are reused, commands whose cache_key is unchanged are skipped,
// and services already up are left alone.
func (e *Engine) Run(spec *fleet.Spec, repoRoot, name string) (*Result, error) {
	if issues := fleet.Validate(spec); len(issues) > 0 {
		return nil, fmt.Errorf("spec is invalid: %s", issues[0].Error())
	}
	res := &Result{}
	// A stage that fails must leave the record honestly unhealthy. An early return
	// used to strand it at `materialising` — neither ready nor unhealthy, and
	// nothing would move it again.
	// `where` is a stage locator only — never the error text. A failed command's
	// combined output can carry anything the command printed, including a
	// credential, and the record is persisted to the relay's disk; the handoff
	// forbids secrets in the workspace record. The full error still goes to the
	// caller, which decides what to log.
	fail := func(where string, err error) (*Result, error) {
		// The record gets a locator only; the detail goes to the relay's log, which
		// is where the sanitised reason points the operator. Without this line that
		// pointer led nowhere.
		// The log line carries the locator and the error's class, never the
		// command's output: that output can hold whatever the command printed,
		// and the relay log is not a private file.
		log.Printf("materialise %s: %s failed: %s", name, where, errClass(err))
		if !e.DryRun {
			_ = e.Prov.SetStateReason(name, workspace.StateUnhealthy, where+" failed; see the relay log")
			if fresh, lerr := e.Prov.Load(name); lerr == nil && fresh != nil {
				res.Workspace = fresh // the failure path returned a pre-run snapshot too
			}
		}
		return res, fmt.Errorf("%s: %w", where, err)
	}

	// --- 1. ports (via the provisioner, which owns allocation and the record) ---
	// A dry run never allocates, never creates a worktree and never writes a
	// record: it computes the plan against a fake allocator, so the output is
	// deterministic and the box is untouched.
	var rec *workspace.Record
	var err error
	if e.DryRun {
		rec = dryRecord(spec, repoRoot, name, e.Prov.Root)
	} else if rec, err = e.Prov.Provision(spec, repoRoot, name); err != nil {
		return nil, err
	}
	// Whether on_start has ever run for this workspace. Gating on the record's
	// prior STATE was wrong: Supervise() can promote a workspace to ready without
	// running the hook, after which every later pass read "already started" and
	// skipped it forever. The stamp is the honest question — did the hook run?
	alreadyStarted := rec.StartedAt != ""
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
			return fail(fmt.Sprintf("files[%d]", i), err)
		}
		res.Steps = append(res.Steps, step)
	}

	// --- 3. commands (cache_key skipping is per workspace and expanded command) ---
	for i, c := range m.Commands {
		step, err := e.runCommand(c, rec, repoRoot)
		if err != nil {
			return fail(fmt.Sprintf("commands[%d]", i), err)
		}
		res.Steps = append(res.Steps, step)
	}

	// --- 4. services ---
	for i, sv := range m.Services {
		step, err := e.startService(sv, rec, repoRoot)
		if err != nil {
			return fail(fmt.Sprintf("services[%d]", i), err)
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
		step := Step{Stage: "escape", Action: "run", Detail: interpolate(m.Escape, rec.Vars)}
		if e.DryRun {
			step.Skipped, step.Reason = true, "dry run"
		} else if _, err := e.Exec.Run(rec.Path, envSlice(rec.Vars), interpolate(m.Escape, rec.Vars), 10*time.Minute); err != nil {
			return fail("escape", err)
		}
		res.Steps = append(res.Steps, step)
	}

	// --- supervision: a probe is a moment, not a guarantee. Re-check every
	// supervised service now, so a service that died between starting and here
	// is caught before the workspace is called ready.
	for _, sv := range m.Services {
		if !sv.Supervise {
			continue
		}
		step, ok := e.superviseService(sv, rec, e.settle())
		res.Steps = append(res.Steps, step)
		if !ok {
			// Same wording as Supervise writes, so a later supervision pass can
			// recognise this as its own verdict and clear it when the service
			// answers again.
			res.Failed = append(res.Failed, supervisionReason+sv.Name)
		}
	}

	// --- on_start: a lifecycle hook, not a seventh stage. It runs once the whole
	// declared sequence has succeeded, and only then.
	if h := m.Hooks; h != nil && h.OnStart != "" && len(res.Failed) == 0 {
		step := Step{Stage: "hooks", Action: "on_start", Detail: interpolate(h.OnStart, rec.Vars)}
		switch {
		case e.DryRun:
			step.Skipped, step.Reason = true, "dry run"
		case alreadyStarted:
			// The hook already ran for this workspace; this pass only converged.
			// Firing again would make a hook that notifies, seeds or registers do it
			// once per materialise call.
			step.Skipped, step.Reason = true, "workspace was already started"
		default:
			if _, err := e.Exec.Run(rec.Path, envSlice(rec.Vars), interpolate(h.OnStart, rec.Vars), 5*time.Minute); err != nil {
				// Do not return here: an early return would leave the record stranded
				// at `materialising`, which is neither ready nor honestly unhealthy.
				// A failed start hook is a failed materialisation, reported the same
				// way a failed probe is.
				step.Reason = err.Error()
				res.Failed = append(res.Failed, "hooks.on_start: "+err.Error())
			} else if !e.DryRun {
				// Stamped only on success, so a hook that failed is retried next pass.
				_ = e.Prov.MarkStarted(name)
			}
		}
		res.Steps = append(res.Steps, step)
	}

	res.State = workspace.StateReady
	reason := ""
	if len(res.Failed) > 0 {
		res.State = workspace.StateUnhealthy
		reason = res.Failed[0]
	}
	if !e.DryRun {
		_ = e.Prov.SetStateReason(name, res.State, reason)
		// Re-read: `rec` is a pre-run snapshot, so without this the result reports
		// the state the workspace was in BEFORE the run — a just-materialised
		// workspace would go over the wire as `provisioning`.
		if fresh, err := e.Prov.Load(name); err == nil && fresh != nil {
			res.Workspace = fresh
		}
	}
	return res, nil
}

// materialiseFile copies or renders one file. Both forms refuse to write outside
// the workspace root — the validator rejects the obvious spellings, and this is
// the enforcement that does not depend on having been validated.
func (e *Engine) materialiseFile(f fleet.File, rec *workspace.Record, repoRoot string) (Step, error) {
	to := interpolate(f.To, rec.Vars)
	dest := filepath.Join(rec.Path, to)
	if !within(rec.Path, dest) {
		return Step{}, fmt.Errorf("destination %q escapes the workspace root", to)
	}
	src := f.From
	if src == "" {
		src = f.Template
	}
	if !filepath.IsAbs(src) {
		src = filepath.Join(repoRoot, src)
	}
	action := "copy"
	if f.Template != "" {
		action = "render"
	}
	// The plan names the source as well as the destination: a reviewer of the
	// dry run must see what a spec reads, not only where it writes.
	detail := f.From
	if detail == "" {
		detail = f.Template
	}
	detail += " -> " + to
	// A dry run reports what it would do without reading the repo or writing the
	// workspace: the plan is about the graph, not about the box's current files.
	if e.DryRun {
		return Step{Stage: "files", Action: action, Detail: detail, Skipped: true, Reason: "dry run"}, nil
	}
	if err := allowedSource(e.FS, src, repoRoot); err != nil {
		return Step{}, err
	}
	b, err := e.FS.ReadFile(src)
	if err != nil {
		return Step{}, fmt.Errorf("reading %s: %w", src, err)
	}
	if f.Template != "" {
		b = []byte(interpolate(string(b), rec.Vars))
	}
	if err := e.FS.WriteFileWithin(rec.Path, to, b, 0o600); err != nil {
		return Step{}, err
	}
	return Step{Stage: "files", Action: action, Detail: detail}, nil
}

// allowedSource bounds what a committed spec may read into a workspace: files
// inside the checkout, or files directly beside it (the `../.env.local`
// convention for per-checkout secrets). Symlinks are resolved first, so a
// committed link cannot point the copy at the operator's home directory, and
// nothing nested under a sibling directory (another project's secrets) is
// reachable.
func allowedSource(fsys FileSystem, src, repoRoot string) error {
	root, err := fsys.Resolve(repoRoot)
	if err != nil {
		return fmt.Errorf("resolving repository root: %w", err)
	}
	resolved, err := fsys.Resolve(src)
	if err != nil {
		return fmt.Errorf("reading %s: %w", src, err)
	}
	if within(root, resolved) || filepath.Dir(resolved) == filepath.Dir(root) {
		return nil
	}
	return fmt.Errorf("source %q resolves to %s, outside the checkout and its parent directory", src, resolved)
}

// runCommand caches successful execution within the same workspace and expanded
// command. Workspace-local artifacts cannot be inferred from another worktree.
func (e *Engine) runCommand(c fleet.Command, rec *workspace.Record, repoRoot string) (Step, error) {
	line := interpolate(c.Run, rec.Vars)
	step := Step{Stage: "commands", Action: "run", Detail: line}
	if e.DryRun {
		step.Skipped = true
		step.Reason = "dry run"
		return step, nil
	}
	hash := ""
	if c.CacheKey != "" {
		// The key is read from the tree the command runs in; the main checkout
		// only stands in when the worktree lacks the file.
		var err error
		if hash, err = e.hashFile(filepath.Join(rec.Path, c.CacheKey)); errors.Is(err, fs.ErrNotExist) {
			hash, _ = e.hashFile(filepath.Join(repoRoot, c.CacheKey))
		}
		if hash != "" {
			hit, err := e.cacheHit(rec.Path, line, hash)
			if err != nil {
				return step, err
			}
			if hit {
				step.Skipped = true
				step.Reason = "cache_key " + c.CacheKey + " unchanged in this workspace"
				return step, nil
			}
		}
	}
	if out, err := e.Exec.Run(rec.Path, envSlice(rec.Vars), line, 30*time.Minute); err != nil {
		return step, fmt.Errorf("%s: %w: %s", line, err, strings.TrimSpace(string(out)))
	}
	if hash != "" {
		if err := e.cachePut(rec.Path, line, hash); err != nil {
			return step, fmt.Errorf("recording command cache: %w", err)
		}
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
	if e.DryRun {
		step.Skipped, step.Reason = true, "dry run"
		return step, nil
	}
	unlock, err := e.lockCacheScope("service:"+scope, 11*time.Minute)
	if err != nil {
		return step, err
	}
	defer unlock()
	up, err := e.serviceUp(scope)
	if err != nil {
		return step, err // an unreadable cache must not start a service twice
	}
	if up {
		// The cache remembers that we started it; it cannot know it is still
		// alive. For a supervised service, check before believing the cache —
		// otherwise a service that died stays "already running" forever and
		// re-materialising can never recover the workspace, which is exactly the
		// cheap recovery D3 promises.
		if !sv.Supervise {
			step.Skipped, step.Reason = true, "already running"
			return step, nil
		}
		// A concurrent starter may have returned before its port binds.
		// Give the declared settle window before admitting a restart.
		if _, alive := e.superviseService(sv, rec, e.settle()); alive {
			step.Skipped, step.Reason = true, "already running"
			return step, nil
		}
		step.Reason = "restarting: supervised service was not answering"
	}
	if out, err := e.Exec.Run(rec.Path, envSlice(rec.Vars), line, 10*time.Minute); err != nil {
		return step, fmt.Errorf("%s: %w: %s", sv.Name, err, strings.TrimSpace(string(out)))
	}
	if err := e.serviceMark(scope); err != nil {
		return step, fmt.Errorf("service started but recording its cache failed; inspect before retry: %w", err)
	}
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

// errClass names an error without reproducing a wrapped command's output.
func errClass(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.String()
	}
	if strings.Contains(err.Error(), "timed out") {
		return "timed out"
	}
	return "error (details in the command output returned to the caller)"
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

func (e *Engine) loadCache() (*cacheSet, error) {
	cs := &cacheSet{Commands: map[string]string{}, Services: map[string]bool{}}
	b, err := os.ReadFile(e.cachePath())
	if os.IsNotExist(err) {
		return cs, nil
	}
	if err != nil {
		return nil, err
	}
	// A cache that cannot be read is not an empty cache: treating it as one
	// would start every once-per-repo service a second time. The engine only
	// ever writes it whole (temp + rename), so a zero-byte file is damage too.
	if err := json.Unmarshal(b, cs); err != nil {
		return nil, fmt.Errorf("materialise cache %s is unreadable; inspect or delete it: %w", e.cachePath(), err)
	}
	if cs.Commands == nil {
		cs.Commands = map[string]string{}
	}
	if cs.Services == nil {
		cs.Services = map[string]bool{}
	}
	return cs, nil
}

func (e *Engine) saveCache(cs *cacheSet) error {
	b, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(e.Prov.Home, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(e.Prov.Home, ".materialise-cache-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), e.cachePath())
}

func (e *Engine) cacheHit(repoRoot, run, hash string) (bool, error) {
	cs, err := e.loadCache()
	if err != nil {
		return false, err
	}
	return cs.Commands[repoRoot+"|"+run] == hash, nil
}

func (e *Engine) cachePut(repoRoot, run, hash string) error {
	unlock, err := e.lockCacheScope("cache-write", 5*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	cs, err := e.loadCache()
	if err != nil {
		return err
	}
	cs.Commands[repoRoot+"|"+run] = hash
	return e.saveCache(cs)
}

func (e *Engine) serviceUp(scope string) (bool, error) {
	cs, err := e.loadCache()
	if err != nil {
		return false, err
	}
	return cs.Services[scope], nil
}

func (e *Engine) serviceMark(scope string) error {
	unlock, err := e.lockCacheScope("cache-write", 5*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	cs, err := e.loadCache()
	if err != nil {
		return err
	}
	cs.Services[scope] = true
	return e.saveCache(cs)
}

// dryRecord is the fake port allocator: every declared port resolves to the low
// end of its range. Deterministic on purpose — a plan you cannot diff is not a
// plan, and the golden file is what proves the graph did not shift.
func dryRecord(spec *fleet.Spec, repoRoot, name, root string) *workspace.Record {
	branch := spec.Workspace.BranchPrefix + name
	ports := map[string]int{}
	for _, p := range spec.Workspace.Materialise.Ports {
		ports[p.Name] = p.Range[0]
	}
	vars := map[string]string{"WORKSPACE": name, "BRANCH": branch, "REPO_ROOT": repoRoot}
	for k, v := range ports {
		vars[k] = fmt.Sprint(v)
	}
	return &workspace.Record{
		Name: name, Repo: repoRoot, Branch: branch, Path: filepath.Join(root, name),
		Ports: ports, Vars: vars, State: workspace.StateProvisioning,
	}
}

// RenderPlan prints a run's steps as a stable, ordered plan — the dry run's
// output and the golden file's shape.
func RenderPlan(spec *fleet.Spec, res *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# dry run: %s (%s)\n", spec.Name, spec.Spec)
	fmt.Fprintf(&b, "# workspace %s on branch %s\n", res.Workspace.Name, res.Workspace.Branch)
	for _, s := range res.Steps {
		fmt.Fprintf(&b, "%-9s %-9s %s", s.Stage, s.Action, s.Detail)
		if s.Skipped {
			fmt.Fprintf(&b, "   [skipped: %s]", s.Reason)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "# would end in state: %s\n", res.State)
	return b.String()
}

// superviseService checks that a supervised service is still answering on its
// allocated port. The validator requires a supervised service to name a real
// allocated port (not merely an interpolatable name), so there is something
// concrete to check rather than an assumption — but the missing-port branch below
// stays, because this is reachable from a record written by an older version.
// settle is how long superviseService waits for a service to come up before
// calling it dead. "Did it come up?" (right after the start command returned)
// and "is it still up?" (a later check) are different questions: `docker compose
// up -d` returns before postgres accepts TCP, and connection-refused comes back
// instantly, so a single dial reported a perfectly healthy service as dead. This
// mirrors the rule probe.go already documents for HTTP probes.
func (e *Engine) superviseService(sv fleet.Service, rec *workspace.Record, settle time.Duration) (Step, bool) {
	step := Step{Stage: "supervise", Action: "check", Detail: sv.Name}
	if e.DryRun {
		step.Skipped, step.Reason = true, "dry run"
		return step, true
	}
	port, ok := rec.Ports[sv.PortVar]
	if !ok {
		step.Reason = "no allocated port for " + sv.PortVar
		return step, false
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(settle)
	var err error
	for {
		if err = e.Dial(addr, 2*time.Second); err == nil {
			return step, true
		}
		if !time.Now().Before(deadline) {
			step.Reason = err.Error()
			return step, false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Supervise re-checks every supervised service in the spec and records the
// resulting state.
//
// NOTHING CALLS THIS YET outside tests: the supervision cadence lands in C5 with
// the teardown executor. Until then a service that dies is caught only by the
// check inside a materialise pass, not between passes. Said plainly here for the
// same reason hooks.on_stop says it — a promise the code does not keep is worse
// than an absent feature. When it is wired, the caller decides the cadence: a workspace that was ready and whose service has since
// died must stop reading ready — and one that was marked unhealthy by this very
// check must be able to read ready again once the service answers, or the first
// blip would condemn it forever.
func (e *Engine) Supervise(spec *fleet.Spec, name string) (string, error) {
	// D6: every path in validates. Without this, an invalid spec (a supervised
	// service with no port_var, say) would mark a perfectly healthy workspace
	// unhealthy on the strength of a document the validator rejects.
	if issues := fleet.Validate(spec); len(issues) > 0 {
		return "", fmt.Errorf("spec is invalid: %s", issues[0].Error())
	}
	rec, err := e.Prov.Load(name)
	if err != nil {
		return "", err
	}
	if rec == nil {
		return "", fmt.Errorf("no such workspace %q", name)
	}
	// Only a workspace that actually completed a materialise has a supervision
	// verdict worth revising. Without this, two calls would walk a
	// never-materialised workspace provisioning -> unhealthy -> ready on the
	// strength of one TCP dial, with no file rendered and no command run — and
	// `ready` is the state a workspace is handed to an agent in.
	if rec.State != workspace.StateReady && rec.State != workspace.StateUnhealthy {
		return rec.State, nil
	}
	for _, sv := range spec.Workspace.Materialise.Services {
		if !sv.Supervise {
			continue
		}
		if _, ok := e.superviseService(sv, rec, 0); !ok {
			reason := supervisionReason + sv.Name
			if err := e.Prov.SetStateReason(name, workspace.StateUnhealthy, reason); err != nil {
				return "", err
			}
			return workspace.StateUnhealthy, nil
		}
	}
	// Everything supervised is answering. Clear an unhealthy that supervision
	// itself set; leave one a failed health probe set, because this check knows
	// nothing about whether that condition cleared.
	if rec.State == workspace.StateUnhealthy && strings.HasPrefix(rec.Reason, supervisionReason) {
		if err := e.Prov.SetStateReason(name, workspace.StateReady, ""); err != nil {
			return "", err
		}
		return workspace.StateReady, nil
	}
	return rec.State, nil
}

// serviceSettle is the default grace a freshly started service gets to bind its
// port. `docker compose up -d` returns before postgres accepts TCP.
const serviceSettle = 30 * time.Second

// settle is the configured grace, or the package default when unset.
func (e *Engine) settle() time.Duration {
	if e.Settle > 0 {
		return e.Settle
	}
	if e.Settle < 0 {
		return 0 // explicitly no grace, for tests that assert the dead path
	}
	return serviceSettle
}

// supervisionReason prefixes the reason supervision writes, so Supervise can
// recognise its own verdict and clear only that.
const supervisionReason = "supervise: service not answering: "
