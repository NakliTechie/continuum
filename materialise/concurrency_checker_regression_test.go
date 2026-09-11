package materialise

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/fleet"
	"github.com/NakliTechie/continuum/workspace"
)

type o4CheckerExec func(string, []string, string, time.Duration) ([]byte, error)

func (f o4CheckerExec) Run(d string, e []string, c string, tm time.Duration) ([]byte, error) {
	return f(d, e, c, tm)
}
func o4CheckerEngine(home string, calls *atomic.Int32) *Engine {
	e := New(workspace.New(home))
	e.Exec = o4CheckerExec(func(string, []string, string, time.Duration) ([]byte, error) { calls.Add(1); return nil, nil })
	return e
}
func o4CheckerRecord(home, name string) *workspace.Record {
	return &workspace.Record{Name: name, Path: filepath.Join(home, name), Vars: map[string]string{}, Ports: map[string]int{"PORT": 43210}}
}

func TestO4CheckerSameScopeGoroutines(t *testing.T) {
	home := t.TempDir()
	var calls atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	steps := make(chan Step, 24)
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := o4CheckerEngine(home, &calls)
			<-start
			s, err := e.startService(fleet.Service{Name: "db", Run: "fake"}, o4CheckerRecord(home, fmt.Sprint(i)), "/repo")
			errs <- err
			steps <- s
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	close(steps)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	nonskip := 0
	for s := range steps {
		if !s.Skipped {
			nonskip++
		}
	}
	if calls.Load() != 1 || nonskip != 1 {
		t.Fatalf("effects=%d non-skipped=%d, want 1 each", calls.Load(), nonskip)
	}
}

func TestO4CheckerSeparateScopesOverlapAndPreserveCache(t *testing.T) {
	home := t.TempDir()
	var calls atomic.Int32
	seed := o4CheckerEngine(home, &calls)
	if err := seed.cachePut("/old", "old-command", "old-hash"); err != nil {
		t.Fatal(err)
	}
	if err := seed.serviceMark("old-service"); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	type tc struct{ repo, name, ws, port string }
	cases := []tc{{"/repo", "db", "a", ""}, {"/repo2", "db", "a", ""}, {"/repo", "db2", "a", ""}, {"/repo", "db", "a", "PORT"}}
	for _, c := range cases {
		wg.Add(1)
		go func(c tc) {
			defer wg.Done()
			e := New(workspace.New(home))
			e.Exec = o4CheckerExec(func(string, []string, string, time.Duration) ([]byte, error) {
				entered <- struct{}{}
				<-release
				return nil, nil
			})
			_, err := e.startService(fleet.Service{Name: c.name, Run: "fake", PortVar: c.port}, o4CheckerRecord(home, c.ws), c.repo)
			errs <- err
		}(c)
	}
	for i := 0; i < len(cases); i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			close(release)
			wg.Wait()
			t.Fatal("separate scopes failed to enter concurrently")
		}
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	cs := mustCache(t, seed)
	if len(cs.Services) != 5 || !cs.Services["old-service"] || cs.Commands["/old|old-command"] != "old-hash" {
		t.Fatalf("lost independent cache entries: %+v", cs)
	}
	for _, c := range cases {
		k := c.repo + "|" + c.name
		if c.port != "" {
			k += "|" + c.ws
		}
		if !cs.Services[k] {
			t.Errorf("missing service %s", k)
		}
	}
}

func TestO4CheckerPortVarWorkspaceAdmission(t *testing.T) {
	home := t.TempDir()
	var calls atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := o4CheckerEngine(home, &calls)
			_, err := e.startService(fleet.Service{Name: "db", Run: "fake", PortVar: "PORT"}, o4CheckerRecord(home, fmt.Sprint(i%2)), "/repo")
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("effects=%d, want one per workspace", calls.Load())
	}
}

func TestO4CheckerMixedCacheWrites(t *testing.T) {
	home := t.TempDir()
	var calls atomic.Int32
	e := o4CheckerEngine(home, &calls)
	var wg sync.WaitGroup
	errs := make(chan error, 128)
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				errs <- e.cachePut(fmt.Sprintf("/workspace/%d", i), "command", "hash")
			} else {
				errs <- e.serviceMark(fmt.Sprintf("/repo|svc%d", i))
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	cs := mustCache(t, e)
	if len(cs.Commands) != 64 || len(cs.Services) != 64 {
		t.Fatalf("cache lengths: commands=%d services=%d", len(cs.Commands), len(cs.Services))
	}
	b, err := os.ReadFile(e.cachePath())
	if err != nil {
		t.Fatal(err)
	}
	var parsed cacheSet
	if err = json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("invalid final JSON: %v", err)
	}
	fi, err := os.Stat(e.cachePath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("cache mode=%v", fi.Mode())
	}
	tmp, err := filepath.Glob(filepath.Join(home, ".materialise-cache-*"))
	if err != nil || len(tmp) != 0 {
		t.Fatalf("temporary cache leftovers=%v err=%v", tmp, err)
	}
}

