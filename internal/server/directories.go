package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/NakliTechie/continuum/api"
)

const directoryEntryLimit = 10000
const directoryNameLimit = 2 << 20

type directoryRoot struct {
	path string
	root *os.Root
}
type directoryBrowser struct{ roots []directoryRoot }
type directoryEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
}
type directoryPage struct {
	Host    string           `json:"host_id"`
	Root    string           `json:"root,omitempty"`
	Path    string           `json:"path,omitempty"`
	Roots   []string         `json:"roots,omitempty"`
	Entries []directoryEntry `json:"entries"`
	Next    string           `json:"next_cursor"`
}
type directoryCursor struct {
	Digest string `json:"digest"`
	Offset int    `json:"offset"`
}

// ConfigureDirectories is startup-only. Hold root descriptors for the daemon's
// lifetime: replacing a configured path must not silently widen authority.
func (m *Modern) ConfigureDirectories(paths []string) error {
	if len(paths) > 32 {
		return errors.New("at most 32 browse roots are allowed")
	}
	b := &directoryBrowser{}
	defer func() {
		if m.directories != b {
			b.close()
		}
	}()
	seen := map[string]bool{}
	for _, path := range paths {
		if !validDirectoryPath(path) {
			return errors.New("browse roots must be clean absolute UTF-8 directory paths (no ..)")
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("browse root: %w", err)
		}
		if !validDirectoryPath(canonical) {
			return errors.New("canonical browse root exceeds the path limits")
		}
		if seen[canonical] {
			continue
		}
		// Trailing slash makes the kernel require a directory even if the
		// configured path is swapped for a FIFO before OpenRoot's own fstat.
		r, err := os.OpenRoot(canonical + string(filepath.Separator))
		if err != nil {
			return fmt.Errorf("browse root: %w", err)
		}
		b.roots = append(b.roots, directoryRoot{canonical, r})
		seen[canonical] = true
	}
	sort.Slice(b.roots, func(i, j int) bool { return b.roots[i].path < b.roots[j].path })
	m.CloseDirectories()
	m.directories = b
	return nil
}
func (b *directoryBrowser) close() {
	for _, r := range b.roots {
		_ = r.root.Close()
	}
}
func (m *Modern) CloseDirectories() {
	if m.directories != nil {
		m.directories.close()
	}
}
func validDirectoryPath(path string) bool {
	if len(path) > 4096 || !utf8.ValidString(path) || strings.ContainsRune(path, 0) || !filepath.IsAbs(path) {
		return false
	}
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		if component == ".." {
			return false
		}
	}
	return filepath.Clean(path) == path
}

