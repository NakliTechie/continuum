package config

import (
	"os/exec"
	"sort"
)

// KnownAgents is the built-in registry of coding-agent CLIs the relay knows how
// to look for. It is a DETECTION list, not a capability claim: at startup the
// relay probes each command on PATH and only advertises the ones it actually
// finds (ResolveAgents), so the browser's agent dropdown lists what this machine
// can really spawn.
//
// The map key is the id shown in the UI; Command is the executable to probe and
// exec. A wrong or renamed Command costs nothing but a missed detection — it can
// never cause a wrong binary to run, because the relay execs exactly the command
// named here (or the one you pin in relay.toml, which always wins).
//
// Transports is deliberately conservative: everything is pty-only except agents
// whose ACP support has been verified by hand. Claiming acp for an agent that
// does not speak it yields a broken session, so an unverified agent gets pty.
//
// This list ships with the binary and is refreshed at release time. It is never
// fetched at runtime: the relay would then be taking the names of programs it
// executes from the network, and a periodic check would be a phone-home. To add
// an agent before the next release, put it in relay.toml.
var KnownAgents = map[string]Agent{
	// --- verified in this project -------------------------------------------
	"mini":        {Command: "mini"},                                             // mini-swe-agent
	"claude-code": {Command: "claude", ResumeArgs: []string{"--resume", "{id}"}}, // Claude Code
	"omp":         {Command: "omp", Transports: []string{"acp"}, ACPArgs: []string{"acp"}, ResumeArgs: []string{"--resume={id}"}},
	"aider":       {Command: "aider"},

	// --- vendor CLIs, binary names verified against npm bin maps, PyPI/pyproject
	// scripts, Cargo [[bin]], Homebrew casks or the vendor's own install doc ---
	"agy":       {Command: "agy", ResumeArgs: []string{"--conversation", "{id}"}}, // Google Antigravity CLI
	"amazon-q":  {Command: "qterm"},                                               // AWS Amazon Q (bare `q` is too generic to probe)
	"auggie":    {Command: "auggie"},                                              // Augment Code
	"blackbox":  {Command: "blackbox"},
	"codai":     {Command: "codai"},
	"codebuddy": {Command: "codebuddy"}, // Tencent
	"codebuff":  {Command: "codebuff"},
	"codex":     {Command: "codex", ResumeArgs: []string{"resume", "{id}"}},  // OpenAI Codex CLI
	"cody":      {Command: "cody"},                                           // Sourcegraph
	"continue":  {Command: "cn"},                                             // Continue CLI
	"copilot":   {Command: "copilot", ResumeArgs: []string{"--resume={id}"}}, // GitHub Copilot CLI
	"crow-cli":  {Command: "crow-cli"},
	"crush":     {Command: "crush"}, // Charm
	// Cursor's current docs call the binary `agent`, but xAI's Grok Build also
	// installs `agent`, so probing that name cannot tell them apart. We probe the
	// unambiguous `cursor-agent` the installer has shipped; pin `agent` by hand
	// in relay.toml if that is what you have.
	"cursor":       {Command: "cursor-agent", ResumeArgs: []string{"--resume", "{id}"}},
	"devin":        {Command: "devin", Transports: []string{"acp"}, ACPArgs: []string{"acp"}, ResumeArgs: []string{"--resume", "{id}"}},
	"droid":        {Command: "droid", ResumeArgs: []string{"--resume", "{id}"}}, // Factory
	"gemini":       {Command: "gemini", Transports: []string{"acp"}, ACPArgs: []string{"--acp"}},
	"gpt-engineer": {Command: "gpt-engineer"},
	"gptme":        {Command: "gptme"},
	"grok":         {Command: "grok", ResumeArgs: []string{"--resume", "{id}"}},
	"hermes":       {Command: "hermes", ResumeArgs: []string{"--resume", "{id}"}}, // Nous Research
	"iflow":        {Command: "iflow"},
	"interpreter":  {Command: "interpreter"}, // Open Interpreter
	"jules":        {Command: "jules"},       // Google
	"junie":        {Command: "junie"},       // JetBrains
	"kaagum":       {Command: "kaagum"},
	"kilo":         {Command: "kilo", ResumeArgs: []string{"--session", "{id}"}},
	"kimi":         {Command: "kimi", ResumeArgs: []string{"--session", "{id}"}}, // Moonshot
	"kiro-cli":     {Command: "kiro-cli"},                                        // AWS Kiro
	"kode":         {Command: "kode"},
	"localharness": {Command: "localharness"},
	"mastracode":   {Command: "mastracode", ResumeArgs: []string{"--thread", "{id}"}},
	"micro-agent":  {Command: "micro-agent"}, // Builder.io
	"nanocoder":    {Command: "nanocoder", Transports: []string{"acp"}, ACPArgs: []string{"--acp"}},
	"octofriend":   {Command: "octofriend"},
	"openclaw":     {Command: "openclaw"},
	"opencode":     {Command: "opencode", ResumeArgs: []string{"--session", "{id}"}},
	"openhands":    {Command: "openhands"},
	"pi":           {Command: "pi", ResumeArgs: []string{"--session", "{id}"}}, // @mariozechner/pi-coding-agent
	"plandex":      {Command: "plandex"},
	"pochi":        {Command: "pochi"}, // TabbyML
	"pool":         {Command: "pool", Transports: []string{"acp"}, ACPArgs: []string{"acp"}},
	"qoder":        {Command: "qoder", Transports: []string{"acp"}, ACPArgs: []string{"--acp"}},
	"qodercli":     {Command: "qodercli", ResumeArgs: []string{"--resume", "{id}"}}, // same package, legacy name
	"qodo":         {Command: "qodo"},
	"qwen":         {Command: "qwen", ResumeArgs: []string{"--resume", "{id}"}},
	"ra-aid":       {Command: "ra-aid"},
	"refact":       {Command: "refact"},
	"roo":          {Command: "roo"}, // Roo Code
	"sigit":        {Command: "sigit", Transports: []string{"acp"}, ACPArgs: []string{"--acp"}},
	"stakpak":      {Command: "stakpak", Transports: []string{"acp"}, ACPArgs: []string{"acp"}},
	"sweagent":     {Command: "sweagent"}, // SWE-agent
	"vtcode":       {Command: "vtcode", Transports: []string{"acp"}, ACPArgs: []string{"acp"}},
	"warp":         {Command: "warp"},
	"aichat":       {Command: "aichat"},
	"cline":        {Command: "cline", Transports: []string{"acp"}, ACPArgs: []string{"--acp"}},
	"construct":    {Command: "construct", Transports: []string{"acp"}, ACPArgs: []string{"acp"}},

	// --- dedicated ACP binaries: the executable IS the ACP server, so no argv --
	"claude-code-acp": {Command: "claude-code-acp", Transports: []string{"acp"}, ACPArgs: []string{}}, // Zed's adapter for `claude`
	"gptme-acp":       {Command: "gptme-acp", Transports: []string{"acp"}, ACPArgs: []string{}},
	"openhands-acp":   {Command: "openhands-acp", Transports: []string{"acp"}, ACPArgs: []string{}},
	"vibe-acp":        {Command: "vibe-acp", Transports: []string{"acp"}, ACPArgs: []string{}}, // Mistral Vibe
	"kode-acp":        {Command: "kode-acp", Transports: []string{"acp"}, ACPArgs: []string{}},
}

