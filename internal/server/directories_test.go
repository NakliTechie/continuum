package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/NakliTechie/continuum/api"
)

func configureBrowse(t *testing.T, m *Modern, paths ...string) string {
	t.Helper()
	if err := m.ConfigureDirectories(paths); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.CloseDirectories)
	return m.directories.roots[0].path
}
func browsePage(t *testing.T, r api.Response) directoryPage {
	t.Helper()
	var p directoryPage
	if r.Class != "ok" || r.Durability != "volatile" || json.Unmarshal(r.Result, &p) != nil || p.Host == "" {
		t.Fatalf("bad page: %+v", r)
	}
	return p
}
func TestDirectoriesScopeTypesAndPagination(t *testing.T) {
	m, call := modernTest(t)
	if r := call("operator", api.Request{Operation: "directories"}); r.Code != "directories_disabled" {
		t.Fatal(r)
	}
	path := configureBrowse(t, m, t.TempDir())
	if err := os.Mkdir(filepath.Join(path, "a"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "b"), []byte("do-not-return-file-contents"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(path, "c")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(path, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(path, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := call("viewer", api.Request{Operation: "directories"}); r.Code != "operator_required" {
		t.Fatal(r)
	}
	p := browsePage(t, call("operator", api.Request{Operation: "directories"}))
	if len(p.Roots) != 1 || p.Roots[0] != path {
		t.Fatal(p)
	}
	q := api.Request{Operation: "directories", Path: path, Limit: 2}
	p = browsePage(t, call("operator", q))
	if len(p.Entries) != 2 || p.Entries[0].Type != "directory" || p.Entries[1].Type != "file" || p.Next == "" {
		t.Fatal(p)
	}
	q.Cursor = p.Next
	p = browsePage(t, call("operator", q))
	if p.Entries[0].Type != "symlink" || p.Entries[0].Name != "c" || p.Entries[1].Name != "escape" || p.Next == "" {
		t.Fatal(p)
	}
	q.Cursor = p.Next
	p = browsePage(t, call("operator", q))
	if len(p.Entries) != 1 || p.Entries[0].Type != "other" || p.Next != "" {
		t.Fatal(p)
	}
	browsePage(t, call("operator", api.Request{Operation: "directories", Path: filepath.Join(path, "c")}))
	for _, tc := range []struct{ path, code string }{
		{path + "/../outside", "directory_path"}, {"relative", "directory_path"},
		{path + "-prefix-bypass", "directory_scope"}, {"/", "directory_scope"},
		{path + "/missing", "directory_missing"}, {path + "/b", "not_directory"},
		{path + "/fifo", "not_directory"}, {path + "/escape", "directory_unavailable"},
	} {
		if r := call("operator", api.Request{Operation: "directories", Path: tc.path}); r.Code != tc.code {
			t.Fatalf("%s: %+v", tc.path, r)
		}
	}
	q.Cursor = "not a cursor"
	if r := call("operator", q); r.Code != "directory_cursor" {
		t.Fatal(r)
	}
	q.Cursor = ""
	q.Limit = 201
	if r := call("operator", q); r.Code != "directory_page" {
		t.Fatal(r)
	}
	q.Limit = 1
	q.Cursor = browsePage(t, call("operator", q)).Next
	if err := os.WriteFile(path+"/new", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if r := call("operator", q); r.Code != "directory_changed" {
		t.Fatal(r)
	}
	// Shrinking beneath the old offset is a changed listing, not a bad cursor.
	q.Cursor = ""
	q.Limit = 2
	q.Cursor = browsePage(t, call("operator", q)).Next
	for _, name := range []string{"b", "c", "escape", "fifo", "new"} {
		if err := os.Remove(filepath.Join(path, name)); err != nil {
			t.Fatal(err)
		}
	}
	if r := call("operator", q); r.Code != "directory_changed" {
		t.Fatal(r)
	}
}

func TestDirectoriesPermissionsEncodingAndBudget(t *testing.T) {
	m, call := modernTest(t)
	path := configureBrowse(t, m, t.TempDir())
	locked := filepath.Join(path, "locked")
	if err := os.Mkdir(locked, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	if os.Geteuid() != 0 {
		if r := call("operator", api.Request{Operation: "directories", Path: locked}); r.Code != "directory_permission" {
			t.Fatal(r)
		}
	}
	bad := filepath.Join(path, "bad\xff")
	q := api.Request{Operation: "directories", Path: path}
	if err := os.WriteFile(bad, nil, 0600); err == nil {
		if r := call("operator", q); r.Code != "directory_encoding" {
			t.Fatal(r)
		}
		if err := os.Remove(bad); err != nil {
			t.Fatal(err)
		}
	} else if !errors.Is(err, syscall.EILSEQ) {
		t.Fatal(err)
	} else {
		t.Log("filesystem rejects non-UTF-8 filenames; encoding case requires Linux")
	}
	// Include enough filenames to hit the entry ceiling; no partial success.
	for i := 0; i < directoryEntryLimit; i++ {
		if err := os.WriteFile(filepath.Join(path, fmt.Sprintf("%05d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if r := call("operator", q); r.Code != "directory_size" {
		t.Fatal(r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := m.listDirectories(ctx, q); r.Code != "directory_cancelled" {
		t.Fatal(r)
	}
}

func TestDirectoriesRootReplacementAndCursorBinding(t *testing.T) {
	m, call := modernTest(t)
	parent := t.TempDir()
	path := filepath.Join(parent, "root")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	path = configureBrowse(t, m, path)
	for _, name := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(path, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	q := api.Request{Operation: "directories", Path: path, Limit: 1}
	p := browsePage(t, call("operator", q))
	if err := os.Mkdir(path+"/sub", 0700); err != nil {
		t.Fatal(err)
	}
	q.Path = path + "/sub"
	q.Cursor = p.Next
	if r := call("operator", q); r.Class != "invalid_request" && r.Code != "directory_changed" {
		t.Fatal(r)
	}
	if err := os.Rename(path, path+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"/SECRET-REPLACEMENT", nil, 0600); err != nil {
		t.Fatal(err)
	}
	r := call("operator", api.Request{Operation: "directories", Path: path})
	if r.Class != "ok" || strings.Contains(string(r.Result), "SECRET-REPLACEMENT") {
		t.Fatal(r)
	}
	if err := m.ConfigureDirectories([]string{"relative"}); err == nil {
		t.Fatal("relative root accepted")
	}
	if err := m.ConfigureDirectories(make([]string, 33)); err == nil {
		t.Fatal("unbounded roots accepted")
	}
}

func TestDirectoriesSymlinkSwapNeverEscapes(t *testing.T) {
	m, call := modernTest(t)
	root := configureBrowse(t, m, t.TempDir())
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "OUTSIDE_SECRET"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root+"/inside", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/inside/allowed", nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		for ctx.Err() == nil {
			for _, target := range []string{"inside", outside} {
				if err := os.Symlink(target, root+"/next-link"); err != nil {
					done <- err
					return
				}
				if err := os.Rename(root+"/next-link", root+"/link"); err != nil {
					done <- err
					return
				}
			}
		}
		done <- nil
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for i := 0; i < 150; i++ {
		r := call("operator", api.Request{Operation: "directories", Path: root + "/link"})
		if strings.Contains(string(r.Result), "OUTSIDE_SECRET") {
			t.Fatal("symlink swap escaped root", r)
		}
		if r.Class == "ok" {
			p := browsePage(t, r)
			if len(p.Entries) != 1 || p.Entries[0].Name != "allowed" {
				t.Fatal(p)
			}
		}
	}
}

func TestDirectoriesPageByteBudget(t *testing.T) {
	m, call := modernTest(t)
	root := configureBrowse(t, m, t.TempDir())
	// Long JSON-escaped paths can exceed a transport bound well before 200 rows.
	path := root
	for i := 0; i < 5; i++ {
		path = filepath.Join(path, strings.Repeat("\x01", 90))
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		name := fmt.Sprintf("%03d-%s", i, strings.Repeat("\x01", 50))
		if err := os.WriteFile(filepath.Join(path, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	q := api.Request{Operation: "directories", Path: path, Limit: 200}
	r := call("operator", q)
	p := browsePage(t, r)
	if len(r.Result) > 512<<10 || len(p.Entries) >= 200 || len(p.Entries) == 0 || p.Next == "" {
		t.Fatalf("unbounded page: %d bytes, %d rows", len(r.Result), len(p.Entries))
	}
	count := len(p.Entries)
	q.Cursor = p.Next
	p = browsePage(t, call("operator", q))
	if count+len(p.Entries) != 200 || p.Next != "" {
		t.Fatal("byte pagination lost rows")
	}
}
