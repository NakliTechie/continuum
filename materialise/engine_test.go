package materialise

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NakliTechie/menagerie/relay-go/fleet"
	"github.com/NakliTechie/menagerie/relay-go/workspace"
)

func testRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"commit", "-q", "--allow-empty", "-m", "root"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// fullSpec exercises every stage, so the order assertion is meaningful.
func fullSpec() *fleet.Spec {
	return &fleet.Spec{
		Spec: fleet.SpecVersion, Name: "t", Repo: ".", Topology: "supervisor",
		Workspace: fleet.Workspace{
			Isolation: "worktree", BranchPrefix: "agent/",
			Materialise: fleet.Materialise{
				Ports: []fleet.Port{{Name: "PORT", Range: [2]int{4900, 4909}}},
				Files: []fleet.File{
					{From: "seed.env", To: ".env.local"},
					{Template: "app.tmpl", To: ".env", Vars: []string{"PORT", "WORKSPACE"}},
				},
				Commands: []fleet.Command{
					{Run: "npm ci", CacheKey: "package-lock.json"},
					{Run: "npm run db:seed"},
				},
				Services: []fleet.Service{{Name: "postgres", Run: "docker compose up -d db"}},
				Health:   []fleet.Probe{{Probe: "http", URL: "http://localhost:${PORT}/healthz", TimeoutS: 5}},
				Escape:   "./bootstrap.sh",
			},
		},
		Roster: []fleet.Role{{Role: "worker", Agent: "codex", Count: 1}},
	}
}

func engineWithFakes(t *testing.T, repo string) (*Engine, *RecordingExecutor, *RecordingFS) {
	t.Helper()
	home := t.TempDir()
	ex := &RecordingExecutor{Fail: map[string]bool{}}
	fsys := NewRecordingFS(map[string][]byte{
		filepath.Join(repo, "seed.env"):          []byte("SEEDED=1\n"),
		filepath.Join(repo, "app.tmpl"):          []byte("PORT=${PORT}\nWS=${WORKSPACE}\n"),
		filepath.Join(repo, "package-lock.json"): []byte(`{"lockfileVersion":3}`),
	})
	e := New(workspace.New(home))
	e.Exec, e.FS, e.Prob = ex, fsys, PassProber{}
	return e, ex, fsys
}