// Deliberately NOT probed — the binary name is owned by a better-known tool, so
// detecting it would offer a "coding agent" that is something else entirely:
// `goose` (pressly/goose, a DB migration tool), `amp` (the amp.rs text editor),
// `agent` (claimed by both Cursor and xAI Grok Build), `code`/`coder` (VS Code,
// Coder), `q` (too generic), `forge` (Foundry's Ethereum tool), `mcode` (MiniMax
// Code and Femto Minion Code both take it), `vibe`, and the one- and two-letter
// aliases `i`, `cb`, `ma`, `sc`. Pin any of these by hand in relay.toml, where
// you know which one you installed. relay.toml.example carries paste-ready rows.

// CustomAgent is the id of the shim that takes its command from spawn.args. It
// is always available: it needs nothing on PATH.
const CustomAgent = "custom"

// ResolveAgents settles which agents this relay advertises, once, at startup.
//
//   - Every agent written in relay.toml is kept, detected or not — an explicit
//     entry is a statement of intent (a wrapper script, a binary off PATH). Its
//     Command is filled in from KnownAgents when the file omitted one.
//   - Every KnownAgent that is not already configured is added if its command
//     resolves on PATH.
//   - "custom" is always present.
//
// It returns the ids added by detection and the known ids that were not found,
// both sorted, for logging and `menagerie-relay agents`. lookPath is injected
// for tests; nil means exec.LookPath.
func (c *Config) ResolveAgents(lookPath func(string) (string, error)) (detected, missing []string) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if c.Agents == nil {
		c.Agents = map[string]Agent{}
	}

	// Configured entries win. Fill a missing command from the registry so an
	// entry like a bare `[agents.claude-code]` still knows to run `claude`.
	for name, a := range c.Agents {
		if a.Command != "" || name == CustomAgent {
			continue
		}
		if known, ok := KnownAgents[name]; ok {
			a.Command = known.Command
		} else {
			a.Command = name
		}
		c.Agents[name] = a
	}

	for name, known := range KnownAgents {
		if _, configured := c.Agents[name]; configured {
			continue
		}
		if _, err := lookPath(known.Command); err != nil {
			missing = append(missing, name)
			continue
		}
		c.Agents[name] = known
		detected = append(detected, name)
	}

	if _, ok := c.Agents[CustomAgent]; !ok {
		c.Agents[CustomAgent] = Agent{}
	}

	sort.Strings(detected)
	sort.Strings(missing)
	return detected, missing
}

// AgentCommands maps agent id to configured executable, for the shim registry.
func (c *Config) AgentCommands() map[string]string {
	cmds := make(map[string]string, len(c.Agents))
	for name, a := range c.Agents {
		cmds[name] = a.Command
	}
	return cmds
}
