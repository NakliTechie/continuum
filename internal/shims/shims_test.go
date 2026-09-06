package shims

import "testing"

func TestLooksLikeNeedsInput(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"y/n prompt", "Proceed with migration? (y/n) ", true},
		{"bracket y/n", "Overwrite file [y/n]", true},
		{"trailing question", "Which branch do you want to use?", true},
		{"are you sure", "Are you sure you want to continue", true},
		{"press enter", "Press enter to continue", true},
		{"inquirer caret", "❯ option one", true},
		{"bare dollar prompt ignored", "user@host:~$ ", false},
		{"bare hash prompt ignored", "# ", false},
		{"plain statement", "Applying migration 0042_add_users...", false},
		{"period-terminated", "This will modify 3 tables.", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LooksLikeNeedsInput([]byte(c.in)); got != c.want {
				t.Errorf("LooksLikeNeedsInput(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestLooksLikeRateLimited(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"429 rate limit", "Error 429: rate limit exceeded. Please retry after 30s.", true},
		{"too many requests", "HTTP 429 Too Many Requests", true},
		{"quota", "Your quota exceeded for this billing period", true},
		{"overloaded", "The model is currently overloaded", true},
		{"hyphenated", "Provider returned a rate-limit error", true},
		{"normal output", "Streaming tokens: hello world", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LooksLikeRateLimited([]byte(c.in)); got != c.want {
				t.Errorf("LooksLikeRateLimited(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func repeat(line string, n int) []string {
	out := make([]string, 0, n+4)
	out = append(out, "starting up", "reading files")
	for i := 0; i < n; i++ {
		out = append(out, line)
	}
	return out
}

func TestLooksLikeStalled(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"identical error looping", repeat("Error: ECONNRESET, retrying connection to api", 8), true},
		{"timestamped retry loop", []string{
			"[12:00:01] retrying request to https://api.example.com",
			"[12:00:04] retrying request to https://api.example.com",
			"[12:00:07] retrying request to https://api.example.com",
			"[12:00:10] retrying request to https://api.example.com",
			"[12:00:13] retrying request to https://api.example.com",
			"[12:00:16] retrying request to https://api.example.com",
			"[12:00:19] retrying request to https://api.example.com",
		}, true},
		{"ansi-styled loop still caught", repeat("\x1b[31mError:\x1b[0m tool call failed, trying again now", 7), true},
		{"healthy varied progress", []string{
			"Reading src/app.ts", "Editing src/app.ts", "Running tests",
			"3 passed", "Reading src/api.ts", "Editing src/api.ts", "Committing changes",
		}, false},
		{"short repeated lines ignored (spinner)", repeat("...", 12), false},
		{"only a few repeats", repeat("processing the next batch of records", 3), false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LooksLikeStalled(c.lines); got != c.want {
				t.Errorf("LooksLikeStalled(%v) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestNewRegistryIsDataDriven(t *testing.T) {
	reg := NewRegistry(map[string]string{"claude-code": "claude", "qwen": "qwen", "custom": ""})
	for _, id := range []string{"claude-code", "qwen", "custom"} {
		if _, ok := reg[id]; !ok {
			t.Errorf("registry is missing %q", id)
		}
	}
	if _, ok := reg["mini"]; ok {
		t.Error("registry invented an agent that was not configured")
	}
	if _, ok := reg["custom"].(Custom); !ok {
		t.Error("custom must keep its own shim, not become a Generic")
	}
}

func TestGenericSpawnUsesConfiguredCommand(t *testing.T) {
	cmd, err := Generic{ID: "claude-code", Cmd: "claude"}.Spawn("/tmp", []string{"--help"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cmd.Args[0]; got != "claude" {
		t.Errorf("exec = %q, want %q", got, "claude")
	}
	if got := cmd.Args[1]; got != "--help" {
		t.Errorf("args not passed through: %v", cmd.Args)
	}
	if cmd.Dir != "/tmp" {
		t.Errorf("cwd = %q", cmd.Dir)
	}
}

func TestGenericSpawnFallsBackToID(t *testing.T) {
	cmd, err := Generic{ID: "opencode"}.Spawn("", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cmd.Args[0]; got != "opencode" {
		t.Errorf("exec = %q, want %q", got, "opencode")
	}
}
