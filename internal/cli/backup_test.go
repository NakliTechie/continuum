package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/NakliTechie/continuum/internal/journal"
)

func TestOfflineBackupRestoreChecksumAndRecoverableReplace(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	s, err := journal.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	host := s.Host
	if err := s.AddBlock(journal.Block{ID: "aaaaaaaaaaaaaaaa", State: "exited"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanShutdown(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "operator.token"), []byte("private-root-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "backup")
	m, err := createBackup(state, backup)
	if err != nil {
		t.Fatal(err)
	}
	if m.HostID != host || m.Files["state.db"] == "" || m.Files["operator.token"] == "" {
		t.Fatal(m)
	}
	target := filepath.Join(root, "restored")
	if _, err := restoreBackup(backup, target, "wrong", false); err == nil {
		t.Fatal("host ID not acknowledged")
	}
	if _, err := restoreBackup(backup, target, host, false); err != nil {
		t.Fatal(err)
	}
	old, err := restoreBackup(backup, target, host, true)
	if err != nil || old == "" {
		t.Fatal("replace did not preserve previous state", old, err)
	}
	if _, err := os.Stat(filepath.Join(old, "state.db")); err != nil {
		t.Fatal("previous state was not recoverable", err)
	}
	if err := os.WriteFile(filepath.Join(backup, "operator.token"), []byte("tampered\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreBackup(backup, filepath.Join(root, "corrupt"), host, false); err == nil {
		t.Fatal("tampered backup restored")
	}
}
