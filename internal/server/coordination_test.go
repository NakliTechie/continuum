package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/journal"
)

func waitState(t *testing.T, v api.Response) waitRecord {
	t.Helper()
	if v.Class != "ok" {
		t.Fatalf("wait response: %+v", v)
	}
	var r waitRecord
	if err := json.Unmarshal(v.Result, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRemoteWaitDoesNotInventCompletionDuringOutage(t *testing.T) {
	m, call := modernTest(t)
	var network atomic.Int32
	block := "cccccccccccccccc"
	m.StartWaits(t.TempDir(), func(_ context.Context, _ api.Peer, q api.Request) api.Response {
		switch q.Operation {
		case "status":
			return api.Result(q.RequestID, map[string]any{"host_id": "peer-identity", "capabilities": []string{"waits_v1"}})
		case "observe":
			if network.Load() == 0 {
				return api.Error(q.RequestID, "unreachable", "ssh_transport", "offline", "status")
			}
			return api.Result(q.RequestID, journal.Observation{Host: "peer-identity", Block: block, State: "running", Cursor: 0})
		case "events":
			if network.Load() != 2 {
				return api.Error(q.RequestID, "unreachable", "ssh_transport", "offline", "status")
			}
			return api.Result(q.RequestID, journal.Page{First: 1, Last: 1, Next: 1, Events: []journal.Event{{Version: 1, Host: "peer-identity", Block: block, Seq: 1, Type: "done", Payload: json.RawMessage(`{"turn":1}`)}}})
		}
		return api.Error(q.RequestID, "unsupported", "operation", "unexpected", "help")
	})
	defer m.StopWaits()
	if v := call("operator", api.Request{Operation: "peer_add", RequestID: "pin-peer", Peer: &api.Peer{Name: "remote", Host: "user@known-host", State: "/tmp/remote", Binary: "/bin/continuum"}}); v.Class != "ok" {
		t.Fatal(v)
	}
	q := api.Request{Operation: "wait_create", RequestID: "remote-outage", Wait: &api.WaitSpec{Mode: "any", DeadlineS: 5, Sources: []api.WaitSource{{Name: "remote", Peer: "remote", Block: block, Until: []string{"done"}}}}}
	if v := call("operator", q); v.Class != "unreachable" || v.Code != "initial_observation" {
		t.Fatal("offline wait admitted without an initial cursor", v)
	}
	if v := call("viewer", api.Request{Operation: "wait_get", WaitID: q.RequestID}); v.Code != "wait_unknown" {
		t.Fatal("failed admission persisted a wait", v)
	}
	network.Store(1)
	if r := waitState(t, call("operator", q)); r.State != "pending" {
		t.Fatal(r)
	}
	time.Sleep(400 * time.Millisecond)
	r := waitState(t, call("viewer", api.Request{Operation: "wait_get", WaitID: q.RequestID}))
	if r.State != "pending" || r.Sources[0].Matched {
		t.Fatal("outage invented completion", r)
	}
	network.Store(2)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r = waitState(t, call("viewer", api.Request{Operation: "wait_get", WaitID: q.RequestID}))
		if r.State == "satisfied" {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatal("reconnected remote completion was not observed", r)
}

func TestDurableWaitAllReplayCancelAndPeerPin(t *testing.T) {
	m, call := modernTest(t)
	m.StartWaits(t.TempDir(), func(_ context.Context, p api.Peer, q api.Request) api.Response {
		if q.Operation == "status" {
			return api.Result(q.RequestID, map[string]any{"host_id": "remote-host-1", "capabilities": []string{"waits_v1"}})
		}
		return api.Error(q.RequestID, "unreachable", "fake_peer", "not connected", "status")
	})
	defer m.StopWaits()
	a, b := "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	for _, id := range []string{a, b} {
		if err := m.Store.AddBlock(journal.Block{ID: id, State: "active"}); err != nil {
			t.Fatal(err)
		}
		if err := m.Store.Append(id, "running", map[string]any{"turn": 0}); err != nil {
			t.Fatal(err)
		}
	}
	spec := &api.WaitSpec{Mode: "all", DeadlineS: 10, Sources: []api.WaitSource{{Name: "a", Peer: "local", Block: a, Until: []string{"done"}}, {Name: "b", Peer: "local", Block: b, Until: []string{"done"}}}}
	q := api.Request{Operation: "wait_create", RequestID: "durable-all", Wait: spec}
	if r := waitState(t, call("operator", q)); r.State != "pending" {
		t.Fatal(r)
	}
	if v := call("viewer", api.Request{Operation: "wait_output", WaitID: q.RequestID}); v.Code != "wait_not_ready" {
		t.Fatal(v)
	}
	if r := waitState(t, call("operator", q)); r.State != "pending" {
		t.Fatal("duplicate changed wait", r)
	}
	changed := *spec
	changed.Mode = "any"
	q.Wait = &changed
	if v := call("operator", q); v.Class != "conflict" {
		t.Fatal("changed duplicate accepted", v)
	}
	if err := m.Store.Append(a, "done", map[string]any{"turn": 1}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Append(b, "done", map[string]any{"turn": 1}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	var completed waitRecord
	for time.Now().Before(deadline) {
		completed = waitState(t, call("viewer", api.Request{Operation: "wait_get", WaitID: "durable-all"}))
		if completed.State == "satisfied" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if completed.State != "satisfied" || completed.Order == 0 {
		t.Fatal(completed)
	}
	m.StopWaits()
	m.StartWaits(t.TempDir(), func(_ context.Context, _ api.Peer, q api.Request) api.Response {
		if q.Operation == "status" {
			return api.Result(q.RequestID, map[string]any{"host_id": "remote-host-1", "capabilities": []string{"waits_v1"}})
		}
		return api.Error(q.RequestID, "unreachable", "offline", "offline", "status")
	})
	replayed := waitState(t, call("viewer", api.Request{Operation: "wait_output", WaitID: "durable-all"}))
	if replayed.State != completed.State || replayed.Order != completed.Order {
		t.Fatal("restart changed durable result", replayed, completed)
	}
	cancel := api.Request{Operation: "wait_create", RequestID: "durable-cancel", Wait: &api.WaitSpec{Mode: "any", DeadlineS: 10, Sources: []api.WaitSource{{Name: "a", Peer: "local", Block: a, Until: []string{"exited"}}}}}
	if r := waitState(t, call("operator", cancel)); r.State != "pending" {
		t.Fatal(r)
	}
	if r := waitState(t, call("operator", api.Request{Operation: "wait_cancel", RequestID: "cancel-intent", WaitID: "durable-cancel"})); r.State != "cancelled" {
		t.Fatal(r)
	}
	peer := api.Request{Operation: "peer_add", RequestID: "peer-register", Peer: &api.Peer{Name: "studio", Host: "test@example", State: "/tmp/studio-state", Binary: "/bin/continuum"}}
	registered := call("operator", peer)
	if registered.Class != "ok" {
		t.Fatal(registered)
	}
	var p api.Peer
	if json.Unmarshal(registered.Result, &p) != nil || p.HostID != "remote-host-1" {
		t.Fatal(p)
	}
	if v := call("viewer", peer); v.Class != "access_denied" {
		t.Fatal("observer registered peer", v)
	}
	if v := call("operator", api.Request{Operation: "peer_remove", RequestID: "peer-remove", PeerName: "studio"}); v.Class != "ok" {
		t.Fatal(v)
	}
}

func TestIncompleteHistoryMarksTheMemberAndKeepsWatching(t *testing.T) {
	m, call := modernTest(t)
	a, b, c := "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc"
	var remoteExited atomic.Bool
	m.StartWaits(t.TempDir(), func(_ context.Context, _ api.Peer, q api.Request) api.Response {
		switch q.Operation {
		case "status":
			return api.Result(q.RequestID, map[string]any{"host_id": "remote-host-1", "capabilities": []string{"waits_v1"}})
		case "observe":
			return api.Result(q.RequestID, journal.Observation{Host: "remote-host-1", Block: q.Block, State: "running", Cursor: 0, Incomplete: true})
		case "events":
			if q.Block == c {
				return api.Error(q.RequestID, "indeterminate", "store_read", "unreadable", "status")
			}
			page := journal.Page{Incomplete: true}
			if remoteExited.Load() {
				page = journal.Page{First: 1, Last: 1, Next: 1, Incomplete: true, Events: []journal.Event{{Version: 1, Host: "remote-host-1", Block: b, Seq: 1, Type: "exited", Payload: json.RawMessage(`{"turn":0}`)}}}
			}
			v := api.Result(q.RequestID, page)
			v.Class, v.Code = "indeterminate", "capture_degraded"
			return v
		}
		return api.Error(q.RequestID, "unsupported", "operation", "unexpected", "help")
	})
	defer m.StopWaits()
	if err := m.Store.AddBlock(journal.Block{ID: a, State: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Append(a, "running", map[string]any{"turn": 0}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.MarkIncomplete(a); err != nil {
		t.Fatal(err)
	}
	if v := call("operator", api.Request{Operation: "peer_add", RequestID: "pin-remote", Peer: &api.Peer{Name: "remote", Host: "user@known-host", State: "/tmp/remote", Binary: "/bin/continuum"}}); v.Class != "ok" {
		t.Fatal(v)
	}
	sources := []api.WaitSource{{Name: "a", Peer: "local", Block: a, Until: []string{"exited"}}, {Name: "b", Peer: "remote", Block: b, Until: []string{"exited"}}}
	r := waitState(t, call("operator", api.Request{Operation: "wait_create", RequestID: "degraded-all", Wait: &api.WaitSpec{Mode: "all", DeadlineS: 10, Sources: sources}}))
	if r.State != "pending" {
		t.Fatal("incomplete history refused admission", r)
	}
	for _, member := range r.Sources {
		if member.History != "incomplete" || member.Code != "" {
			t.Fatal("admission did not mark incomplete history", member)
		}
	}
	time.Sleep(400 * time.Millisecond)
	r = waitState(t, call("viewer", api.Request{Operation: "wait_get", WaitID: "degraded-all"}))
	if r.State != "pending" {
		t.Fatal("incomplete history without a transition changed the wait", r)
	}
	if err := m.Store.Append(a, "exited", map[string]any{"turn": 0}); err != nil {
		t.Fatal(err)
	}
	remoteExited.Store(true)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		r = waitState(t, call("viewer", api.Request{Operation: "wait_get", WaitID: "degraded-all"}))
		if r.State != "pending" {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if r.State != "satisfied" || r.Code != "condition_met" {
		t.Fatal("journaled exits after incomplete history did not satisfy the wait", r)
	}
	for _, member := range r.Sources {
		if !member.Matched || member.History != "incomplete" {
			t.Fatal("satisfied member lost its incomplete marker", member)
		}
	}
	unreadable := waitState(t, call("operator", api.Request{Operation: "wait_create", RequestID: "unreadable-any", Wait: &api.WaitSpec{Mode: "any", DeadlineS: 10, Sources: []api.WaitSource{{Name: "c", Peer: "remote", Block: c, Until: []string{"exited"}}}}}))
	if unreadable.State != "pending" {
		t.Fatal(unreadable)
	}
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		unreadable = waitState(t, call("viewer", api.Request{Operation: "wait_get", WaitID: "unreadable-any"}))
		if unreadable.State != "pending" {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if unreadable.State != "failed" || unreadable.Code != "source_failed" || unreadable.Sources[0].Code != "source_indeterminate" {
		t.Fatal("an unreadable source was not an explicit failure", unreadable)
	}
}

func TestPromptWaitBindsTheNewACPGeneration(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	store, err := journal.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	noCapture := ""
	srv := New(&config.Config{Name: "coordination-test", Listen: "127.0.0.1:0", Tmux: "off", RegistrationToken: "test-registration-token", CaptureDir: &noCapture, Agents: map[string]config.Agent{"fake": {Command: fakeAgentBin, Transports: []string{"acp"}}}})
	m := srv.EnableModern(store, "viewer")
	m.StartWaits(state, nil)
	ws := httptest.NewServer(srv.Handler())
	httpAPI := httptest.NewServer(m)
	t.Cleanup(func() { httpAPI.Close(); ws.Close(); m.StopWaits(); srv.StopAll(); store.Close() })
	call := func(q api.Request) api.Response {
		raw, _ := json.Marshal(q)
		req, _ := http.NewRequest(http.MethodPost, httpAPI.URL, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer test-registration-token")
		res, err := http.DefaultClient.Do(req)
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
	_, id, _ := registerAndSpawn(t, ws, nil, nil)
	lease := value(t, call(api.Request{Operation: "takeover", RequestID: "prompt-control", Block: id}), "lease")
	q := api.Request{Operation: "prompt_wait", RequestID: "new-turn-wait", Block: id, Lease: lease, Text: "Reply with hello", Wait: &api.WaitSpec{Mode: "all", DeadlineS: 15, Sources: []api.WaitSource{{Name: "new-turn", Peer: "local", Block: id, Until: []string{"done"}}}}}
	if v := call(q); v.Class != "ok" {
		t.Fatal("prompt_wait was not accepted", v)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		r := waitState(t, call(api.Request{Operation: "wait_get", WaitID: q.RequestID}))
		if r.State == "satisfied" {
			if r.Sources[0].MinTurn == 0 {
				t.Fatal("prompt wait was not fenced by turn", r)
			}
			return
		}
		if r.State != "pending" {
			t.Fatal("prompt wait resolved unsuccessfully", r)
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatal("prompt wait did not observe fake ACP completion")
}
