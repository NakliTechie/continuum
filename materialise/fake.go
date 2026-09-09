package materialise

import (
	"fmt"
	"io/fs"
	"sync"
	"time"
)

// RecordingExecutor runs nothing and remembers everything. It is the seam the
// dry run (C3) and the tests share: a run against it proves the graph's shape
// without touching the box.
type RecordingExecutor struct {
	mu   sync.Mutex
	Runs []string
	// Fail makes the named command line return an error, for exercising the
	// unhealthy path.
	Fail map[string]bool
}

func (r *RecordingExecutor) Run(dir string, env []string, cmdline string, timeout time.Duration) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Runs = append(r.Runs, cmdline)
	if r.Fail[cmdline] {
		return nil, fmt.Errorf("command failed (fake): %s", cmdline)
	}
	return nil, nil
}

// Count returns how many times a command line was run — what proves a service
// was started once rather than once per materialisation.
func (r *RecordingExecutor) Count(cmdline string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.Runs {
		if c == cmdline {
			n++
		}
	}
	return n
}

// RecordingFS answers reads from a seeded map and records writes without
// performing them, so a dry run can be asserted to touch nothing.
type RecordingFS struct {
	mu     sync.Mutex
	Seed   map[string][]byte
	Writes map[string][]byte
}

// NewRecordingFS returns a filesystem seeded with the given file contents.
func NewRecordingFS(seed map[string][]byte) *RecordingFS {
	return &RecordingFS{Seed: seed, Writes: map[string][]byte{}}
}

func (f *RecordingFS) ReadFile(p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.Seed[p]; ok {
		return b, nil
	}
	if b, ok := f.Writes[p]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("no such file (fake): %s", p)
}

func (f *RecordingFS) WriteFile(p string, b []byte, _ fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Writes[p] = b
	return nil
}

func (f *RecordingFS) MkdirAll(string, fs.FileMode) error { return nil }

func (f *RecordingFS) Stat(p string) (fs.FileInfo, error) {
	return nil, fmt.Errorf("no such file (fake): %s", p)
}

// PassProber passes every probe; FailProber fails every one.
type PassProber struct{}

func (PassProber) HTTP(string, time.Duration) error { return nil }

type FailProber struct{}

func (FailProber) HTTP(url string, _ time.Duration) error {
	return fmt.Errorf("probe failed (fake): %s", url)
}