// The order is the contract: a file that needs a port must be able to read it,
// and a command that needs a file must run after it.
func TestStagesRunInTheFixedOrder(t *testing.T) {
	repo := testRepo(t)
	e, _, _ := engineWithFakes(t, repo)
	res, err := e.Run(fullSpec(), repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, s := range res.Steps {
		if len(order) == 0 || order[len(order)-1] != s.Stage {
			order = append(order, s.Stage)
		}
	}
	want := []string{"ports", "files", "commands", "services", "health", "escape"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("stage order = %v, want %v", order, want)
	}
}

func TestTemplateIsRenderedAndCopyIsNot(t *testing.T) {
	repo := testRepo(t)
	e, _, fsys := engineWithFakes(t, repo)
	res, err := e.Run(fullSpec(), repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	port := res.Workspace.Ports["PORT"]
	var rendered, copied string
	for p, b := range fsys.Writes {
		switch {
		case strings.HasSuffix(p, "/.env"):
			rendered = string(b)
		case strings.HasSuffix(p, "/.env.local"):
			copied = string(b)
		}
	}
	if !strings.Contains(rendered, "PORT="+strconv.Itoa(port)) || !strings.Contains(rendered, "WS=w1") {
		t.Errorf("template not interpolated: %q", rendered)
	}
	if copied != "SEEDED=1\n" {
		t.Errorf("copy was altered: %q", copied)
	}
}

// D3, the load-bearing property: three materialisations converge on identical
// state, and the service starts exactly once.
func TestThreeRunsConvergeWithNoDuplicateServices(t *testing.T) {
	repo := testRepo(t)
	e, ex, _ := engineWithFakes(t, repo)
	var ports []int
	var states []string
	for i := 0; i < 3; i++ {
		res, err := e.Run(fullSpec(), repo, "w1")
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		ports = append(ports, res.Workspace.Ports["PORT"])
		states = append(states, res.State)
	}
	if ports[0] != ports[1] || ports[1] != ports[2] {
		t.Errorf("ports drifted across runs: %v", ports)
	}
	for i, s := range states {
		if s != workspace.StateReady {
			t.Errorf("run %d state = %q, want ready", i, s)
		}
	}
	if n := ex.Count("docker compose up -d db"); n != 1 {
		t.Errorf("service started %d times, want exactly 1", n)
	}
	if n := ex.Count("npm ci"); n != 1 {
		t.Errorf("cache_key command ran %d times, want exactly 1", n)
	}
	if n := ex.Count("npm run db:seed"); n != 3 {
		t.Errorf("uncached command ran %d times, want 3 (it has no cache_key)", n)
	}
}

// The cache is keyed by repo and command, never by workspace — that is what
// makes workspaces 2..N cheap.
func TestCacheKeySkipsAcrossWorkspacesAndReRunsWhenTheKeyChanges(t *testing.T) {
	repo := testRepo(t)
	e, ex, fsys := engineWithFakes(t, repo)
	if _, err := e.Run(fullSpec(), repo, "w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(fullSpec(), repo, "w2"); err != nil {
		t.Fatal(err)
	}
	if n := ex.Count("npm ci"); n != 1 {
		t.Errorf("npm ci ran %d times across two workspaces, want 1", n)
	}
	fsys.Seed[filepath.Join(repo, "package-lock.json")] = []byte(`{"lockfileVersion":4}`)
	if _, err := e.Run(fullSpec(), repo, "w3"); err != nil {
		t.Fatal(err)
	}
	if n := ex.Count("npm ci"); n != 2 {
		t.Errorf("npm ci ran %d times after the lockfile changed, want 2", n)
	}
}

// A per-workspace port makes a service per-workspace; without one it is per repo.
func TestServiceScopeFollowsPortVar(t *testing.T) {
	repo := testRepo(t)
	e, ex, _ := engineWithFakes(t, repo)
	spec := fullSpec()
	spec.Workspace.Materialise.Services[0].PortVar = "PORT"
	for _, name := range []string{"w1", "w2"} {
		if _, err := e.Run(spec, repo, name); err != nil {
			t.Fatal(err)
		}
	}
	if n := ex.Count("docker compose up -d db"); n != 2 {
		t.Errorf("port_var service started %d times across 2 workspaces, want 2", n)
	}
}

func TestFailingProbeLeavesTheWorkspaceUnhealthyNotReady(t *testing.T) {
	repo := testRepo(t)
	e, _, _ := engineWithFakes(t, repo)
	e.Prob = FailProber{}
	res, err := e.Run(fullSpec(), repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if res.State != workspace.StateUnhealthy {
		t.Fatalf("state = %q, want unhealthy", res.State)
	}
	if len(res.Failed) != 1 {
		t.Errorf("failed probes = %v, want 1", res.Failed)
	}
	rec, _ := e.Prov.Load("w1")
	if rec.State != workspace.StateUnhealthy {
		t.Errorf("recorded state = %q, want unhealthy", rec.State)
	}
}

// Enforcement that does not depend on having been validated first.
func TestFileDestinationOutsideTheWorkspaceIsRefused(t *testing.T) {
	repo := testRepo(t)
	e, _, _ := engineWithFakes(t, repo)
	spec := fullSpec()
	spec.Workspace.Materialise.Files = []fleet.File{{From: "seed.env", To: "../escaped.env"}}
	if _, err := e.Run(spec, repo, "w1"); err == nil {
		t.Fatal("a destination outside the workspace root was accepted")
	}
}

// The checkpoint's timing bound. The fixture is synthetic — a sleep stands in
// for a real install — so this asserts the shape (warm is cheap because the
// cache_key hits), not a real-world install time.
func TestColdUnder90sAndWarmUnder10s(t *testing.T) {
	repo := testRepo(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "package-lock.json"), []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := &fleet.Spec{
		Spec: fleet.SpecVersion, Name: "t", Repo: ".", Topology: "flat",
		Workspace: fleet.Workspace{Isolation: "worktree", Materialise: fleet.Materialise{
			Commands: []fleet.Command{{Run: "sleep 1", CacheKey: "package-lock.json"}}}},
		Roster: []fleet.Role{{Role: "worker", Agent: "codex", Count: 1}},
	}
	e := New(workspace.New(home))

	start := time.Now()
	if _, err := e.Run(spec, repo, "cold"); err != nil {
		t.Fatal(err)
	}
	cold := time.Since(start)
	if cold > 90*time.Second {
		t.Errorf("cold materialise took %s, want under 90s", cold)
	}

	start = time.Now()
	if _, err := e.Run(spec, repo, "warm"); err != nil {
		t.Fatal(err)
	}
	warm := time.Since(start)
	if warm > 10*time.Second {
		t.Errorf("warm materialise took %s, want under 10s", warm)
	}
	if warm >= cold {
		t.Errorf("warm (%s) was not faster than cold (%s) — the cache_key did not hit", warm, cold)
	}
}
