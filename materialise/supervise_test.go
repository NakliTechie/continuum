package materialise

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NakliTechie/menagerie/relay-go/fleet"
	"github.com/NakliTechie/menagerie/relay-go/workspace"
)

// supervisedSpec has one service supervised on its own allocated port.
func supervisedSpec() *fleet.Spec {
	return &fleet.Spec{
		Spec: fleet.SpecVersion, Name: "t", Repo: ".", Topology: "flat",
		Workspace: fleet.Workspace{Isolation: "worktree", Materialise: fleet.Materialise{
			Ports:    []fleet.Port{{Name: "DB_PORT", Range: [2]int{5700, 5709}}},
			Services: []fleet.Service{{Name: "db", Run: "start-db", PortVar: "DB_PORT", Supervise: true}},
		}},
		Roster: []fleet.Role{{Role: "worker", Agent: "codex", Count: 1}},
	}
}

// The hole this closes: a probe is a moment, not a guarantee. A service that
// dies after a green probe used to leave the workspace reading `ready`.
func TestASupervisedServiceThatDiesFlipsTheWorkspaceToUnhealthy(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	e := New(workspace.New(home))
	e.Exec, e.Settle = &RecordingExecutor{Fail: map[string]bool{}}, -1

	// A real listener stands in for the running service, so supervision is
	// dialling something that genuinely exists rather than a stub saying yes.
	var alive net.Listener
	e.Dial = func(addr string, timeout time.Duration) error {
		c, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return err
		}
		return c.Close()
	}

	spec := supervisedSpec()
	// Materialise once to learn which port was allocated, then bind it and
	// re-materialise so the service is live for the run that must go ready.
	first, err := e.Run(spec, repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	port := first.Workspace.Ports["DB_PORT"]
	if first.State != workspace.StateUnhealthy {
		t.Fatalf("with nothing listening the workspace should be unhealthy, got %q", first.State)
	}

	alive, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("could not bind the allocated port %d: %v", port, err)
	}
	second, err := e.Run(spec, repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if second.State != workspace.StateReady {
		t.Fatalf("with the service listening the workspace should be ready, got %q (failed: %v)", second.State, second.Failed)
	}

	// The service dies. Nothing else changes.
	_ = alive.Close()
	state, err := e.Supervise(spec, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if state != workspace.StateUnhealthy {
		t.Fatalf("after the service died Supervise returned %q, want unhealthy", state)
	}
	rec, _ := e.Prov.Load("w1")
	if rec.State != workspace.StateUnhealthy {
		t.Errorf("recorded state = %q, want unhealthy — a dead service must not read as ready", rec.State)
	}
}

// on_start runs after the whole declared sequence, and only when it succeeded.
func TestOnStartRunsAfterASuccessfulMaterialiseAndNotAfterAFailedOne(t *testing.T) {
	repo := testRepo(t)

	// Success: no supervised service, a passing probe, so on_start fires.
	e := New(workspace.New(t.TempDir()))
	ex := &RecordingExecutor{Fail: map[string]bool{}}
	e.Exec, e.Prob = ex, PassProber{}
	spec := &fleet.Spec{
		Spec: fleet.SpecVersion, Name: "t", Repo: ".", Topology: "flat",
		Workspace: fleet.Workspace{Isolation: "worktree", Materialise: fleet.Materialise{
			Commands: []fleet.Command{{Run: "setup"}},
			Health:   []fleet.Probe{{Probe: "http", URL: "http://localhost/healthz", TimeoutS: 1}},
			Hooks:    &fleet.Hooks{OnStart: "announce"},
		}},
		Roster: []fleet.Role{{Role: "worker", Agent: "codex", Count: 1}},
	}
	res, err := e.Run(spec, repo, "ok")
	if err != nil {
		t.Fatal(err)
	}
	if res.State != workspace.StateReady {
		t.Fatalf("state = %q, want ready", res.State)
	}
	if ex.Count("announce") != 1 {
		t.Errorf("on_start ran %d times after a successful materialise, want 1", ex.Count("announce"))
	}
	// Order matters: the hook is a lifecycle event after the sequence, so it is last.
	if last := res.Steps[len(res.Steps)-1]; last.Stage != "hooks" || last.Action != "on_start" {
		t.Errorf("last step = %s/%s, want hooks/on_start", last.Stage, last.Action)
	}

	// Failure: the probe fails, so on_start must not fire.
	e2 := New(workspace.New(t.TempDir()))
	ex2 := &RecordingExecutor{Fail: map[string]bool{}}
	e2.Exec, e2.Prob = ex2, FailProber{}
	res2, err := e2.Run(spec, repo, "bad")
	if err != nil {
		t.Fatal(err)
	}
	if res2.State != workspace.StateUnhealthy {
		t.Fatalf("state = %q, want unhealthy", res2.State)
	}
	if n := ex2.Count("announce"); n != 0 {
		t.Errorf("on_start ran %d times after a failed materialise, want 0", n)
	}
}

// Regression, found by an independent verifier during the 2026-09-09 run: the
// service cache remembered "started" and never re-checked, so a supervised
// service that died left the workspace permanently unhealthy — re-materialising
// skipped the restart as "already running" and could never recover it.
func TestReMaterialiseRestartsADeadSupervisedService(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	e := New(workspace.New(home))
	ex := &RecordingExecutor{Fail: map[string]bool{}}
	e.Exec, e.Dial, e.Settle = ex, TCPDial, -1
	spec := supervisedSpec()
	spec.Workspace.Materialise.Ports[0].Range = [2]int{5810, 5819}

	first, err := e.Run(spec, repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	port := first.Workspace.Ports["DB_PORT"]

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("could not bind the allocated port %d: %v", port, err)
	}
	if second, err := e.Run(spec, repo, "w1"); err != nil || second.State != workspace.StateReady {
		t.Fatalf("with the service listening: state=%v err=%v", second.State, err)
	}
	startsWhileHealthy := ex.Count("start-db")

	// The service dies, and the workspace is re-materialised to recover it.
	_ = ln.Close()
	third, err := e.Run(spec, repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if ex.Count("start-db") != startsWhileHealthy+1 {
		t.Errorf("start-db ran %d times, want %d — a dead supervised service must be restarted, not skipped as \"already running\"",
			ex.Count("start-db"), startsWhileHealthy+1)
	}
	var restarted bool
	for _, s := range third.Steps {
		if s.Stage == "services" && !s.Skipped {
			restarted = true
		}
	}
	if !restarted {
		t.Error("no service start step ran on the recovery pass")
	}
}

// Regression, same verifier: on_start is a lifecycle transition, not a
// per-call side effect. Three convergence passes over one workspace fired it
// three times, so a hook that notifies, seeds or registers would repeat.
func TestOnStartFiresOnceAcrossRepeatedMaterialisations(t *testing.T) {
	repo := testRepo(t)
	e := New(workspace.New(t.TempDir()))
	ex := &RecordingExecutor{Fail: map[string]bool{}}
	e.Exec, e.Prob = ex, PassProber{}
	spec := &fleet.Spec{
		Spec: fleet.SpecVersion, Name: "t", Repo: ".", Topology: "flat",
		Workspace: fleet.Workspace{Isolation: "worktree", Materialise: fleet.Materialise{
			Commands: []fleet.Command{{Run: "setup"}},
			Hooks:    &fleet.Hooks{OnStart: "announce"},
		}},
		Roster: []fleet.Role{{Role: "worker", Agent: "codex", Count: 1}},
	}
	for i := 0; i < 3; i++ {
		if _, err := e.Run(spec, repo, "w"); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if n := ex.Count("announce"); n != 1 {
		t.Errorf("on_start ran %d times across 3 materialisations of one workspace, want 1", n)
	}
}

// Regression: a failing on_start used to return early, stranding the record at
// `materialising` — neither ready nor honestly unhealthy, and nothing would ever
// move it again.
func TestAFailingOnStartLeavesTheWorkspaceUnhealthyNotStranded(t *testing.T) {
	repo := testRepo(t)
	e := New(workspace.New(t.TempDir()))
	ex := &RecordingExecutor{Fail: map[string]bool{"announce": true}}
	e.Exec, e.Prob = ex, PassProber{}
	spec := &fleet.Spec{
		Spec: fleet.SpecVersion, Name: "t", Repo: ".", Topology: "flat",
		Workspace: fleet.Workspace{Isolation: "worktree", Materialise: fleet.Materialise{
			Commands: []fleet.Command{{Run: "setup"}},
			Hooks:    &fleet.Hooks{OnStart: "announce"},
		}},
		Roster: []fleet.Role{{Role: "worker", Agent: "codex", Count: 1}},
	}
	res, err := e.Run(spec, repo, "w")
	if err != nil {
		t.Fatalf("a failing hook should be reported as an unhealthy workspace, not a hard error: %v", err)
	}
	if res.State != workspace.StateUnhealthy {
		t.Errorf("state = %q, want unhealthy", res.State)
	}
	rec, _ := e.Prov.Load("w")
	if rec.State != workspace.StateUnhealthy {
		t.Errorf("recorded state = %q, want unhealthy — never left at materialising", rec.State)
	}
}

// Regression: once supervision marked a workspace unhealthy, it could never read
// ready again even after the service came back, so a single blip condemned it.
func TestSupervisionRestoresReadyWhenTheServiceComesBack(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	e := New(workspace.New(home))
	e.Exec, e.Dial, e.Settle = &RecordingExecutor{Fail: map[string]bool{}}, TCPDial, -1
	spec := supervisedSpec()
	spec.Workspace.Materialise.Ports[0].Range = [2]int{5820, 5829}

	first, err := e.Run(spec, repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	port := first.Workspace.Ports["DB_PORT"]
	if first.State != workspace.StateUnhealthy {
		t.Fatalf("nothing is listening, so the workspace should be unhealthy; got %q", first.State)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("could not bind %d: %v", port, err)
	}
	defer ln.Close()

	state, err := e.Supervise(spec, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if state != workspace.StateReady {
		t.Errorf("Supervise returned %q with the service answering again, want ready", state)
	}
}

// Regression: an unhealthy set by a failed HEALTH PROBE is not supervision's to
// clear — supervision knows nothing about whether that condition recovered.
func TestSupervisionDoesNotClearAnUnhealthyItDidNotCause(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	e := New(workspace.New(home))
	e.Exec, e.Prob, e.Dial, e.Settle = &RecordingExecutor{Fail: map[string]bool{}}, FailProber{}, TCPDial, -1
	spec := supervisedSpec()
	spec.Workspace.Materialise.Ports[0].Range = [2]int{5830, 5839}
	spec.Workspace.Materialise.Health = []fleet.Probe{{Probe: "http", URL: "http://localhost/healthz", TimeoutS: 1}}

	first, err := e.Run(spec, repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", first.Workspace.Ports["DB_PORT"]))
	if err != nil {
		t.Skipf("could not bind: %v", err)
	}
	defer ln.Close()

	state, err := e.Supervise(spec, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if state == workspace.StateReady {
		t.Error("supervision cleared an unhealthy caused by a failed health probe, which it cannot know has recovered")
	}
}

// D6: every path in validates. Without it, an invalid spec marks a HEALTHY
// workspace unhealthy on the strength of a document the validator rejects — so
// the workspace has to be genuinely ready before the invalid call is made.
func TestSuperviseRefusesAnInvalidSpec(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	e := New(workspace.New(home))
	e.Exec, e.Dial, e.Settle = &RecordingExecutor{Fail: map[string]bool{}}, TCPDial, -1
	spec := supervisedSpec()
	spec.Workspace.Materialise.Ports[0].Range = [2]int{5840, 5849}

	first, err := e.Run(spec, repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", first.Workspace.Ports["DB_PORT"]))
	if err != nil {
		t.Skipf("could not bind: %v", err)
	}
	defer ln.Close()
	second, err := e.Run(spec, repo, "w1")
	if err != nil || second.State != workspace.StateReady {
		t.Fatalf("setup: state=%v err=%v, want a ready workspace", second.State, err)
	}

	bad := supervisedSpec()
	bad.Workspace.Materialise.Services[0].PortVar = "" // supervise with nothing to check
	if _, err := e.Supervise(bad, "w1"); err == nil {
		t.Fatal("Supervise accepted a spec Validate rejects")
	}
	rec, _ := e.Prov.Load("w1")
	if rec.State != workspace.StateReady {
		t.Errorf("state = %q after an invalid Supervise call, want it untouched at ready", rec.State)
	}
}

// Regression for the blocker an independent verifier found on 2026-09-09, caused
// by two fixes interacting: on_start was gated on the workspace's prior STATE,
// and Supervise() promotes a workspace to ready without running the hook. A
// workspace recovered by supervision therefore skipped on_start forever after.
func TestOnStartStillRunsAfterSupervisionPromotedTheWorkspace(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	e := New(workspace.New(home))
	ex := &RecordingExecutor{Fail: map[string]bool{}}
	e.Exec, e.Dial, e.Settle = ex, TCPDial, -1
	spec := supervisedSpec()
	spec.Workspace.Materialise.Ports[0].Range = [2]int{5950, 5959}
	spec.Workspace.Materialise.Hooks = &fleet.Hooks{OnStart: "announce"}

	// Nothing is listening yet, so the first pass is unhealthy and the hook must
	// not have run.
	first, err := e.Run(spec, repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if first.State != workspace.StateUnhealthy || ex.Count("announce") != 0 {
		t.Fatalf("setup: state=%q announce=%d, want unhealthy and 0", first.State, ex.Count("announce"))
	}

	// The service comes up and supervision promotes the workspace to ready.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", first.Workspace.Ports["DB_PORT"]))
	if err != nil {
		t.Skipf("could not bind: %v", err)
	}
	defer ln.Close()
	if state, err := e.Supervise(spec, "w1"); err != nil || state != workspace.StateReady {
		t.Fatalf("Supervise: state=%q err=%v, want ready", state, err)
	}
	if ex.Count("announce") != 0 {
		t.Fatalf("Supervise ran the start hook itself (%d times); it must not", ex.Count("announce"))
	}

	// The next materialise must still run on_start — this workspace has never
	// been started, whatever its state says.
	second, err := e.Run(spec, repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if ex.Count("announce") != 1 {
		var hook Step
		for _, s := range second.Steps {
			if s.Stage == "hooks" {
				hook = s
			}
		}
		t.Fatalf("on_start ran %d times after a supervision promotion, want 1 (hook step: skipped=%v reason=%q)",
			ex.Count("announce"), hook.Skipped, hook.Reason)
	}

	// And still exactly once thereafter.
	if _, err := e.Run(spec, repo, "w1"); err != nil {
		t.Fatal(err)
	}
	if n := ex.Count("announce"); n != 1 {
		t.Errorf("on_start ran %d times in total, want 1", n)
	}
}

// Regression: a service that has not finished binding when its start command
// returns was reported dead. `docker compose up -d db` returns before postgres
// accepts TCP, and connection-refused comes back instantly, so a single dial
// condemned a perfectly healthy service. probe.go documents this rule for HTTP
// probes; supervision now follows it too.
func TestASlowBindingServiceIsNotReportedDead(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	e := New(workspace.New(home))
	e.Settle = 3 * time.Second

	var mu sync.Mutex
	var ln net.Listener
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if ln != nil {
			_ = ln.Close()
		}
	})

	spec := supervisedSpec()
	spec.Workspace.Materialise.Ports[0].Range = [2]int{5960, 5969}

	// The start command returns immediately and binds the port 600ms later,
	// exactly like a container that is still coming up.
	e.Dial = TCPDial
	e.Exec = &slowBinder{home: home, prov: e.Prov, mu: &mu, ln: &ln}

	res, err := e.Run(spec, repo, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if res.State != workspace.StateReady {
		t.Fatalf("state = %q (failed: %v), want ready — the service bound 600ms after its start command returned",
			res.State, res.Failed)
	}
}

// slowBinder starts "the service" asynchronously, binding the workspace's
// allocated port after a delay.
type slowBinder struct {
	home string
	prov *workspace.Provisioner
	mu   *sync.Mutex
	ln   *net.Listener
}

func (s *slowBinder) Run(dir string, env []string, cmdline string, timeout time.Duration) ([]byte, error) {
	if cmdline != "start-db" {
		return nil, nil
	}
	var port int
	for _, kv := range env {
		if strings.HasPrefix(kv, "DB_PORT=") {
			port, _ = strconv.Atoi(strings.TrimPrefix(kv, "DB_PORT="))
		}
	}
	go func() {
		time.Sleep(600 * time.Millisecond)
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return
		}
		s.mu.Lock()
		*s.ln = l
		s.mu.Unlock()
	}()
	return nil, nil
}
