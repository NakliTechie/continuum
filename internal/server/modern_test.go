package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/journal"
	"github.com/NakliTechie/continuum/internal/protocol"
)

func modernTest(t *testing.T) (*Modern, func(string, api.Request) api.Response) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	noCapture := ""
	s := New(&config.Config{RegistrationToken: "operator", Agents: map[string]config.Agent{"custom": {}}, Tmux: "off", CaptureDir: &noCapture})
	m := s.EnableModern(store, "viewer")
	h := httptest.NewServer(m)
	t.Cleanup(func() { h.Close(); s.StopAll(); time.Sleep(40 * time.Millisecond); store.Close() })
	return m, func(token string, q api.Request) api.Response {
		t.Helper()
		raw, _ := json.Marshal(q)
		r, _ := http.NewRequest("POST", h.URL, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var v api.Response
		if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
}
func value(t *testing.T, r api.Response, key string) string {
	t.Helper()
	if r.Class != "ok" {
		t.Fatalf("unexpected response %+v", r)
	}
	var v map[string]any
	_ = json.Unmarshal(r.Result, &v)
	s, ok := v[key].(string)
	if !ok {
		t.Fatalf("missing %s", key)
	}
	return s
}
func TestModernObserversAndLegacyFencing(t *testing.T) {
	m, call := modernTest(t)
	if r := call("bad", api.Request{Operation: "status"}); r.Class != "access_denied" {
		t.Fatal(r)
	}
	if r := call("viewer", api.Request{Operation: "open"}); r.Class != "access_denied" {
		t.Fatal(r)
	}
	q := api.Request{Operation: "open", RequestID: "open", Cwd: t.TempDir(), Args: []string{"/bin/cat"}}
	first := call("operator", q)
	id := value(t, first, "block_id")
	if second := call("operator", q); string(second.Result) != string(first.Result) {
		t.Fatal("duplicate launch changed result")
	}
	q.Args = []string{"/bin/sh"}
	if r := call("operator", q); r.Class != "conflict" {
		t.Fatal(r)
	}
	lease := value(t, call("operator", api.Request{Operation: "acquire", Block: id, RequestID: "control"}), "lease")
	for i := 0; i < 2; i++ {
		r := call("viewer", api.Request{Operation: "events", Block: id})
		if r.Class != "ok" {
			t.Fatal(r)
		}
	}
	if r := call("operator", api.Request{Operation: "input", RequestID: "input", Block: id, Lease: lease, Data: "observers-did-not-steal\n"}); r.Class != "ok" {
		t.Fatal(r)
	}
	// An explicit legacy attach fences the modern controller in the same registry.
	cn := &conn{srv: m.s, ctx: context.Background(), registered: true, sink: func([]byte) error { return nil }}
	raw, _ := json.Marshal(protocol.Attach{SessionID: id})
	cn.handleAttach(raw)
	if r := call("operator", api.Request{Operation: "input", RequestID: "stale", Block: id, Lease: lease, Data: "bad"}); r.Code != "stale_control" {
		t.Fatal(r)
	}
	fresh := value(t, call("operator", api.Request{Operation: "takeover", Block: id, RequestID: "takeover"}), "lease")
	if fresh == lease {
		t.Fatal("takeover reused secret")
	}
	m.leases[id] = structLeaseExpired(fresh)
	if r := call("operator", api.Request{Operation: "resize", RequestID: "expired", Block: id, Lease: fresh, Cols: 80, Rows: 24}); r.Code != "stale_control" {
		t.Fatal(r)
	}
}
func structLeaseExpired(token string) lease { return lease{token, time.Now().Add(-time.Second)} }
func TestUnresolvedIntentAndCaptureFailure(t *testing.T) {
	m, call := modernTest(t)
	q := api.Request{Operation: "open", RequestID: "pending", Cwd: t.TempDir(), Args: []string{"/bin/cat"}}
	raw, _ := json.Marshal(q)
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])
	if _, err := m.Store.Begin(q.RequestID, digest); err != nil {
		t.Fatal(err)
	}
	if r := call("operator", q); r.Class != "indeterminate" {
		t.Fatal(r)
	}
	m.degraded.Store(true)
	q.RequestID = "new"
	if r := call("operator", q); r.Class != "resource_exhausted" {
		t.Fatal(r)
	}
	if r := call("viewer", api.Request{Operation: "events"}); r.Code != "capture_degraded" {
		t.Fatal(r)
	}
}
