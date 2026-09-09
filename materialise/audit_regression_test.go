package materialise

import (
	"github.com/NakliTechie/continuum/fleet"
	"github.com/NakliTechie/continuum/workspace"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAuditDryRunCache(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "lock"), []byte("lock"), 0600)
	e := New(workspace.New(home))
	e.DryRun = true
	_, err := e.runCommand(fleet.Command{Run: "false", CacheKey: "lock"}, &workspace.Record{Path: repo, Vars: map[string]string{}}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(e.cachePath()); err == nil {
		t.Fatal("dry run wrote success cache")
	}
}
func TestAuditFailedCommandCache(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "lock"), []byte("lock"), 0600)
	e := New(workspace.New(home))
	c := fleet.Command{Run: "false", CacheKey: "lock"}
	rec := &workspace.Record{Path: repo, Vars: map[string]string{}}
	if _, err := e.runCommand(c, rec, repo); err == nil {
		t.Fatal("expected first failure")
	}
	step, err := e.runCommand(c, rec, repo)
	if err == nil && step.Skipped {
		t.Fatal("failed command now skipped as success")
	}
}
func TestAuditSymlinkWrite(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(root, "source"), []byte("overwritten"), 0600)
	os.Symlink(outside, filepath.Join(root, "link"))
	e := New(workspace.New(t.TempDir()))
	_, err := e.materialiseFile(fleet.File{From: "source", To: "link/victim"}, &workspace.Record{Path: root}, root)
	if err == nil {
		t.Fatal("expected symlink escape refusal")
	}
	if _, err = os.Stat(filepath.Join(outside, "victim")); err == nil {
		t.Fatal("wrote outside workspace through symlink")
	}
}

func TestAuditCacheSkipsWorkspaceArtifacts(t *testing.T) {
	repo := t.TempDir()
	one := t.TempDir()
	two := t.TempDir()
	os.WriteFile(filepath.Join(repo, "lock"), []byte("lock"), 0600)
	e := New(workspace.New(t.TempDir()))
	c := fleet.Command{Run: "touch installed", CacheKey: "lock"}
	for _, root := range []string{one, two} {
		if _, err := e.runCommand(c, &workspace.Record{Path: root, Vars: map[string]string{}}, repo); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(two, "installed")); os.IsNotExist(err) {
		t.Fatal("second workspace skipped setup without receiving first workspace artifacts")
	}
}

func TestAuditShellTimeoutRace(t *testing.T) {
	_, err := (ShellExecutor{}).Run(t.TempDir(), nil, "sleep 0.1", time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout")
	}
	time.Sleep(150 * time.Millisecond)
}
