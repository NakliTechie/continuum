package materialise

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NakliTechie/menagerie/relay-go/fleet"
	"github.com/NakliTechie/menagerie/relay-go/workspace"
)

// Finding 1: port_var was checked against everything ${VAR} can resolve — which
// includes the builtins — so `port_var: "BRANCH"` passed validation and then took
// the "no allocated port" branch forever: permanently unhealthy, on_start never
// run, start command re-run every pass.
func TestSuperviseRejectsABuiltinAsPortVar(t *testing.T) {
	spec := supervisedSpec()
	spec.Workspace.Materialise.Services[0].PortVar = "BRANCH"
	issues := fleet.Validate(spec)
	var found bool
	for _, is := range issues {
		if strings.HasSuffix(is.Path, "/port_var") && is.Code == "undeclared_var" {
			found = true
		}
	}
	if !found {
		t.Fatalf("port_var naming a builtin was accepted; issues = %v", issues)
	}
}

// Finding 3: a stage that fails must leave the record honestly unhealthy, not
// stranded at `materialising` where nothing would ever move it again.
func TestAFailingStageRecordsUnhealthyRatherThanStranding(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	e := New(workspace.New(home))
	e.Exec = &RecordingExecutor{Fail: map[string]bool{"boom": true}}
	spec := &fleet.Spec{
		Spec: fleet.SpecVersion, Name: "t", Repo: ".", Topology: "flat",
		Workspace: fleet.Workspace{Isolation: "worktree", Materialise: fleet.Materialise{
			Commands: []fleet.Command{{Run: "boom"}}}},
		Roster: []fleet.Role{{Role: "worker", Agent: "codex", Count: 1}},
	}
	if _, err := e.Run(spec, repo, "w"); err == nil {
		t.Fatal("expected the failing command to surface as an error")
	}
	rec, _ := e.Prov.Load("w")
	if rec.State != workspace.StateUnhealthy {
		t.Errorf("recorded state = %q, want unhealthy (never left at materialising)", rec.State)
	}
	if rec.Reason == "" {
		t.Error("no reason recorded for the failure")
	}
}

// Finding 4: two Supervise calls used to walk a workspace that had never been
// materialised from provisioning -> unhealthy -> ready on one TCP dial. `ready` is
// the state a workspace is handed to an agent in.
func TestSuperviseIgnoresAWorkspaceThatNeverMaterialised(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	e := New(workspace.New(home))
	e.Exec, e.Dial, e.Settle = &RecordingExecutor{Fail: map[string]bool{}}, TCPDial, -1
	spec := supervisedSpec()
	spec.Workspace.Materialise.Ports[0].Range = [2]int{5970, 5979}

	rec, err := e.Prov.Provision(spec, repo, "w1") // provisioned, never materialised
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", rec.Ports["DB_PORT"]))
	if err != nil {
		t.Skipf("could not bind: %v", err)
	}
	defer ln.Close()

	for i := 0; i < 2; i++ {
		state, err := e.Supervise(spec, "w1")
		if err != nil {
			t.Fatal(err)
		}
		if state == workspace.StateReady {
			t.Fatalf("call %d promoted a never-materialised workspace to ready", i+1)
		}
	}
	back, _ := e.Prov.Load("w1")
	if back.State == workspace.StateReady {
		t.Errorf("recorded state = ready for a workspace with no files, no commands and no probes")
	}
}

// Finding 7: the returned snapshot was taken before the run, so a
// just-materialised workspace went over the wire as `provisioning`.
func TestResultCarriesThePostRunWorkspaceState(t *testing.T) {
	repo := testRepo(t)
	e := New(workspace.New(t.TempDir()))
	e.Exec, e.Prob = &RecordingExecutor{Fail: map[string]bool{}}, PassProber{}
	spec := &fleet.Spec{
		Spec: fleet.SpecVersion, Name: "t", Repo: ".", Topology: "flat",
		Workspace: fleet.Workspace{Isolation: "worktree", Materialise: fleet.Materialise{
			Commands: []fleet.Command{{Run: "setup"}},
			Hooks:    &fleet.Hooks{OnStart: "announce"}}},
		Roster: []fleet.Role{{Role: "worker", Agent: "codex", Count: 1}},
	}
	res, err := e.Run(spec, repo, "w")
	if err != nil {
		t.Fatal(err)
	}
	if res.Workspace.State != workspace.StateReady {
		t.Errorf("result snapshot state = %q, want ready — it was a pre-run snapshot", res.Workspace.State)
	}
	if res.Workspace.StartedAt == "" {
		t.Error("result snapshot has no started_at although the hook ran and stamped the record")
	}
}