func TestO4CheckerFailedServiceRetries(t *testing.T) {
	home := t.TempDir()
	var calls atomic.Int32
	e := o4CheckerEngine(home, &calls)
	e.Exec = o4CheckerExec(func(string, []string, string, time.Duration) ([]byte, error) {
		if calls.Add(1) == 1 {
			return []byte("failed output"), errors.New("injected failure")
		}
		return nil, nil
	})
	sv := fleet.Service{Name: "db", Run: "fake"}
	rec := o4CheckerRecord(home, "a")
	if _, err := e.startService(sv, rec, "/repo"); err == nil || !strings.Contains(err.Error(), "injected failure: failed output") {
		t.Fatalf("missing execution error: %v", err)
	}
	if mustCache(t, e).Services["/repo|db"] {
		t.Fatal("failed start cached")
	}
	if s, err := e.startService(sv, rec, "/repo"); err != nil || s.Skipped {
		t.Fatalf("retry: %+v %v", s, err)
	}
	if s, err := e.startService(sv, rec, "/repo"); err != nil || !s.Skipped {
		t.Fatalf("warm: %+v %v", s, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("effects=%d", calls.Load())
	}
}

func TestO4CheckerFailedCommandRetries(t *testing.T) {
	home := t.TempDir()
	var calls atomic.Int32
	e := o4CheckerEngine(home, &calls)
	e.FS = NewRecordingFS(map[string][]byte{"/repo/lock": []byte("value")})
	e.Exec = o4CheckerExec(func(string, []string, string, time.Duration) ([]byte, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("injected failure")
		}
		return nil, nil
	})
	c := fleet.Command{Run: "fake", CacheKey: "lock"}
	rec := o4CheckerRecord(home, "a")
	if _, err := e.runCommand(c, rec, "/repo"); err == nil {
		t.Fatal("failed command accepted")
	}
	if len(mustCache(t, e).Commands) != 0 {
		t.Fatal("failed command cached")
	}
	if s, err := e.runCommand(c, rec, "/repo"); err != nil || s.Skipped {
		t.Fatalf("retry: %+v %v", s, err)
	}
	if s, err := e.runCommand(c, rec, "/repo"); err != nil || !s.Skipped {
		t.Fatalf("warm: %+v %v", s, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("effects=%d", calls.Load())
	}
}

func TestO4CheckerDryRunCreatesNoHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent-home")
	var calls atomic.Int32
	e := o4CheckerEngine(home, &calls)
	e.DryRun = true
	e.Dial = func(string, time.Duration) error { t.Error("dry-run dialed"); return nil }
	sv := fleet.Service{Name: "db", Run: "fake", PortVar: "PORT", Supervise: true}
	if s, err := e.startService(sv, o4CheckerRecord(home, "a"), "/repo"); err != nil || !s.Skipped || s.Reason != "dry run" {
		t.Fatalf("dry start: %+v %v", s, err)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run created home: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("dry-run effects=%d", calls.Load())
	}
}

