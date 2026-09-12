package server

import (
	"encoding/json"
	"testing"

	"github.com/NakliTechie/continuum/api"
)

// The version operation is the contract's negotiation point. Its identity and
// stable surface are pinned here: changing them is a deliberate, test-breaking
// act, which is what "stable" means. Observers can read it (it never mutates).
func TestVersionOperationPinsTheContract(t *testing.T) {
	_, call := modernTest(t)
	for _, tok := range []string{"operator", "viewer"} {
		r := call(tok, api.Request{Operation: "version"})
		if r.Class != "ok" {
			t.Fatalf("%s version: %+v", tok, r)
		}
		var v struct {
			Contract        string `json:"contract"`
			ContractVersion string `json:"contract_version"`
			SchemaVersion   int    `json:"schema_version"`
			Operations      []string
			Capabilities    struct {
				Stable       []string
				Experimental []string
			}
		}
		if err := json.Unmarshal(r.Result, &v); err != nil {
			t.Fatal(err)
		}
		if v.Contract != "continuum/v1" || v.ContractVersion != "1.0" || v.SchemaVersion != 1 {
			t.Fatalf("contract identity drifted: %+v", v)
		}
		if got := join(v.Capabilities.Stable); got != "pty,observers,control_lease,event_replay,control_renewal,legacy_1.3" {
			t.Fatalf("stable capability set changed without intent: %s", got)
		}
		if got := join(v.Capabilities.Experimental); got != "terminal_screen_v1,terminal_input_base64" {
			t.Fatalf("experimental capability set changed: %s", got)
		}
		if got := join(v.Operations); got != "version,status,events,screen,open,acquire,renew,release,takeover,input,resize,stop" {
			t.Fatalf("operation vocabulary changed without intent: %s", got)
		}
	}
	// status keeps advertising the flat capability union for back-compat, now
	// alongside contract_version.
	sr := call("operator", api.Request{Operation: "status"})
	var sv struct {
		Capabilities    []string
		ContractVersion string `json:"contract_version"`
	}
	_ = json.Unmarshal(sr.Result, &sv)
	if join(sv.Capabilities) != "pty,observers,control_lease,event_replay,control_renewal,legacy_1.3,terminal_screen_v1,terminal_input_base64" || sv.ContractVersion != "1.0" {
		t.Fatalf("status capabilities/contract drifted: %+v", sv)
	}
	// A mutation still needs the operator token; version being a read must not
	// have widened observer authority.
	if r := call("viewer", api.Request{Operation: "stop", RequestID: "x", Block: "0123456789abcdef"}); r.Code != "operator_required" {
		t.Fatalf("observer mutation not refused: %+v", r)
	}
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
