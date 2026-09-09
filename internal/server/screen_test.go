package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/protocol"
)

func screenValue(t *testing.T, r api.Response) screenResult {
	t.Helper()
	if r.Class != "ok" {
		t.Fatalf("screen response: %+v", r)
	}
	var s screenResult
	if err := json.Unmarshal(r.Result, &s); err != nil {
		t.Fatal(err)
	}
	if r.Durability != "volatile" {
		t.Fatalf("screen cannot claim persistence: %s", r.Durability)
	}
	return s
}
func waitScreen(t *testing.T, call func(string, api.Request) api.Response, id, text string) screenResult {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := screenValue(t, call("viewer", api.Request{Operation: "screen", Block: id}))
		if strings.Contains(strings.Join(s.Frame.Lines, ""), text) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	r := call("viewer", api.Request{Operation: "screen", Block: id})
	t.Fatalf("screen never contained %q; last response: %s", text, r.Result)
	return screenResult{}
}
func TestServerOwnedScreenDetachedQueriesAndLegacyGate(t *testing.T) {
	m, call := modernTest(t)
	helper := filepath.Join(t.TempDir(), "screen-child")
	build := exec.Command("go", "build", "-o", helper, "./testdata/screenchild")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build screen child: %v: %s", err, output)
	}
	id := value(t, call("operator", api.Request{Operation: "open", RequestID: "screen-open", Terminal: "screen-v1", Cols: 90, Rows: 30, Cwd: t.TempDir(), Args: []string{helper}}), "block_id")
	// Every client is absent while the child needs a terminal query response.
	time.Sleep(150 * time.Millisecond)
	frame := waitScreen(t, call, id, "QUERY_OK")
	if frame.Frame.Cols != 90 || frame.Frame.Rows != 30 {
		t.Fatal(frame)
	}
	lease := value(t, call("operator", api.Request{Operation: "acquire", Block: id, RequestID: "screen-acquire"}), "lease")
	before := frame.Frame.Revision
	for i := 0; i < 3; i++ {
		s := screenValue(t, call("viewer", api.Request{Operation: "screen", Block: id, Cols: 200, Rows: 90}))
		if s.Frame.Revision != before || s.Frame.Cols != 90 {
			t.Fatal("observer mutated screen")
		}
	}
	var frames bytes.Buffer
	cn := &conn{srv: m.s, ctx: context.Background(), registered: true, sink: func(b []byte) error { frames.Write(b); return nil }}
	raw, _ := json.Marshal(protocol.Attach{SessionID: id})
	cn.handleAttach(raw)
	if !strings.Contains(frames.String(), "terminal_profile_required") || strings.Contains(frames.String(), "\"type\":\"attached\"") {
		t.Fatal(frames.String())
	}
	if r := call("viewer", api.Request{Operation: "resize", RequestID: "viewer-resize", Block: id, Lease: lease, Cols: 40, Rows: 10}); r.Class != "access_denied" {
		t.Fatal(r)
	}
	if r := call("operator", api.Request{Operation: "input", RequestID: "screen-finish", Block: id, Lease: lease, Data: "finish"}); r.Class != "ok" {
		t.Fatal(r)
	}
	waitScreen(t, call, id, "FINAL_OK")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := screenValue(t, call("viewer", api.Request{Operation: "screen", Block: id}))
		if s.State == "exited" {
			if s.ExitCode == nil || *s.ExitCode != 0 {
				t.Fatal(s)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("final screen not retained")
}
func TestScreenOptInResizeFencingAndFault(t *testing.T) {
	_, call := modernTest(t)
	open := func(profile, id string, args ...string) string {
		return value(t, call("operator", api.Request{Operation: "open", Terminal: profile, RequestID: id, Cwd: t.TempDir(), Args: args}), "block_id")
	}
	legacy := open("", "legacy-cat", "/bin/cat")
	if r := call("viewer", api.Request{Operation: "screen", Block: legacy}); r.Code != "terminal_profile_required" {
		t.Fatal(r)
	}
	id := open("screen-v1", "screen-cat", "/bin/cat")
	a := value(t, call("operator", api.Request{Operation: "acquire", Block: id, RequestID: "a"}), "lease")
	b := value(t, call("operator", api.Request{Operation: "takeover", Block: id, RequestID: "b"}), "lease")
	if r := call("operator", api.Request{Operation: "resize", RequestID: "stale-size", Block: id, Lease: a, Cols: 40, Rows: 10}); r.Code != "stale_control" {
		t.Fatal(r)
	}
	if r := call("operator", api.Request{Operation: "resize", RequestID: "new-size", Block: id, Lease: b, Cols: 40, Rows: 10}); r.Class != "ok" {
		t.Fatal(r)
	}
	frame := screenValue(t, call("viewer", api.Request{Operation: "screen", Block: id}))
	if frame.Frame.Cols != 40 || frame.Frame.Rows != 10 {
		t.Fatal(frame)
	}
	if r := call("operator", api.Request{Operation: "resize", RequestID: "huge-size", Block: id, Lease: b, Cols: 1000, Rows: 1000}); r.Code != "size" {
		t.Fatal(r)
	}
	fault := open("screen-v1", "fault", "/bin/sh", "-c", "printf '\033[999999999b'; sleep 3")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r := call("viewer", api.Request{Operation: "screen", Block: fault})
		if r.Code == "terminal_fault" {
			if r.Class != "indeterminate" || len(r.Result) == 0 {
				t.Fatal(r)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("terminal resource fault not surfaced")
}
func TestScreenAdmissionAndUnknownProfile(t *testing.T) {
	_, call := modernTest(t)
	if r := call("operator", api.Request{Operation: "open", Terminal: "unknown", RequestID: "unknown", Cwd: t.TempDir(), Args: []string{"/bin/cat"}}); r.Code != "terminal_profile" {
		t.Fatal(r)
	}
	cwd := t.TempDir()
	for i := 0; i < 17; i++ {
		r := call("operator", api.Request{Operation: "open", Terminal: "screen-v1", RequestID: "limit-" + strconv.Itoa(i), Cwd: cwd, Args: []string{"/bin/cat"}})
		if i < 16 && r.Class != "ok" {
			t.Fatal(i, r)
		}
		if i == 16 && r.Code != "terminal_limit" {
			t.Fatal(r)
		}
	}
}
