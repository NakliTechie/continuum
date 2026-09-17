package server

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/api"
)

// A daemon started from an environment with no locale must not hand its
// children a C locale: bash's readline then reads the UTF-8 every client sends
// as meta-prefixed keys (an ellipsis becomes backward-word) and edits the line
// instead of inserting the character. Reproduced on the live run 2026-09-12.
func TestMultibyteInputReachesAnInteractiveShellIntact(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	for _, key := range []string{"LANG", "LC_ALL", "LC_CTYPE"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	_, call := modernTest(t)
	open := call("operator", api.Request{Operation: "open", RequestID: "open", Cwd: t.TempDir(), Args: []string{bash, "--noprofile", "--norc", "-i"}})
	id := value(t, open, "block_id")
	lease := value(t, call("operator", api.Request{Operation: "acquire", Block: id, RequestID: "control"}), "lease")
	time.Sleep(150 * time.Millisecond)
	const line = "echo \"left… right\"\n"
	if r := call("operator", api.Request{Operation: "input", RequestID: "input", Block: id, Lease: lease, Data: line}); r.Class != "ok" {
		t.Fatal(r)
	}
	deadline := time.Now().Add(5 * time.Second)
	var seen strings.Builder
	for time.Now().Before(deadline) {
		r := call("viewer", api.Request{Operation: "events", Block: id})
		var page struct {
			Events []struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			} `json:"events"`
		}
		_ = json.Unmarshal(r.Result, &page)
		seen.Reset()
		for _, e := range page.Events {
			if e.Type != "output" {
				continue
			}
			var p struct{ Data string }
			_ = json.Unmarshal(e.Payload, &p)
			b, _ := base64.StdEncoding.DecodeString(p.Data)
			seen.Write(b)
		}
		// The result must be an unquoted line of its own. PTYs emit both CRLF
		// and bare CR around readline's bracketed-paste mode on Linux.
		for _, line := range strings.FieldsFunc(seen.String(), func(r rune) bool { return r == '\r' || r == '\n' }) {
			if line == "left… right" {
				return
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("shell did not echo the multibyte line intact; saw %q", seen.String())
}
