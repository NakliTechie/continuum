package config

import (
	"errors"
	"reflect"
	"testing"
)

// fakeLookPath resolves only the commands named in found.
func fakeLookPath(found ...string) func(string) (string, error) {
	set := map[string]bool{}
	for _, f := range found {
		set[f] = true
	}
	return func(cmd string) (string, error) {
		if set[cmd] {
			return "/usr/local/bin/" + cmd, nil
		}
		return "", errors.New("not found")
	}
}

func TestResolveAgentsAdvertisesOnlyWhatIsInstalled(t *testing.T) {
	c := &Config{}
	detected, missing := c.ResolveAgents(fakeLookPath("claude", "opencode"))

	if !reflect.DeepEqual(detected, []string{"claude-code", "opencode"}) {
		t.Fatalf("detected = %v, want [claude-code opencode]", detected)
	}
	if got := c.AgentNames(); !reflect.DeepEqual(got, []string{"claude-code", "custom", "opencode"}) {
		t.Fatalf("advertised = %v, want [claude-code custom opencode]", got)
	}
	for _, absent := range []string{"codex", "mini", "omp", "qwen"} {
		if _, ok := c.Agents[absent]; ok {
			t.Errorf("agent %q was advertised but its command is not on PATH", absent)
		}
	}
	if len(missing) != len(KnownAgents)-len(detected) {
		t.Errorf("missing = %d, want %d", len(missing), len(KnownAgents)-len(detected))
	}
}

func TestResolveAgentsKeepsConfiguredAgentsRegardlessOfPath(t *testing.T) {
	c := &Config{Agents: map[string]Agent{
		"house-bot":   {Command: "/opt/bin/house-bot"},
		"claude-code": {}, // bare entry: command filled from the registry
	}}
	c.ResolveAgents(fakeLookPath()) // nothing on PATH at all

	if _, ok := c.Agents["house-bot"]; !ok {
		t.Fatal("an explicitly configured agent was dropped because it is not on PATH")
	}
	if got := c.Agents["claude-code"].Command; got != "claude" {
		t.Errorf("bare [agents.claude-code] command = %q, want %q", got, "claude")
	}
	if _, ok := c.Agents[CustomAgent]; !ok {
		t.Error("custom must always be available")
	}
}

func TestResolveAgentsConfigWinsOverRegistry(t *testing.T) {
	c := &Config{Agents: map[string]Agent{"codex": {Command: "/opt/wrapper/codex-wrapped"}}}
	c.ResolveAgents(fakeLookPath("codex"))

	if got := c.Agents["codex"].Command; got != "/opt/wrapper/codex-wrapped" {
		t.Errorf("codex command = %q, want the configured wrapper", got)
	}
}

func TestResolveAgentsUnknownConfiguredAgentDefaultsCommandToItsID(t *testing.T) {
	c := &Config{Agents: map[string]Agent{"weirdbot": {}}}
	c.ResolveAgents(fakeLookPath())
	if got := c.Agents["weirdbot"].Command; got != "weirdbot" {
		t.Errorf("command = %q, want %q", got, "weirdbot")
	}
}

func TestKnownAgentsClaimACPOnlyWhereVerified(t *testing.T) {
	for name, a := range KnownAgents {
		if a.Command == "" {
			t.Errorf("KnownAgents[%q] has no command to probe", name)
		}
		if a.SupportsACP() && name != "omp" {
			t.Errorf("KnownAgents[%q] claims acp; only hand-verified agents may", name)
		}
	}
}

func TestDefaultConfigShipsNoUndetectedAgents(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.AgentNames(); !reflect.DeepEqual(got, []string{CustomAgent}) {
		t.Errorf("default agents = %v, want just [%s] (the rest are detected)", got, CustomAgent)
	}
}

func TestAgentCommands(t *testing.T) {
	c := &Config{Agents: map[string]Agent{"a": {Command: "abin"}, "b": {Command: "bbin"}}}
	if got := c.AgentCommands(); !reflect.DeepEqual(got, map[string]string{"a": "abin", "b": "bbin"}) {
		t.Errorf("AgentCommands() = %v", got)
	}
}