func (m *Modern) listDirectories(ctx context.Context, q api.Request) api.Response {
	fail := func(class, code, message string) api.Response {
		return api.Error(q.RequestID, class, code, message, "directories")
	}
	b := m.directories
	if b == nil || len(b.roots) == 0 {
		return fail("unsupported", "directories_disabled", "operator must configure serve --browse-root before browsing")
	}
	if q.Limit < 0 || q.Limit > 200 || len(q.Cursor) > 1024 {
		return fail("invalid_request", "directory_page", "limit must be 1..200 (0 defaults to 100); cursor must fit 1024 bytes")
	}
	p := directoryPage{Host: m.Store.Host, Entries: []directoryEntry{}}
	if q.Path == "" {
		if q.Cursor != "" {
			return fail("invalid_request", "directory_cursor", "a cursor requires its original path")
		}
		for _, r := range b.roots {
			p.Roots = append(p.Roots, r.path)
		}
		v := api.Result(q.RequestID, p)
		v.Durability = "volatile"
		return v
	}
	if !validDirectoryPath(q.Path) {
		return fail("invalid_request", "directory_path", "path must be clean, absolute UTF-8 with no parent traversal")
	}
	var root *directoryRoot
	var relative string
	for i := range b.roots {
		r := &b.roots[i]
		rel, err := filepath.Rel(r.path, q.Path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && (root == nil || len(r.path) > len(root.path)) {
			root, relative = r, rel
		}
	}
	if root == nil {
		return fail("access_denied", "directory_scope", "path is outside configured browse roots")
	}
	pathError := func(err error) api.Response {
		switch {
		case os.IsNotExist(err):
			return fail("invalid_request", "directory_missing", "directory does not exist")
		case errors.Is(err, syscall.ENOTDIR):
			return fail("invalid_request", "not_directory", "path is not a directory")
		case os.IsPermission(err):
			return fail("access_denied", "directory_permission", "directory is not readable by the daemon")
		default:
			return fail("access_denied", "directory_unavailable", "directory cannot be accessed within its configured root")
		}
	}
	// Check the final type atomically at open, inside the confined root. Plain
	// Open/OpenRoot followed by Stat can block forever on a FIFO/device first.
	f, err := root.root.OpenFile(relative, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return pathError(err)
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return pathError(err)
	}
	entries := []directoryEntry{}
	nameBytes := 0
	for {
		if ctx.Err() != nil {
			return fail("unreachable", "directory_cancelled", "directory read cancelled")
		}
		batch, err := f.ReadDir(256)
		if err != nil && err != io.EOF {
			return pathError(err)
		}
		for _, e := range batch {
			nameBytes += len(e.Name())
			if len(entries) >= directoryEntryLimit || nameBytes > directoryNameLimit {
				return fail("resource_exhausted", "directory_size", "directory exceeds the 10,000-entry or 2 MiB name scan limit")
			}
			if !utf8.ValidString(e.Name()) {
				return fail("unsupported", "directory_encoding", "directory contains non-UTF-8 filenames")
			}
			kind := "other"
			switch {
			case e.Type()&os.ModeSymlink != 0:
				kind = "symlink"
			case e.IsDir():
				kind = "directory"
			case e.Type().IsRegular():
				kind = "file"
			}
			// Materialize full paths only for the response page, not 10,000 rows.
			entries = append(entries, directoryEntry{Name: e.Name(), Type: kind})
		}
		if err == io.EOF {
			break
		}
	}
	after, err := f.Stat()
	if err != nil {
		return pathError(err)
	}
	changed := func() api.Response {
		return fail("conflict", "directory_changed", "directory changed; restart listing without a cursor")
	}
	if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		return changed()
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	h := sha256.New()
	// Identity metadata prevents using a cursor against a replacement directory;
	// names/types also detect changes on filesystems with coarse mtime precision.
	identity := before.Sys().(*syscall.Stat_t)
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d:%d:%d:%d\x00", m.Store.Host, root.path, q.Path, identity.Dev, identity.Ino, before.ModTime().UnixNano(), before.Size())
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%s\x00", e.Name, e.Type)
	}
	digest := hex.EncodeToString(h.Sum(nil))
	offset := 0
	if q.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		var cursor directoryCursor
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Offset < 0 || cursor.Offset > directoryEntryLimit || len(cursor.Digest) != 64 {
			return fail("invalid_request", "directory_cursor", "invalid directory cursor")
		}
		if cursor.Digest != digest {
			return changed()
		}
		if cursor.Offset > len(entries) {
			return fail("invalid_request", "directory_cursor", "invalid directory offset")
		}
		offset = cursor.Offset
	}
	limit := q.Limit
	if limit == 0 {
		limit = 100
	}
	p.Root, p.Path = root.path, q.Path
	base, _ := json.Marshal(p)
	pageBytes := len(base) + 256 // include room for the opaque next cursor
	end := offset
	for end < min(offset+limit, len(entries)) {
		e := entries[end]
		e.Path = filepath.Join(q.Path, e.Name)
		raw, _ := json.Marshal(e)
		// Long/control-character names must not exceed the 1 MiB transport
		// envelope. Byte-bounded pages can contain fewer rows than requested.
		if pageBytes+len(raw)+1 > 512<<10 {
			break
		}
		p.Entries = append(p.Entries, e)
		pageBytes += len(raw) + 1
		end++
	}
	if end < len(entries) {
		raw, _ := json.Marshal(directoryCursor{digest, end})
		p.Next = base64.RawURLEncoding.EncodeToString(raw)
	}
	v := api.Result(q.RequestID, p)
	v.Durability = "volatile"
	return v
}
