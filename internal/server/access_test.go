package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/journal"
)

func grantToken(t *testing.T, v api.Response) (string, string) {
	t.Helper()
	if v.Class != "ok" {
		t.Fatal(v)
	}
	var r struct {
		Token string `json:"token"`
		ID    string `json:"grant_id"`
	}
	if json.Unmarshal(v.Result, &r) != nil || r.Token == "" || r.ID == "" {
		t.Fatal("missing show-once token", v)
	}
	return r.Token, r.ID
}

func TestGrantDoesNotFollowRetiredBlockIDIntoNewEpoch(t *testing.T) {
	m, call := modernTest(t)
	m.StartWaits(t.TempDir(), nil)
	defer m.StopWaits()
	id := "dddddddddddddddd"
	if err := m.Store.AddBlock(journal.Block{ID: id, State: "exited", Started: "epoch-one"}); err != nil {
		t.Fatal(err)
	}
	token, _ := grantToken(t, call("operator", api.Request{Operation: "grant_create", RequestID: "epoch-grant", Grant: &api.GrantSpec{Class: "observer", Blocks: []string{id}}}))
	if v := call("operator", api.Request{Operation: "share_set", RequestID: "epoch-share", Block: id, Sharing: "observers"}); v.Class != "ok" {
		t.Fatal(v)
	}
	if v := call(token, api.Request{Operation: "events", Block: id}); v.Class != "ok" {
		t.Fatal(v)
	}
	if err := m.Store.Retire(id); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.AddBlock(journal.Block{ID: id, State: "exited", Started: "epoch-two"}); err != nil {
		t.Fatal(err)
	}
	if v := call(token, api.Request{Operation: "events", Block: id}); v.Class != "access_denied" {
		t.Fatal("grant followed reused block identity", v)
	}
}

func TestScopedGrantsSharingRevocationAndBrowserObserver(t *testing.T) {
	m, call := modernTest(t)
	m.StartWaits(t.TempDir(), nil)
	defer m.StopWaits()
	id := value(t, call("operator", api.Request{Operation: "open", RequestID: "grant-block", Cwd: t.TempDir(), Args: []string{"/bin/cat"}}), "block_id")
	observer, observerID := grantToken(t, call("operator", api.Request{Operation: "grant_create", RequestID: "grant-observer", Grant: &api.GrantSpec{Class: "observer", Blocks: []string{id}}}))
	if v := call("operator", api.Request{Operation: "grant_create", RequestID: "grant-observer", Grant: &api.GrantSpec{Class: "observer", Blocks: []string{id}}}); v.Code != "token_not_replayed" {
		t.Fatal("token replayed", v)
	}
	if v := call(observer, api.Request{Operation: "status"}); v.Class != "ok" || !bytes.Contains(v.Result, []byte(`"total":0`)) {
		t.Fatal("private block disclosed", v)
	}
	if v := call("operator", api.Request{Operation: "share_set", RequestID: "share-observer", Block: id, Sharing: "observers"}); v.Class != "ok" {
		t.Fatal(v)
	}
	if v := call(observer, api.Request{Operation: "events", Block: id}); v.Class != "ok" {
		t.Fatal("scoped read denied", v)
	}
	if v := call(observer, api.Request{Operation: "acquire", Block: id, RequestID: "observer-mutate"}); v.Class != "access_denied" {
		t.Fatal("observer mutated", v)
	}
	controller, _ := grantToken(t, call("operator", api.Request{Operation: "grant_create", RequestID: "grant-controller", Grant: &api.GrantSpec{Class: "controller", Blocks: []string{id}}}))
	if v := call(controller, api.Request{Operation: "acquire", Block: id, RequestID: "controller-too-early"}); v.Class != "access_denied" {
		t.Fatal("controller crossed sharing policy", v)
	}
	if v := call("operator", api.Request{Operation: "share_set", RequestID: "share-controller", Block: id, Sharing: "controllers"}); v.Class != "ok" {
		t.Fatal(v)
	}
	lease := value(t, call(controller, api.Request{Operation: "acquire", Block: id, RequestID: "controller-acquire"}), "lease")
	if v := call(controller, api.Request{Operation: "input", Block: id, RequestID: "controller-input", Lease: lease, Data: "scoped\n"}); v.Class != "ok" {
		t.Fatal(v)
	}
	if v := call(controller, api.Request{Operation: "takeover", Block: id, RequestID: "controller-takeover"}); v.Class != "access_denied" {
		t.Fatal("controller got moderator authority", v)
	}
	moderator, _ := grantToken(t, call("operator", api.Request{Operation: "grant_create", RequestID: "grant-moderator", Grant: &api.GrantSpec{Class: "moderator", Blocks: []string{id}}}))
	if v := call(moderator, api.Request{Operation: "takeover", Block: id, RequestID: "moderator-takeover"}); v.Class != "ok" {
		t.Fatal(v)
	}
	if v := call(observer, api.Request{Operation: "grant_list"}); v.Class != "access_denied" {
		t.Fatal("grant read widened", v)
	}
	if v := call("operator", api.Request{Operation: "grant_revoke", RequestID: "revoke-observer", GrantID: observerID}); v.Class != "ok" {
		t.Fatal(v)
	}
	if v := call(observer, api.Request{Operation: "events", Block: id}); v.Class != "access_denied" {
		t.Fatal("revoked observer still reads", v)
	}
	if v := call("viewer", api.Request{Operation: "events", Block: id}); v.Class != "ok" {
		t.Fatal("root observer changed", v)
	}
	browser := httptest.NewServer(http.HandlerFunc(m.BrowserRead))
	defer browser.Close()
	browserCall := func(token, operation string) int {
		body, _ := json.Marshal(api.Request{Operation: operation})
		r, _ := http.NewRequest(http.MethodPost, browser.URL, bytes.NewReader(body))
		r.Header.Set("Origin", browser.URL)
		r.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if status := browserCall("operator", "status"); status != http.StatusForbidden {
		t.Fatal("browser accepted root operator", status)
	}
	if status := browserCall("viewer", "status"); status != http.StatusOK {
		t.Fatal("browser rejected observer", status)
	}
	if status := browserCall("viewer", "open"); status != http.StatusForbidden {
		t.Fatal("browser mutation accepted", status)
	}
}
