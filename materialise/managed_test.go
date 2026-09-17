package materialise

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/fleet"
	"github.com/NakliTechie/continuum/workspace"
)

func writeManagedSpec(t *testing.T, repo string, spec *fleet.Spec) string {
	t.Helper()
	path := filepath.Join(repo, "fleet.json")
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func smallManagedSpec() *fleet.Spec {
	return &fleet.Spec{Spec: fleet.SpecVersion, Name: "managed", Repo: ".", Topology: "one", Workspace: fleet.Workspace{Isolation: "worktree", Materialise: fleet.Materialise{Hooks: &fleet.Hooks{OnStart: "start-hook", OnStop: "stop-hook", OnDestroy: "destroy-hook"}}, Teardown: &fleet.Teardown{Commands: []string{"cleanup-hook"}}}, Roster: []fleet.Role{{Role: "worker", Agent: "codex", Count: 1}}}
}

func TestManagedTrustLifecycleAndUncertainHook(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	path := writeManagedSpec(t, repo, smallManagedSpec())
	e := New(workspace.New(home))
	ex := &RecordingExecutor{Fail: map[string]bool{}}
	e.Exec = ex
	_, _, hash, err := ManagedSpec(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunManaged(path, "w1", "wrong"); err == nil {
		t.Fatal("untrusted shell accepted")
	}
	if r, _ := e.Prov.Load("w1"); r != nil {
		t.Fatal("untrusted spec provisioned workspace")
	}
	res, err := e.RunManaged(path, "w1", hash)
	if err != nil || res.State != workspace.StateReady {
		t.Fatal(res, err)
	}
	if ex.Count("start-hook") != 1 {
		t.Fatal("on_start did not run once")
	}
	ex.Fail["stop-hook"] = true
	if err := e.StopManaged("w1", false); err == nil {
		t.Fatal("failing stop hook reported success")
	}
	ex.Fail["stop-hook"] = false
	if err := e.StopManaged("w1", false); err == nil {
		t.Fatal("uncertain hook replayed implicitly")
	}
	if err := e.StopManaged("w1", true); err != nil {
		t.Fatal(err)
	}
	if err := e.DestroyManaged("w1", false); err != nil {
		t.Fatal(err)
	}
	if ex.Count("cleanup-hook") != 1 || ex.Count("destroy-hook") != 1 {
		t.Fatal("teardown/hook did not run exactly once", ex.Runs)
	}
	if rec, _ := e.Prov.Load("w1"); rec != nil {
		t.Fatal("destroy kept workspace record")
	}
}

func TestManagedHashChangeSuspendsAndRestartBudgetPersists(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	spec := smallManagedSpec()
	spec.Workspace.Materialise.Hooks = &fleet.Hooks{OnStop: "stop-service"}
	spec.Workspace.Teardown = nil
	spec.Workspace.Materialise.Ports = []fleet.Port{{Name: "PORT", Range: [2]int{6300, 6310}}}
	spec.Workspace.Materialise.Services = []fleet.Service{{Name: "svc", Run: "start-service", PortVar: "PORT", Supervise: true}}
	path := writeManagedSpec(t, repo, spec)
	_, _, hash, err := ManagedSpec(path)
	if err != nil {
		t.Fatal(err)
	}
	e := New(workspace.New(home))
	ex := &RecordingExecutor{Fail: map[string]bool{}}
	e.Exec, e.Settle = ex, -1
	e.Dial = func(string, time.Duration) error { return os.ErrNotExist }
	res, err := e.RunManaged(path, "w1", hash)
	if err != nil || res.State != workspace.StateUnhealthy {
		t.Fatal(res, err)
	}
	for count := 1; count <= 3; count++ {
		if err := e.TickManaged(); err != nil {
			t.Fatal(err)
		}
		r, _ := e.Prov.Load("w1")
		if r.RestartCount["svc"] != count {
			t.Fatal("restart budget not persisted", r.RestartCount)
		}
		if count < 3 {
			if err := e.Prov.SetRestart("w1", "svc", count, time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	starts := ex.Count("start-service")
	if err := e.TickManaged(); err != nil {
		t.Fatal(err)
	}
	if ex.Count("start-service") != starts {
		t.Fatal("exhausted restart budget ran shell")
	}
	spec.Topology = "changed"
	writeManagedSpec(t, repo, spec)
	if err := e.TickManaged(); err != nil {
		t.Fatal(err)
	}
	r, _ := e.Prov.Load("w1")
	if r.ManagedArmed || r.State != workspace.StateUnhealthy {
		t.Fatal("changed spec remained armed", r)
	}
}

func TestChangedManagedSpecCannotRewriteExistingPortsOrVars(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	spec := smallManagedSpec()
	spec.Workspace.Materialise.Hooks = nil
	spec.Workspace.Teardown = nil
	spec.Workspace.Materialise.Ports = []fleet.Port{{Name: "PORT", Range: [2]int{6400, 6410}}}
	path := writeManagedSpec(t, repo, spec)
	_, _, firstHash, err := ManagedSpec(path)
	if err != nil {
		t.Fatal(err)
	}
	e := New(workspace.New(home))
	e.Exec = &RecordingExecutor{Fail: map[string]bool{}}
	if _, err := e.RunManaged(path, "w1", firstHash); err != nil {
		t.Fatal(err)
	}
	before, err := e.Prov.Load("w1")
	if err != nil || before == nil {
		t.Fatal(err)
	}
	spec.Workspace.Materialise.Ports = []fleet.Port{{Name: "OTHER", Range: [2]int{6500, 6510}}}
	writeManagedSpec(t, repo, spec)
	_, _, secondHash, err := ManagedSpec(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunManaged(path, "w1", secondHash); err == nil {
		t.Fatal("changed managed spec replaced an existing record")
	}
	if _, err := e.Prov.Provision(spec, repo, "w1"); err == nil {
		t.Fatal("one-shot provision rewrote a managed record")
	}
	after, err := e.Prov.Load("w1")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected spec changed managed record: before=%+v after=%+v err=%v", before, after, err)
	}
}

func TestStopFailedMaterialisationRunsHookAndRefusesLiveService(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	spec := smallManagedSpec()
	spec.Workspace.Materialise.Hooks = &fleet.Hooks{OnStop: "stop-service"}
	spec.Workspace.Materialise.Ports = []fleet.Port{{Name: "PORT", Range: [2]int{6500, 6510}}}
	spec.Workspace.Materialise.Services = []fleet.Service{{Name: "svc", Run: "start-service", PortVar: "PORT", Supervise: true}}
	spec.Workspace.Materialise.Health = []fleet.Probe{{Probe: "command", Run: "bad-health"}}
	path := writeManagedSpec(t, repo, spec)
	_, _, hash, err := ManagedSpec(path)
	if err != nil {
		t.Fatal(err)
	}
	e := New(workspace.New(home))
	ex := &RecordingExecutor{Fail: map[string]bool{"bad-health": true}}
	e.Exec = ex
	e.Dial = func(string, time.Duration) error { return nil }
	if res, err := e.RunManaged(path, "w1", hash); err != nil || res.State != workspace.StateUnhealthy {
		t.Fatal("failing health passed", res, err)
	}
	if rec, _ := e.Prov.Load("w1"); rec.StartedAt != "" {
		t.Fatal("failed materialisation unexpectedly stamped on_start")
	}
	if err := e.StopManaged("w1", false); err == nil {
		t.Fatal("live supervised service allowed stop")
	}
	if ex.Count("stop-service") != 1 {
		t.Fatal("on_stop was skipped after failed materialisation")
	}
	if rec, _ := e.Prov.Load("w1"); rec.State == workspace.StateStopped {
		t.Fatal("live service marked stopped")
	}
	e.Dial = func(string, time.Duration) error { return os.ErrNotExist }
	if err := e.StopManaged("w1", true); err != nil {
		t.Fatal(err)
	}
}

func TestManagedPlanAndRunBothRequireSupervisedStopHook(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	spec := smallManagedSpec()
	spec.Workspace.Materialise.Hooks = nil
	spec.Workspace.Materialise.Ports = []fleet.Port{{Name: "PORT", Range: [2]int{6500, 6510}}}
	spec.Workspace.Materialise.Services = []fleet.Service{{Name: "svc", Run: "start-service", PortVar: "PORT", Supervise: true}}
	path := writeManagedSpec(t, repo, spec)
	e := New(workspace.New(home))
	_, _, hash, err := ManagedSpec(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.PlanManaged(path, "w1"); err == nil {
		t.Fatal("plan accepted a managed service without cleanup")
	}
	if _, err := e.RunManaged(path, "w1", hash); err == nil {
		t.Fatal("run accepted a managed service without cleanup")
	}
	if rec, err := e.Prov.Load("w1"); err != nil || rec != nil {
		t.Fatal("rejected service spec changed state", rec, err)
	}
}
