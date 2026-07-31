package tmux

import "testing"

func TestSessionNameRoundTrip(t *testing.T) {
	for _, id := range []string{"abc123", "f8b373afddb6574a", "0"} {
		name := SessionName(id)
		got, ok := IDFromName(name)
		if !ok || got != id {
			t.Errorf("round-trip %q: name=%q -> id=%q ok=%v", id, name, got, ok)
		}
	}
}

func TestIDFromNameRejectsForeign(t *testing.T) {
	for _, name := range []string{"grok", "work", "menagerie", "mysession"} {
		if _, ok := IDFromName(name); ok {
			t.Errorf("IDFromName(%q) should reject a non-menagerie session", name)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"claude":            "claude",             // bare word untouched
		"":                  "''",                 // empty -> ''
		"echo hi":           "'echo hi'",          // space
		"a;b":               "'a;b'",              // shell metachar
		"it's":              `'it'\''s'`,          // embedded single quote
		"--flag=value":      "'--flag=value'",     // '=' is quoted (belt-and-suspenders)
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"sh", "-c", "echo hi; sleep 1"})
	want := "sh -c 'echo hi; sleep 1'"
	if got != want {
		t.Errorf("shellJoin = %q, want %q", got, want)
	}
}
