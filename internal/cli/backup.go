package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/NakliTechie/continuum/internal/journal"
	bolt "go.etcd.io/bbolt"
)

const backupHelp = `Usage: continuum backup create --state PRIVATE_DIR --to NEW_ABSOLUTE_DIR
       continuum backup restore --from BACKUP_DIR --state NEW_ABSOLUTE_DIR --accept-host-id HOST_ID [--replace]
Offline only. Backup includes the private journal, root credentials and managed
workspace records/cache; it does not copy worktree contents or live processes.
Restore preserves the source host ID. --replace moves the old state aside intact.
`

type backupManifest struct {
	Version   int               `json:"version"`
	Schema    string            `json:"schema"`
	HostID    string            `json:"host_id"`
	CreatedAt string            `json:"created_at"`
	Files     map[string]string `json:"files"`
}

var backupFiles = []string{"state.db", "operator.token", "observer.token", "workspaces.json", "materialise-cache.json"}

func backupCLI(args []string, out, diag io.Writer) int {
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(out, backupHelp)
		return 0
	}
	cmd := args[0]
	if cmd != "create" && cmd != "restore" {
		fmt.Fprint(diag, backupHelp)
		return 2
	}
	f := flag.NewFlagSet("backup "+cmd, flag.ContinueOnError)
	f.SetOutput(diag)
	state := f.String("state", "", "private state directory")
	to := f.String("to", "", "new backup directory")
	from := f.String("from", "", "backup directory")
	accept := f.String("accept-host-id", "", "acknowledge preserved host ID")
	replace := f.Bool("replace", false, "move existing state to a recoverable sibling before restore")
	if err := f.Parse(args[1:]); err != nil {
		return 2
	}
	if f.NArg() != 0 || !filepath.IsAbs(*state) || (cmd == "create" && (!filepath.IsAbs(*to) || *from != "")) || (cmd == "restore" && (!filepath.IsAbs(*from) || *accept == "" || *to != "")) {
		fmt.Fprint(diag, backupHelp)
		return 2
	}
	if cmd == "create" {
		manifest, err := createBackup(*state, *to)
		if err != nil {
			fmt.Fprintln(diag, "backup not completed:", err)
			return 1
		}
		_ = json.NewEncoder(out).Encode(map[string]any{"backup": *to, "host_id": manifest.HostID, "files": len(manifest.Files)})
		return 0
	}
	old, err := restoreBackup(*from, *state, *accept, *replace)
	if err != nil {
		fmt.Fprintln(diag, "restore not completed:", err)
		return 1
	}
	_ = json.NewEncoder(out).Encode(map[string]any{"restored": *state, "previous_state": old})
	return 0
}

func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("directory must be private (0700) and not a symlink")
	}
	return nil
}

func privateRegular(path string, max int64) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > max {
		return nil, errors.New("file must be private, regular and within its size limit")
	}
	return info, nil
}

func inspectOfflineDB(path string) (schema, host string, closeDB func(), err error) {
	if _, err = privateRegular(path, 512<<20); err != nil {
		return
	}
	var db *bolt.DB
	db, err = bolt.Open(path, 0600, &bolt.Options{Timeout: 250 * time.Millisecond})
	if err != nil {
		return "", "", nil, fmt.Errorf("state is busy or invalid; stop the daemon: %w", err)
	}
	closeDB = func() { _ = db.Close() }
	err = db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		blocks := tx.Bucket([]byte("blocks"))
		if meta == nil || blocks == nil {
			return errors.New("state database is missing required buckets")
		}
		schema, host = string(meta.Get([]byte("schema"))), string(meta.Get([]byte("host")))
		if schema != "2" || host == "" {
			return errors.New("unsupported schema or missing host identity")
		}
		return blocks.ForEach(func(_, v []byte) error {
			var b journal.Block
			if json.Unmarshal(v, &b) != nil {
				return errors.New("invalid block record")
			}
			if b.State == "active" {
				return errors.New("active blocks cannot be backed up or restored offline")
			}
			return nil
		})
	})
	if err != nil {
		closeDB()
		closeDB = nil
	}
	return
}