func TestO4CheckerLockDeadlineAndRecovery(t *testing.T) {
	e := New(workspace.New(t.TempDir()))
	unlock, err := e.lockCacheScope("scope", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	other, err := e.lockCacheScope("scope", 40*time.Millisecond)
	elapsed := time.Since(start)
	if other != nil {
		other()
	}
	unlock()
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
	if elapsed < 40*time.Millisecond || elapsed > time.Second {
		t.Fatalf("deadline took %s", elapsed)
	}
	unlock, err = e.lockCacheScope("scope", 40*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestO4CheckerServiceMarkerLockTimeoutReported(t *testing.T) {
	home := t.TempDir()
	var calls atomic.Int32
	e := o4CheckerEngine(home, &calls)
	unlock, err := e.lockCacheScope("cache-write", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now()
	_, err = e.startService(fleet.Service{Name: "db", Run: "fake"}, o4CheckerRecord(home, "a"), "/repo")
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "inspect before retry") || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("missing uncertainty error: %v", err)
	}
	if elapsed < 5*time.Second || elapsed > 8*time.Second {
		t.Fatalf("cache timeout=%s", elapsed)
	}
	if calls.Load() != 1 {
		t.Fatalf("effects=%d", calls.Load())
	}
	if mustCache(t, e).Services["/repo|db"] {
		t.Fatal("marker recorded despite write lock timeout")
	}
}

func TestO4CheckerLockIOFailurePreventsExecution(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "materialise-locks"), []byte("obstruction"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	e := o4CheckerEngine(home, &calls)
	if _, err := e.startService(fleet.Service{Name: "db", Run: "fake"}, o4CheckerRecord(home, "a"), "/repo"); err == nil {
		t.Fatal("lock I/O failure ignored")
	}
	if calls.Load() != 0 {
		t.Fatalf("effects=%d before lock", calls.Load())
	}
}

func TestO4CheckerMarkerIOFailureReported(t *testing.T) {
	home := t.TempDir()
	var calls atomic.Int32
	// An unreadable cache stops the run before any effect: a service must not
	// start twice because its marker could not be read.
	e := o4CheckerEngine(home, &calls)
	if err := os.Mkdir(e.cachePath(), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.startService(fleet.Service{Name: "db", Run: "fake"}, o4CheckerRecord(home, "a"), "/repo"); err == nil {
		t.Fatal("unreadable cache ignored")
	}
	if calls.Load() != 0 {
		t.Fatalf("effects=%d before the cache was readable", calls.Load())
	}
	if err := os.Remove(e.cachePath()); err != nil {
		t.Fatal(err)
	}
	// A marker that cannot be written after the effect is reported, not lost:
	// the fake service obstructs the cache path while it "runs".
	e = New(workspace.New(home))
	e.Exec = o4CheckerExec(func(string, []string, string, time.Duration) ([]byte, error) {
		calls.Add(1)
		return nil, os.Mkdir(e.cachePath(), 0700)
	})
	if _, err := e.startService(fleet.Service{Name: "db", Run: "fake"}, o4CheckerRecord(home, "a"), "/repo"); err == nil || !strings.Contains(err.Error(), "inspect before retry") {
		t.Fatalf("marker failure error=%v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("effects=%d", calls.Load())
	}
	tmp, err := filepath.Glob(filepath.Join(home, ".materialise-cache-*"))
	if err != nil || len(tmp) != 0 {
		t.Fatalf("temporary cache leftovers=%v err=%v", tmp, err)
	}
}

type o4ChildConfig struct{ Home, Repo, Service, Workspace, PortVar, ID string }

func o4WaitFile(p string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(p); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waiting for %s", p)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func TestO4CheckerProcessHelper(t *testing.T) {
	raw := os.Getenv("CONTINUUM_O4_CHECKER_CHILD")
	if raw == "" {
		return
	}
	var c o4ChildConfig
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	e := New(workspace.New(c.Home))
	e.Exec = o4CheckerExec(func(string, []string, string, time.Duration) ([]byte, error) {
		f, err := os.OpenFile(filepath.Join(c.Home, "effects"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		_, err = fmt.Fprintln(f, c.ID)
		f.Close()
		time.Sleep(120 * time.Millisecond)
		return nil, err
	})
	if err := os.WriteFile(filepath.Join(c.Home, "ready-"+c.ID), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := o4WaitFile(filepath.Join(c.Home, "go"), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	s, err := e.startService(fleet.Service{Name: c.Service, Run: "fake", PortVar: c.PortVar}, o4CheckerRecord(c.Home, c.Workspace), c.Repo)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := e.cachePut("/child/"+c.ID, fmt.Sprint(i), "hash"); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(c.Home, "result-"+c.ID), b, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestO4CheckerCrossProcessAdmissionAndPreservation(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%v", mixed), func(t *testing.T) {
			home := t.TempDir()
			var calls atomic.Int32
			e := o4CheckerEngine(home, &calls)
			if err := e.cachePut("/old", "old", "seed"); err != nil {
				t.Fatal(err)
			}
			if err := e.serviceMark("old-svc"); err != nil {
				t.Fatal(err)
			}
			const n = 12
			cmds := make([]*exec.Cmd, n)
			logs := make([]*os.File, n)
			want := 1
			if mixed {
				want = 4
			}
			for i := 0; i < n; i++ {
				c := o4ChildConfig{Home: home, Repo: "/repo", Service: "db", Workspace: fmt.Sprint(i), ID: fmt.Sprint(i)}
				if mixed {
					c.Workspace = "a"
					switch i % 4 {
					case 1:
						c.Repo = "/repo2"
					case 2:
						c.PortVar = "PORT"
					case 3:
						c.PortVar = "PORT"
						c.Workspace = "b"
					}
				}
				b, _ := json.Marshal(c)
				cmd := exec.Command(os.Args[0], "-test.run=^TestO4CheckerProcessHelper$", "-test.timeout=15s")
				cmd.Env = append(os.Environ(), "CONTINUUM_O4_CHECKER_CHILD="+string(b))
				f, err := os.Create(filepath.Join(home, "child-"+c.ID+".log"))
				if err != nil {
					t.Fatal(err)
				}
				cmd.Stdout = f
				cmd.Stderr = f
				logs[i] = f
				cmds[i] = cmd
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = cmd.Process.Kill() })
			}
			for i := 0; i < n; i++ {
				if err := o4WaitFile(filepath.Join(home, "ready-"+fmt.Sprint(i)), 10*time.Second); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(home, "go"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			for i, cmd := range cmds {
				err := cmd.Wait()
				logs[i].Close()
				if err != nil {
					b, _ := os.ReadFile(filepath.Join(home, fmt.Sprintf("child-%d.log", i)))
					t.Fatalf("child %d: %v %s", i, err, b)
				}
			}
			b, err := os.ReadFile(filepath.Join(home, "effects"))
			if err != nil {
				t.Fatal(err)
			}
			if got := len(strings.Fields(string(b))); got != want {
				t.Fatalf("cross-process effects=%d, want %d", got, want)
			}
			nonskip := 0
			for i := 0; i < n; i++ {
				b, err := os.ReadFile(filepath.Join(home, "result-"+fmt.Sprint(i)))
				if err != nil {
					t.Fatal(err)
				}
				var s Step
				if err := json.Unmarshal(b, &s); err != nil {
					t.Fatal(err)
				}
				if !s.Skipped {
					nonskip++
				}
			}
			if nonskip != want {
				t.Fatalf("non-skipped=%d want %d", nonskip, want)
			}
			cs := mustCache(t, e)
			if len(cs.Services) != want+1 || !cs.Services["old-svc"] || len(cs.Commands) != 49 || cs.Commands["/old|old"] != "seed" {
				t.Fatalf("cache preservation: services=%d commands=%d old=%v", len(cs.Services), len(cs.Commands), cs)
			}
		})
	}
}

func TestO4CheckerSupervisedDelayedReadinessDoesNotReadmit(t *testing.T) {
	home := t.TempDir()
	var calls atomic.Int32
	var readyAt atomic.Int64
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := o4CheckerEngine(home, &calls)
			e.Settle = time.Second
			e.Dial = func(string, time.Duration) error {
				if at := readyAt.Load(); at != 0 && time.Now().UnixNano() >= at {
					return nil
				}
				return errors.New("fake service still binding")
			}
			e.Exec = o4CheckerExec(func(string, []string, string, time.Duration) ([]byte, error) {
				calls.Add(1)
				entered <- struct{}{}
				<-release
				readyAt.CompareAndSwap(0, time.Now().Add(100*time.Millisecond).UnixNano())
				return nil, nil
			})
			<-start
			_, err := e.startService(fleet.Service{Name: "db", Run: "fake", PortVar: "PORT", Supervise: true}, o4CheckerRecord(home, "a"), "/repo")
			errs <- err
		}()
	}
	close(start)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no service command entered")
	}
	time.Sleep(40 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent supervised same-scope effects=%d, want 1 while first successful start is still binding", calls.Load())
	}
}

func TestO4CheckerConcurrentRunSupervisedBinding(t *testing.T) {
	repo, home := testRepo(t), t.TempDir()
	spec := supervisedSpec()
	if _, err := workspace.New(home).Provision(spec, repo, "w1"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var readyAt atomic.Int64
	start := make(chan struct{})
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := New(workspace.New(home))
			e.Settle = time.Second
			e.Dial = func(string, time.Duration) error {
				if at := readyAt.Load(); at != 0 && time.Now().UnixNano() >= at {
					return nil
				}
				return errors.New("fake service still binding")
			}
			e.Exec = o4CheckerExec(func(string, []string, string, time.Duration) ([]byte, error) {
				calls.Add(1)
				entered <- struct{}{}
				<-release
				readyAt.CompareAndSwap(0, time.Now().Add(100*time.Millisecond).UnixNano())
				return nil, nil
			})
			<-start
			res, err := e.Run(supervisedSpec(), repo, "w1")
			if err == nil && res.State != workspace.StateReady {
				err = fmt.Errorf("materialise state=%s failures=%v", res.State, res.Failed)
			}
			errs <- err
		}()
	}
	close(start)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no service command entered")
	}
	time.Sleep(40 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent Engine.Run effects=%d, want 1 during delayed service binding", calls.Load())
	}
}

func mustCache(t *testing.T, e *Engine) *cacheSet {
	t.Helper()
	cs, err := e.loadCache()
	if err != nil {
		t.Fatal(err)
	}
	return cs
}
