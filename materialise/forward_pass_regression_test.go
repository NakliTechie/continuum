package materialise

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NakliTechie/continuum/fleet"
	"github.com/NakliTechie/continuum/workspace"
)

// A committed spec may read files inside the checkout or directly beside it;
// a symlink that points elsewhere, an absolute path into the operator's home,
// or a file nested under a sibling directory is refused before it is read.
func TestSourcePolicyBoundsWhatASpecMayRead(t *testing.T) {
	repo := testRepo(t)
	parent := filepath.Dir(repo)
	home := t.TempDir()
	secret := filepath.Join(home, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, ".env.local"), []byte("BESIDE=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(parent, "other-project"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "other-project", ".env"), []byte("OTHER=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "inside.env"), []byte("INSIDE=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(repo, "seed.env")); err != nil {
		t.Fatal(err)
	}
	n := 0
	run := func(from string) (string, error) {
		n++
		spec := &fleet.Spec{
			Spec: fleet.SpecVersion, Name: "t", Repo: ".", Topology: "flat",
			Workspace: fleet.Workspace{Isolation: "worktree", BranchPrefix: "agent/",
				Materialise: fleet.Materialise{Files: []fleet.File{{From: from, To: "copied"}}}},
			Roster: []fleet.Role{{Role: "worker", Agent: "codex", Count: 1}},
		}
		e := New(workspace.New(t.TempDir()))
		e.Exec, e.Prob = &RecordingExecutor{}, PassProber{}
		res, err := e.Run(spec, repo, fmt.Sprintf("w%d", n))
		if err != nil {
			return "", err
		}
		b, err := os.ReadFile(filepath.Join(res.Workspace.Path, "copied"))
		return string(b), err
	}
	if got, err := run("inside.env"); err != nil || got != "INSIDE=1\n" {
		t.Fatalf("file inside the checkout: %q %v", got, err)
	}
	if got, err := run("../.env.local"); err != nil || got != "BESIDE=1\n" {
		t.Fatalf("file beside the checkout: %q %v", got, err)
	}
	for _, from := range []string{"seed.env", secret, "../other-project/.env", "../../" + filepath.Base(filepath.Dir(parent)) + "/x"} {
		got, err := run(from)
		if err == nil || got == "PRIVATE" || got == "OTHER=1\n" {
			t.Fatalf("source %q must be refused: got %q err %v", from, got, err)
		}
		if !strings.Contains(err.Error(), "outside the checkout") && !strings.Contains(err.Error(), "reading") {
			t.Fatalf("source %q refused for the wrong reason: %v", from, err)
		}
	}
}

// ${VAR} expansion is a single pass over the input: a substituted value that
// itself contains ${...} stays literal whatever order the map iterates in.
func TestInterpolateIsSinglePassAndOrderIndependent(t *testing.T) {
	vars := map[string]string{"PORT": "4000", "BRANCH": "${PORT}/w1", "WORKSPACE": "w1"}
	for i := 0; i < 50; i++ {
		if got := interpolate("b=${BRANCH} p=${PORT} u=${UNKNOWN} x=${", vars); got != "b=${PORT}/w1 p=4000 u=${UNKNOWN} x=${" {
			t.Fatalf("iteration %d: %q", i, got)
		}
	}
}

// A destination may reference declared variables: the validator accepts them,
// so the engine must expand them rather than write a literal ${WORKSPACE}.
func TestFileDestinationIsInterpolated(t *testing.T) {
	repo := testRepo(t)
	e, _, fsys := engineWithFakes(t, repo)
	spec := fullSpec()
	spec.Workspace.Materialise.Files = []fleet.File{{From: "seed.env", To: "cfg/${WORKSPACE}.env"}}
	res, err := e.Run(spec, repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fsys.Writes[filepath.Join(res.Workspace.Path, "cfg", "w1.env")]; !ok {
		t.Fatalf("destination not interpolated: %v", keys(fsys.Writes))
	}
	if res.Steps[1].Detail != "seed.env -> cfg/w1.env" {
		t.Fatalf("plan detail must show source and expanded destination: %q", res.Steps[1].Detail)
	}
}

// The cache key is read from the tree the command runs in, so a branch that
// changes its lockfile re-runs setup even though the main checkout did not.
func TestCacheKeyReadsTheWorktree(t *testing.T) {
	repo := testRepo(t)
	e, ex, fsys := engineWithFakes(t, repo)
	res, err := e.Run(fullSpec(), repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if n := ex.Count("npm ci"); n != 1 {
		t.Fatalf("npm ci ran %d times, want 1", n)
	}
	fsys.Seed[filepath.Join(res.Workspace.Path, "package-lock.json")] = []byte(`{"lockfileVersion":9}`)
	if _, err := e.Run(fullSpec(), repo, "w1"); err != nil {
		t.Fatal(err)
	}
	if n := ex.Count("npm ci"); n != 2 {
		t.Fatalf("a changed lockfile in the worktree must re-run setup: ran %d", n)
	}
}

// An unreadable cache is an error, not an empty cache: silently treating it as
// empty would start every once-per-repo service again.
func TestCorruptCacheFailsTheRun(t *testing.T) {
	repo := testRepo(t)
	e, ex, _ := engineWithFakes(t, repo)
	if _, err := e.Run(fullSpec(), repo, "w1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.cachePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := ex.Count("docker compose up -d db")
	_, err := e.Run(fullSpec(), repo, "w2")
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("corrupt cache must fail the run: %v", err)
	}
	if ex.Count("docker compose up -d db") != before {
		t.Fatal("the service was started again from a corrupt cache")
	}
	// A spec whose only cached thing is the service reaches the service path
	// directly; it must fail there too, before the service runs.
	svcOnly := fullSpec()
	svcOnly.Workspace.Materialise.Commands = nil
	_, err = e.Run(svcOnly, repo, "w3")
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("service path must refuse a corrupt cache: %v", err)
	}
	if ex.Count("docker compose up -d db") != before {
		t.Fatal("the service ran before the corrupt cache was noticed")
	}
}

// Declared commands see the workspace variables and PATH, never the relay's
// own environment: a secret must arrive through files[].from, which is what
// the lint now says.
func TestDeclaredCommandsDoNotInheritTheRelayEnvironment(t *testing.T) {
	t.Setenv("DEPLOY_TOKEN", "relay-secret")
	out, err := ShellExecutor{}.Run(t.TempDir(), envSlice(map[string]string{"WORKSPACE": "w1"}), `printf '%s|%s' "$DEPLOY_TOKEN" "$WORKSPACE"`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "|w1" {
		t.Fatalf("child environment: %q", out)
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