func copyChecked(src, dst string, max int64) (string, error) {
	before, err := privateRegular(src, max)
	if err != nil {
		return "", err
	}
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), io.LimitReader(in, max+1))
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	after, err := privateRegular(src, max)
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", errors.New("source changed during offline copy")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func createBackup(state, to string) (backupManifest, error) {
	m := backupManifest{}
	if err := privateDirectory(state); err != nil {
		return m, err
	}
	if _, err := os.Lstat(to); !os.IsNotExist(err) {
		return m, errors.New("backup destination must not exist")
	}
	schema, host, closeDB, err := inspectOfflineDB(filepath.Join(state, "state.db"))
	if err != nil {
		return m, err
	}
	defer closeDB()
	parent := filepath.Dir(to)
	stage, err := os.MkdirTemp(parent, ".continuum-backup-*")
	if err != nil {
		return m, err
	}
	defer os.RemoveAll(stage)
	m = backupManifest{Version: 1, Schema: schema, HostID: host, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Files: map[string]string{}}
	for _, name := range backupFiles {
		src := filepath.Join(state, name)
		if _, err := os.Lstat(src); os.IsNotExist(err) {
			if name == "state.db" {
				return backupManifest{}, err
			}
			continue
		} else if err != nil {
			return backupManifest{}, err
		}
		max := int64(32 << 20)
		if name == "state.db" {
			max = 512 << 20
		}
		digest, err := copyChecked(src, filepath.Join(stage, name), max)
		if err != nil {
			return backupManifest{}, err
		}
		m.Files[name] = digest
	}
	raw, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), append(raw, '\n'), 0600); err != nil {
		return backupManifest{}, err
	}
	if err := os.Rename(stage, to); err != nil {
		return backupManifest{}, err
	}
	return m, nil
}

func readManifest(from string) (backupManifest, error) {
	var m backupManifest
	if err := privateDirectory(from); err != nil {
		return m, err
	}
	path := filepath.Join(from, "manifest.json")
	if _, err := privateRegular(path, 1<<20); err != nil {
		return m, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if json.Unmarshal(raw, &m) != nil || m.Version != 1 || m.Schema != "2" || m.HostID == "" || len(m.Files) < 1 || len(m.Files) > len(backupFiles) {
		return m, errors.New("unsupported backup manifest")
	}
	allowed := map[string]bool{}
	for _, name := range backupFiles {
		allowed[name] = true
	}
	for name, digest := range m.Files {
		if !allowed[name] || len(digest) != 64 {
			return m, errors.New("backup contains an unexpected file or checksum")
		}
	}
	if m.Files["state.db"] == "" {
		return m, errors.New("backup has no state.db")
	}
	return m, nil
}

func restoreBackup(from, state, accept string, replace bool) (string, error) {
	m, err := readManifest(from)
	if err != nil {
		return "", err
	}
	if state == "/" {
		return "", errors.New("refusing a broad restore destination")
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil && filepath.Clean(state) == filepath.Clean(home) {
		return "", errors.New("refusing to replace the home directory")
	}
	if cwd, cwdErr := os.Getwd(); cwdErr == nil && filepath.Clean(state) == filepath.Clean(cwd) {
		return "", errors.New("refusing to replace the workspace root")
	}
	if accept != m.HostID {
		return "", errors.New("--accept-host-id does not match the backup; no state changed")
	}
	parent := filepath.Dir(state)
	stage, err := os.MkdirTemp(parent, ".continuum-restore-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	for name, want := range m.Files {
		max := int64(32 << 20)
		if name == "state.db" {
			max = 512 << 20
		}
		digest, err := copyChecked(filepath.Join(from, name), filepath.Join(stage, name), max)
		if err != nil || digest != want {
			return "", errors.New("backup file is missing, unsafe or fails checksum verification")
		}
	}
	schema, host, closeDB, err := inspectOfflineDB(filepath.Join(stage, "state.db"))
	if err != nil {
		return "", err
	}
	closeDB()
	if schema != m.Schema || host != m.HostID {
		return "", errors.New("backup state identity does not match manifest")
	}
	old := ""
	if _, err := os.Lstat(state); err == nil {
		if !replace {
			return "", errors.New("destination exists; use explicit --replace to preserve it as a sibling")
		}
		if err := privateDirectory(state); err != nil {
			return "", err
		}
		_, _, closeOld, err := inspectOfflineDB(filepath.Join(state, "state.db"))
		if err != nil {
			return "", err
		}
		closeOld()
		old = state + ".pre-restore-" + time.Now().UTC().Format("20060102T150405Z")
		if _, err := os.Lstat(old); !os.IsNotExist(err) {
			return "", errors.New("previous-state destination already exists")
		}
		if err := os.Rename(state, old); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Rename(stage, state); err != nil {
		if old != "" {
			_ = os.Rename(old, state)
		}
		return "", err
	}
	return old, nil
}
