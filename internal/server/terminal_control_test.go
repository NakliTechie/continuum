package server

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/api"
)

func TestTerminalBinaryInputAndRenewal(t *testing.T) {
	m, call := modernTest(t)
	id := value(t, call("operator", api.Request{Operation: "open", Terminal: "screen-v1", RequestID: "binary-open", Cwd: t.TempDir(), Args: []string{"/bin/sh", "-c", "stty raw -echo; printf READY; dd bs=1 count=4 2>/dev/null | od -An -tx1 | tr -s ' '"}}), "block_id")
	waitScreen(t, call, id, "READY")
	token := value(t, call("operator", api.Request{Operation: "acquire", Block: id, RequestID: "binary-control"}), "lease")
	m.s.controlMu.Lock()
	m.leases[id] = lease{Token: token, Expires: time.Now().Add(time.Second)}
	m.s.controlMu.Unlock()
	q := api.Request{Operation: "renew", Block: id, Lease: token, RequestID: "renew-1"}
	renewed := call("operator", q)
	if renewed.Class != "ok" {
		t.Fatal(renewed)
	}
	var result struct {
		Expires time.Time `json:"expires_at"`
	}
	if json.Unmarshal(renewed.Result, &result) != nil || time.Until(result.Expires) < 55*time.Second {
		t.Fatal(string(renewed.Result))
	}
	if again := call("operator", q); string(again.Result) != string(renewed.Result) {
		t.Fatal("duplicate renewal changed its committed result")
	}
	if r := call("viewer", api.Request{Operation: "renew", Block: id, Lease: token, RequestID: "viewer-renew"}); r.Class != "access_denied" {
		t.Fatal(r)
	}
	want := []byte{0xff, 0xc3, 0xa9, 0x03}
	if r := call("operator", api.Request{Operation: "input", Encoding: "base64", Data: base64.StdEncoding.EncodeToString(want), Block: id, Lease: token, RequestID: "binary-input"}); r.Class != "ok" {
		t.Fatal(r)
	}
	waitScreen(t, call, id, "ff c3 a9 03")
}
func TestTerminalRenewalRemainsFencedAndInputValidation(t *testing.T) {
	_, call := modernTest(t)
	id := value(t, call("operator", api.Request{Operation: "open", Terminal: "screen-v1", RequestID: "validation-open", Cwd: t.TempDir(), Args: []string{"/bin/cat"}}), "block_id")
	a := value(t, call("operator", api.Request{Operation: "acquire", Block: id, RequestID: "validation-a"}), "lease")
	b := value(t, call("operator", api.Request{Operation: "takeover", Block: id, RequestID: "validation-b"}), "lease")
	if r := call("operator", api.Request{Operation: "renew", Block: id, Lease: a, RequestID: "old-renew"}); r.Code != "stale_control" {
		t.Fatal(r)
	}
	for i, q := range []api.Request{{Encoding: "base64", Data: "%%%"}, {Encoding: "rot13", Data: "x"}, {Data: strings.Repeat("x", 64<<10+1)}} {
		q.Operation = "input"
		q.Block = id
		q.Lease = b
		q.RequestID = "invalid-" + string(rune('a'+i))
		if r := call("operator", q); r.Class != "invalid_request" {
			t.Fatal(i, r)
		}
	}
	legacy := value(t, call("operator", api.Request{Operation: "open", RequestID: "legacy-open-validation", Cwd: t.TempDir(), Args: []string{"/bin/cat"}}), "block_id")
	legacyLease := value(t, call("operator", api.Request{Operation: "acquire", Block: legacy, RequestID: "legacy-acquire-validation"}), "lease")
	if r := call("operator", api.Request{Operation: "input", Encoding: "base64", Data: "eA==", Block: legacy, Lease: legacyLease, RequestID: "legacy-binary"}); r.Code != "terminal_profile_required" {
		t.Fatal(r)
	}
}