// Finding 9: escape was the one declared field neither interpolated nor
// validated, so ${PORT} in a bootstrap script stayed literal.
func TestEscapeIsInterpolatedAndItsVarsValidated(t *testing.T) {
	repo := testRepo(t)
	e := New(workspace.New(t.TempDir()))
	ex := &RecordingExecutor{Fail: map[string]bool{}}
	e.Exec, e.Prob = ex, PassProber{}
	spec := &fleet.Spec{
		Spec: fleet.SpecVersion, Name: "t", Repo: ".", Topology: "flat",
		Workspace: fleet.Workspace{Isolation: "worktree", Materialise: fleet.Materialise{
			Ports:  []fleet.Port{{Name: "PORT", Range: [2]int{5980, 5989}}},
			Escape: "./bootstrap.sh ${PORT}"}},
		Roster: []fleet.Role{{Role: "worker", Agent: "codex", Count: 1}},
	}
	res, err := e.Run(spec, repo, "w")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("./bootstrap.sh %d", res.Workspace.Ports["PORT"])
	if ex.Count(want) != 1 {
		t.Errorf("escape ran as %v, want the interpolated %q", ex.Runs, want)
	}

	bad := *spec
	bad.Workspace.Materialise.Escape = "./bootstrap.sh ${NOPE}"
	var flagged bool
	for _, is := range fleet.Validate(&bad) {
		if is.Path == "/workspace/materialise/escape" && is.Code == "undeclared_var" {
			flagged = true
		}
	}
	if !flagged {
		t.Error("an undeclared var in escape was not flagged")
	}
}

// A failed command's combined output must never reach the persisted record: the
// record is on the relay's disk and the handoff forbids secrets there. A setup
// command that echoes a credential while failing used to put it in `reason`.
func TestAFailedCommandsOutputNeverReachesTheRecord(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	e := New(workspace.New(home))
	e.Exec = &leakyExecutor{}
	spec := &fleet.Spec{
		Spec: fleet.SpecVersion, Name: "t", Repo: ".", Topology: "flat",
		Workspace: fleet.Workspace{Isolation: "worktree", Materialise: fleet.Materialise{
			Commands: []fleet.Command{{Run: "setup"}}}},
		Roster: []fleet.Role{{Role: "worker", Agent: "codex", Count: 1}},
	}
	if _, err := e.Run(spec, repo, "w"); err == nil {
		t.Fatal("expected the failing command to error")
	}
	rec, _ := e.Prov.Load("w")
	if strings.Contains(rec.Reason, "AKIA") || strings.Contains(rec.Reason, "hunter2") {
		t.Fatalf("the record's reason carries command output: %q", rec.Reason)
	}
	if rec.State != workspace.StateUnhealthy {
		t.Errorf("state = %q, want unhealthy", rec.State)
	}
	// The failure path must also return a fresh snapshot, not the pre-run one.
	raw, _ := os.ReadFile(filepath.Join(home, "workspaces.json"))
	if strings.Contains(string(raw), "AKIA") {
		t.Error("the credential reached workspaces.json on disk")
	}
}

// leakyExecutor fails while printing something credential-shaped, the way a real
// setup command echoing its environment would.
type leakyExecutor struct{}

func (leakyExecutor) Run(dir string, env []string, cmdline string, timeout time.Duration) ([]byte, error) {
	return []byte("connecting with AKIAIOSFODNN7EXAMPLE / hunter2\n"), fmt.Errorf("exit status 1")
}
