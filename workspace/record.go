package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// recordFile is the relay's own record of what it has provisioned. It lives on
// the relay's disk, not in the repo: it is machine state, not a committed
// document.
const recordFile = "workspaces.json"

// Record is one provisioned workspace.
type Record struct {
	Name   string            `json:"name"`
	Repo   string            `json:"repo"`
	Branch string            `json:"branch"`
	Path   string            `json:"path"`
	Ports  map[string]int    `json:"ports"`
	Vars   map[string]string `json:"vars"`
	State  string            `json:"state"`
	// Reason says why the workspace is in State, when State is unhealthy. It is
	// what lets supervision clear an unhealthy it caused without clearing one a
	// failed health probe caused.
	Reason    string `json:"reason,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// States a workspace can be in. `unhealthy` is deliberately distinct from
// `ready`: a workspace whose probes failed must never read as usable.
const (
	StateProvisioning  = "provisioning"
	StateMaterialising = "materialising"
	StateReady         = "ready"
	StateUnhealthy     = "unhealthy"
)

type recordSet struct {
	Workspaces map[string]*Record `json:"workspaces"`
}

func recordPath(home string) string { return filepath.Join(home, recordFile) }

func loadRecords(home string) (*recordSet, error) {
	rs := &recordSet{Workspaces: map[string]*Record{}}
	b, err := os.ReadFile(recordPath(home))
	if os.IsNotExist(err) {
		return rs, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return rs, nil
	}
	if err := json.Unmarshal(b, rs); err != nil {
		return nil, err
	}
	if rs.Workspaces == nil {
		rs.Workspaces = map[string]*Record{}
	}
	return rs, nil
}

// saveRecords writes through a temp file and renames, so a crash mid-write
// leaves the previous record intact rather than a truncated one.
func saveRecords(home string, rs *recordSet) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		return err
	}
	tmp := recordPath(home) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, recordPath(home))
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
