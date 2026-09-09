// Package fleet is the single ingress for a fleet spec (D6): every path into
// materialisation parses and validates here first, so no other package needs to
// re-check the shape of what it was handed.
package fleet

// SpecVersion is the only accepted value of the `spec` field. A spec that does
// not name it is refused rather than guessed at — a future v2 must be a
// deliberate migration, not a silent reinterpretation of a v1 document.
const SpecVersion = "menagerie.fleet.v1"

// Spec is a whole fleet.json document.
type Spec struct {
	Spec string `json:"spec"`
	Name string `json:"name"`
	Repo string `json:"repo"`
	// Topology is required and has no default. The handoff's reasoning: roughly
	// four in five multi-agent failures are specification and coordination
	// problems, and an unnamed topology is the first of them. The *value* is not
	// enumerated here — the requirement is that the author names it, and an
	// invented enum would refuse valid future shapes.
	Topology string `json:"topology"`

	Workspace Workspace `json:"workspace"`
	Roster    []Role    `json:"roster"`
	Budgets   *Budgets  `json:"budgets,omitempty"`
	Exits     *Exits    `json:"exits,omitempty"`
}

// Workspace describes how one agent's working copy is isolated and made runnable.
type Workspace struct {
	Isolation    string      `json:"isolation"`
	BranchPrefix string      `json:"branch_prefix,omitempty"`
	Materialise  Materialise `json:"materialise"`
	Teardown     *Teardown   `json:"teardown,omitempty"`
}

// Materialise is the environment-materialisation block. Its stages run in a
// fixed order — ports → files → commands → services → health → escape — which is
// not configurable: a file that needs a port must be able to read it, and a
// command that needs a file must run after it.
type Materialise struct {
	Ports    []Port    `json:"ports,omitempty"`
	Files    []File    `json:"files,omitempty"`
	Commands []Command `json:"commands,omitempty"`
	Services []Service `json:"services,omitempty"`
	Health   []Probe   `json:"health,omitempty"`
	// Escape runs last, after everything declared, with all variables exported.
	// It is the honest hatch, not the default path.
	Escape string `json:"escape,omitempty"`
	// Hooks are a lifecycle layer beside the six stages, not a seventh stage:
	// the ports->files->commands->services->health->escape order is unchanged,
	// and on_start runs once that whole sequence has succeeded.
	Hooks *Hooks `json:"hooks,omitempty"`
}

// Hooks run at workspace lifecycle transitions. on_start runs after a successful
// materialise.
//
// on_stop and on_destroy are DECLARED but not yet executed: the teardown
// executor lands in C5, and until it does, nothing runs them. Validating and
// documenting a field that silently does nothing is how an author's on_destroy
// volume cleanup passes review and never runs, so it is said plainly here rather
// than implied by the schema.
type Hooks struct {
	OnStart   string `json:"on_start,omitempty"`
	OnStop    string `json:"on_stop,omitempty"`
	OnDestroy string `json:"on_destroy,omitempty"`
}

// Port is a variable bound to a port the relay allocates from Range. Hardcoded
// ports are the top cause of parallel-workspace collisions (D5).
type Port struct {
	Name  string `json:"name"`
	Range [2]int `json:"range"`
}

// File is either a copy (From) or a rendered template (Template). Exactly one.
type File struct {
	From     string   `json:"from,omitempty"`
	Template string   `json:"template,omitempty"`
	To       string   `json:"to"`
	Vars     []string `json:"vars,omitempty"`
}

// Command is a setup step. CacheKey names a file whose hash decides whether the
// step can be skipped — what turns a 3-minute `npm ci` into a no-op on
// workspaces 2 through N.
type Command struct {
	Run      string `json:"run"`
	CacheKey string `json:"cache_key,omitempty"`
}

// Service starts once per repo, unless PortVar is set — then once per workspace,
// because a per-workspace port implies a per-workspace instance.
type Service struct {
	Name    string `json:"name"`
	Run     string `json:"run"`
	PortVar string `json:"port_var,omitempty"`
	// Supervise re-checks the service after its health probe passed, during each
	// materialise pass. A probe is a moment, not a guarantee.
	//
	// It is NOT yet continuous: nothing polls between passes, because
	// Engine.Supervise has no caller until C5 wires a cadence. A service that dies
	// between passes is therefore not noticed until the next materialise. Said
	// plainly because a promise the code does not keep is worse than an absent
	// feature. Supervision needs something concrete to check, so it requires
	// PortVar — and PortVar must name an allocated port, not merely an
	// interpolatable name.
	Supervise bool `json:"supervise,omitempty"`
}

// Probe gates completion. A workspace whose probes fail is `unhealthy`, never `ready`.
type Probe struct {
	Probe    string `json:"probe"`
	URL      string `json:"url,omitempty"`
	Run      string `json:"run,omitempty"`
	TimeoutS int    `json:"timeout_s,omitempty"`
}

// Teardown reverses materialisation. KeepBranch defaults to false only when the
// block is present and says so; a missing Teardown keeps the branch.
type Teardown struct {
	Commands   []string `json:"commands,omitempty"`
	KeepBranch bool     `json:"keep_branch,omitempty"`
}

// Role is one row of the roster: how many of which agent, at what budget.
type Role struct {
	Role                string `json:"role"`
	Agent               string `json:"agent"`
	Model               string `json:"model,omitempty"`
	Count               int    `json:"count"`
	ContextBudgetTokens int    `json:"context_budget_tokens,omitempty"`
	MaxDepth            int    `json:"max_depth,omitempty"`
}

// Budgets caps spend and wall-clock. OnExceed is "drain" (finish the current
// unit, checkpoint, stop) or "kill" (immediate); empty means drain.
type Budgets struct {
	PerAgentUSD          float64 `json:"per_agent_usd,omitempty"`
	PerRunUSD            float64 `json:"per_run_usd,omitempty"`
	PerAgentWallclockMin int     `json:"per_agent_wallclock_min,omitempty"`
	OnExceed             string  `json:"on_exceed,omitempty"`
}

// Exits are the run's loop-breakers.
type Exits struct {
	NoProgressRepeats int      `json:"no_progress_repeats,omitempty"`
	ConvergeOn        []string `json:"converge_on,omitempty"`
}

// BuiltinVars are the names ${VAR} interpolation resolves beyond the declared
// ports. Nothing else: no environment inheritance, no arbitrary shell expansion
// in declared fields.
var BuiltinVars = []string{"WORKSPACE", "BRANCH", "REPO_ROOT"}
