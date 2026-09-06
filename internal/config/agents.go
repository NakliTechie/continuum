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
	// Verified in this project.
	"mini":        {Command: "mini"},   // mini-swe-agent
	"claude-code": {Command: "claude"}, // Claude Code
	"omp":         {Command: "omp", Transports: []string{"acp"}},

	// Named in the project's own docs.
	"aider": {Command: "aider"},

	// Seeded from herdr's published integrations list (2026-09-07). Ids follow
	// herdr; commands are the CLI's binary where known, else the id.
	"antigravity-cli": {Command: "antigravity"},
	"codex":           {Command: "codex"},        // OpenAI Codex CLI
	"copilot":         {Command: "copilot"},      // GitHub Copilot CLI
	"cursor":          {Command: "cursor-agent"}, // Cursor CLI
	"devin":           {Command: "devin"},
	"droid":           {Command: "droid"}, // Factory Droid
	"grok":            {Command: "grok"},
	"hermes":          {Command: "hermes"},
	"kilo":            {Command: "kilo"},
	"kimi":            {Command: "kimi"},
	"mastracode":      {Command: "mastracode"},
	"opencode":        {Command: "opencode"},
	"pi":              {Command: "pi"},
	"qodercli":        {Command: "qodercli"},
	"qwen":            {Command: "qwen"},
}

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
