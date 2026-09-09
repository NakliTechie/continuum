package config

import "testing"

func TestOvernightDedicatedACPUsesNoSubcommand(t *testing.T) {
	for _, name := range []string{"claude-code-acp", "gptme-acp", "openhands-acp", "vibe-acp", "kode-acp"} {
		got := KnownAgents[name].ACPArgsOrDefault()
		if len(got) != 0 {
			t.Errorf("%s declares no subcommand but receives %q", name, got)
		}
	}
}
