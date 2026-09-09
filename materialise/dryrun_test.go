package materialise

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/NakliTechie/menagerie/relay-go/fleet"
	"github.com/NakliTechie/menagerie/relay-go/workspace"
)

func loadSpec(t *testing.T) *fleet.Spec {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "fleet", "testdata", "valid", "full.json"))
	if err != nil {
		t.Fatal(err)
	}
	spec, issues := fleet.ValidateBytes(b)
	if len(issues) > 0 {
		t.Fatalf("the golden fixture does not validate: %v", issues)
	}
	return spec
}

// The plan must be stable: a plan you cannot diff is not a plan, and this golden
// file is what catches the graph shifting under a refactor.
func TestDryRunMatchesTheGoldenPlan(t *testing.T) {
	spec := loadSpec(t)
	e := New(workspace.New(t.TempDir()))
	e.DryRun = true
	res, err := e.Run(spec, "/repo", "w1")
	if err != nil {
		t.Fatal(err)
	}
	got := RenderPlan(spec, res)
	wantB, err := os.ReadFile(filepath.Join("testdata", "golden", "full-dryrun.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got != string(wantB) {
		t.Fatalf("dry-run plan drifted from the golden file.\n--- got ---\n%s\n--- want ---\n%s", got, wantB)
	}
}

// Twice in a row must produce byte-identical output — a plan that depends on
// wall-clock, map iteration order or an allocator's mood is not comparable.
func TestDryRunIsDeterministic(t *testing.T) {
	spec := loadSpec(t)
	var first string
	for i := 0; i < 5; i++ {
		e := New(workspace.New(t.TempDir()))
		e.DryRun = true
		res, err := e.Run(spec, "/repo", "w1")
		if err != nil {
			t.Fatal(err)
		}
		got := RenderPlan(spec, res)
		if i == 0 {
			first = got
		} else if got != first {
			t.Fatalf("run %d differed from run 0:\n%s\nvs\n%s", i, got, first)
		}
	}
}

// The checkpoint asks for a read-only mount. A unit test cannot mount anything
// without privileges, so the portable equivalent: a home directory with write
// permission removed. Any allocation, record write or cache write would fail
// against it, so a clean run is evidence the dry run touched nothing.
func TestDryRunWritesNothing(t *testing.T) {
	spec := loadSpec(t)
	home := filepath.Join(t.TempDir(), "readonly-home")
	if err := os.MkdirAll(home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })

	e := New(workspace.New(home))
	e.DryRun = true
	fsys := NewRecordingFS(map[string][]byte{})
	ex := &RecordingExecutor{Fail: map[string]bool{}}
	e.FS, e.Exec = fsys, ex

	if _, err := e.Run(spec, "/repo", "w1"); err != nil {
		t.Fatalf("dry run failed against a non-writable home: %v", err)
	}
	if len(fsys.Writes) != 0 {
		t.Errorf("dry run wrote %d file(s): %v", len(fsys.Writes), fsys.Writes)
	}
	if len(ex.Runs) != 0 {
		t.Errorf("dry run executed %d command(s): %v", len(ex.Runs), ex.Runs)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dry run created %d entry/entries in the relay home", len(entries))
	}
}

// A dry run must not allocate: the fake allocator resolves every port to the low
// end of its range, so nothing is bound and nothing is reserved.
func TestDryRunUsesTheFakeAllocator(t *testing.T) {
	spec := loadSpec(t)
	e := New(workspace.New(t.TempDir()))
	e.DryRun = true
	res, err := e.Run(spec, "/repo", "w1")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Workspace.Ports["PORT"]; got != 4000 {
		t.Errorf("PORT = %d, want the range's low end 4000", got)
	}
	if got := res.Workspace.Ports["DB_PORT"]; got != 5500 {
		t.Errorf("DB_PORT = %d, want the range's low end 5500", got)
	}
	if rec, _ := e.Prov.Load("w1"); rec != nil {
		t.Error("a dry run left a workspace record behind")
	}
}
