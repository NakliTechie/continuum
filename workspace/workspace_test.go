package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NakliTechie/menagerie/relay-go/fleet"
)

// testRepo makes a real git repository with one commit — `git worktree add`
// refuses to work against anything less, and stubbing git would test the stub.
func testRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
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

func specWithPorts(low, high int) *fleet.Spec {
	return &fleet.Spec{
		Spec: fleet.SpecVersion, Name: "test", Repo: ".", Topology: "flat",
		Workspace: fleet.Workspace{
			Isolation: "worktree", BranchPrefix: "agent/",
			Materialise: fleet.Materialise{Ports: []fleet.Port{{Name: "PORT", Range: [2]int{low, high}}}},
		},
		Roster: []fleet.Role{{Role: "worker", Agent: "codex", Count: 1}},
	}
}

func TestProvisionCreatesWorktreePortsAndRecord(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	p := New(home)
	rec, err := p.Provision(specWithPorts(4400, 4409), repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Branch != "agent/w1" {
		t.Errorf("branch = %q, want agent/w1", rec.Branch)
	}
	if _, err := os.Stat(filepath.Join(rec.Path, ".git")); err != nil {
		t.Errorf("worktree not created at %s: %v", rec.Path, err)
	}
	port := rec.Ports["PORT"]
	if port < 4400 || port > 4409 {
		t.Errorf("PORT = %d, want it inside [4400, 4409]", port)
	}
	if rec.Vars["PORT"] != fmt.Sprint(port) {
		t.Errorf("vars[PORT] = %q, want %d", rec.Vars["PORT"], port)
	}
	for _, k := range []string{"WORKSPACE", "BRANCH", "REPO_ROOT"} {
		if rec.Vars[k] == "" {
			t.Errorf("builtin var %s is empty", k)
		}
	}
	// The variable set is closed: nothing inherits from the relay's environment.
	if len(rec.Vars) != 4 {
		t.Errorf("vars = %v, want exactly PORT + 3 builtins", rec.Vars)
	}
	back, err := p.Load("w1")
	if err != nil || back == nil || back.Ports["PORT"] != port {
		t.Fatalf("record did not round-trip: %v %v", back, err)
	}
}

// D3: re-provisioning a live workspace converges — same ports, same worktree,
// no duplicate record.
func TestProvisionIsIdempotent(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	p := New(home)
	first, err := p.Provision(specWithPorts(4410, 4419), repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Provision(specWithPorts(4410, 4419), repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Ports["PORT"] != second.Ports["PORT"] {
		t.Errorf("port changed on re-provision: %d then %d", first.Ports["PORT"], second.Ports["PORT"])
	}
	rs, err := loadRecords(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Workspaces) != 1 {
		t.Errorf("records = %d, want 1", len(rs.Workspaces))
	}
}

// The load-bearing one: 8 workspaces materialising at once against a 10-port
// range must produce zero collisions and zero duplicate allocations.
func TestConcurrentProvisionNeverCollides(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	p := New(home)
	const n = 8
	var wg sync.WaitGroup
	recs := make([]*Record, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recs[i], errs[i] = p.Provision(specWithPorts(4500, 4509), repo, fmt.Sprintf("w%d", i))
		}(i)
	}
	wg.Wait()

	seen := map[int]string{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("w%d: %v", i, errs[i])
		}
		port := recs[i].Ports["PORT"]
		if port < 4500 || port > 4509 {
			t.Errorf("w%d: port %d outside the declared range", i, port)
		}
		if other, dup := seen[port]; dup {
			t.Errorf("collision: w%d and %s both got %d", i, other, port)
		}
		seen[port] = fmt.Sprintf("w%d", i)
	}
	if len(seen) != n {
		t.Errorf("distinct ports = %d, want %d", len(seen), n)
	}
	rs, err := loadRecords(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Workspaces) != n {
		t.Errorf("records = %d, want %d", len(rs.Workspaces), n)
	}
}

// A port already bound by something outside our record is skipped, not handed
// out — the allocator bind-tests rather than trusting its own table.
func TestAllocationSkipsPortsHeldOutsideTheRecord(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	ln, err := listenOn(4600)
	if err != nil {
		t.Skip("port 4600 unavailable on this box")
	}
	defer ln.Close()
	rec, err := New(home).Provision(specWithPorts(4600, 4601), repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Ports["PORT"] == 4600 {
		t.Error("allocator handed out a port that was already bound")
	}
}

func TestProvisionRefusesAnInvalidSpec(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	bad := specWithPorts(4700, 4709)
	bad.Topology = ""
	if _, err := New(home).Provision(bad, repo, "w1"); err == nil {
		t.Fatal("an invalid spec was provisioned; the ingress must refuse it (D6)")
	}
}

func TestSetStateRecordsUnhealthySeparatelyFromReady(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	p := New(home)
	if _, err := p.Provision(specWithPorts(4800, 4809), repo, "w1"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetState("w1", StateUnhealthy); err != nil {
		t.Fatal(err)
	}
	rec, _ := p.Load("w1")
	if rec.State != StateUnhealthy {
		t.Errorf("state = %q, want %q", rec.State, StateUnhealthy)
	}
}

// The workspace name reaches a filesystem path and a git branch name, so it is
// refused rather than escaped.
func TestProvisionRefusesUnsafeWorkspaceNames(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	p := New(home)
	for _, name := range []string{
		"", "   ", "../escape", "a/b", "..", ".hidden", "-rf",
		"with space", "tab\tname", "null\x00byte", strings.Repeat("x", 65),
	} {
		if _, err := p.Provision(specWithPorts(4900, 4909), repo, name); err == nil {
			t.Errorf("name %q was accepted; it reaches a path and a branch name", name)
		}
	}
	for _, name := range []string{"w1", "agent-3", "feature.x", "A_1"} {
		if err := ValidName(name); err != nil {
			t.Errorf("name %q should be allowed: %v", name, err)
		}
	}
}
