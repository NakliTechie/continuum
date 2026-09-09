package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every invalid fixture names the path a validator must point at. A fixture that
// fails for the wrong reason is as bad as one that passes: the error path is the
// contract an editor puts its cursor on.
var invalidWantPath = map[string]string{
	"missing-spec.json":                   "/spec",
	"wrong-spec-version.json":             "/spec",
	"missing-name.json":                   "/name",
	"missing-topology.json":               "/topology",
	"missing-repo.json":                   "/repo",
	"empty-roster.json":                   "/roster",
	"roster-missing-agent.json":           "/roster/0/agent",
	"roster-zero-count.json":              "/roster/0/count",
	"port-range-inverted.json":            "/workspace/materialise/ports/0/range",
	"file-neither-from-nor-template.json": "/workspace/materialise/files/0",
	"command-missing-run.json":            "/workspace/materialise/commands/0/run",
	"bad-on-exceed.json":                  "/budgets/on_exceed",
	"supervise-without-port-var.json":     "/workspace/materialise/services/0/supervise",
	"hook-undeclared-var.json":            "/workspace/materialise/hooks/on_start",
	"supervise-with-blank-port-var.json":  "/workspace/materialise/services/0/supervise",
}

func read(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(filepath.Join("testdata", dir))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestValidFixturesPass(t *testing.T) {
	fs := names(t, "valid")
	if len(fs) != 7 {
		t.Fatalf("valid fixtures = %d, want 7", len(fs))
	}
	for _, n := range fs {
		if _, issues := ValidateBytes(read(t, "valid", n)); len(issues) > 0 {
			t.Errorf("%s: want no issues, got %v", n, issues)
		}
	}
}

func TestInvalidFixturesRejectedAtTheRightPath(t *testing.T) {
	fs := names(t, "invalid")
	if len(fs) != 15 {
		t.Fatalf("invalid fixtures = %d, want 15", len(fs))
	}
	for _, n := range fs {
		want, ok := invalidWantPath[n]
		if !ok {
			t.Fatalf("%s has no expected error path — add one rather than loosening the test", n)
		}
		_, issues := ValidateBytes(read(t, "invalid", n))
		if len(issues) == 0 {
			t.Errorf("%s: accepted, want rejection at %s", n, want)
			continue
		}
		var found bool
		for _, is := range issues {
			if is.Path == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no issue at %s; got %v", n, want, issues)
		}
	}
}

func TestSecretFixturesFireTheLeakLint(t *testing.T) {
	fs := names(t, "secrets")
	if len(fs) != 5 {
		t.Fatalf("secret fixtures = %d, want 5", len(fs))
	}
	for _, n := range fs {
		_, issues := ValidateBytes(read(t, "secrets", n))
		var found bool
		for _, is := range issues {
			if is.Code == "secret_in_spec" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: leak lint did not fire; got %v", n, issues)
		}
	}
}

// A credential-shaped string must not be reported for an ordinary opaque value:
// a lint that cries wolf gets disabled, and then D4 is unenforced.
func TestLeakLintDoesNotFireOnOrdinaryStrings(t *testing.T) {
	ok := [][]byte{
		[]byte(`{"run":"npm ci --prefer-offline","cache_key":"package-lock.json"}`),
		[]byte(`{"run":"deploy --token \"$DEPLOY_TOKEN\""}`),
		[]byte(`{"url":"postgres://localhost:5432/app"}`),
		[]byte(`{"sha":"sk-"}`),
		[]byte(`{"note":"AKIA is a prefix"}`),
	}
	for _, b := range ok {
		if is := LintSecrets(b); len(is) > 0 {
			t.Errorf("false positive on %s: %v", b, is)
		}
	}
}

// ${VAR} resolves against allocated ports and the builtins, nothing else.
func TestUndeclaredInterpolationIsRejected(t *testing.T) {
	s := &Spec{Spec: SpecVersion, Name: "x", Repo: ".", Topology: "flat",
		Workspace: Workspace{Isolation: "worktree", Materialise: Materialise{
			Ports:    []Port{{Name: "PORT", Range: [2]int{4000, 4001}}},
			Commands: []Command{{Run: "start --port ${PORT} --db ${DB_URL}"}},
		}},
		Roster: []Role{{Role: "worker", Agent: "codex", Count: 1}}}
	issues := Validate(s)
	var got string
	for _, is := range issues {
		if is.Code == "undeclared_var" {
			got = is.Message
		}
	}
	if !strings.Contains(got, "DB_URL") {
		t.Fatalf("want an undeclared_var issue naming DB_URL, got %v", issues)
	}
	if strings.Contains(got, "${PORT}") {
		t.Fatalf("PORT is declared and must not be flagged: %v", issues)
	}
}

// An unknown health probe kind is refused rather than skipped at run time.
func TestUnknownProbeKindIsRejected(t *testing.T) {
	s := &Spec{Spec: SpecVersion, Name: "x", Repo: ".", Topology: "flat",
		Workspace: Workspace{Isolation: "worktree", Materialise: Materialise{
			Health: []Probe{{Probe: "telepathy"}}}},
		Roster: []Role{{Role: "worker", Agent: "codex", Count: 1}}}
	for _, is := range Validate(s) {
		if is.Path == "/workspace/materialise/health/0/probe" && is.Code == "unsupported" {
			return
		}
	}
	t.Fatal("unknown probe kind was accepted")
}

// A file destination may not escape the workspace root, however it is spelled.
func TestFileDestinationCannotEscapeWorkspace(t *testing.T) {
	for _, to := range []string{"/etc/passwd", "../outside/.env"} {
		s := &Spec{Spec: SpecVersion, Name: "x", Repo: ".", Topology: "flat",
			Workspace: Workspace{Isolation: "worktree", Materialise: Materialise{
				Files: []File{{From: "a", To: to}}}},
			Roster: []Role{{Role: "worker", Agent: "codex", Count: 1}}}
		var found bool
		for _, is := range Validate(s) {
			if is.Code == "escapes_workspace" {
				found = true
			}
		}
		if !found {
			t.Errorf("destination %q was accepted", to)
		}
	}
}
