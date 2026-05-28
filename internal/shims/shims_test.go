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
