package jsonwire

import (
	"bytes"
	"encoding/json"
	"testing"
)

type frame struct {
	Type   string          `json:"type"`
	Acp    json.RawMessage `json:"acp"`
	hidden json.RawMessage
}

type page struct {
	Events []*frame         `json:"events"`
	Extra  map[string]any   `json:"extra"`
	Ptr    *json.RawMessage `json:"ptr,omitempty"`
}

// Raw payloads keep their bytes on every toolchain: U+2028/U+2029 stay raw,
// an escape the agent wrote stays an escape, and <>& are never escaped.
func TestMarshalKeepsRawPayloadBytes(t *testing.T) {
	payload := []byte("{\"text\":\"<>&\u2028\u2029 日本🙂 \\u2028 literal\"}")
	compact := payload // already compact

	got, err := Marshal(frame{Type: "session_update", Acp: payload})
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(`{"type":"session_update","acp":`), compact...), '}')
	if !bytes.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}

	raw := json.RawMessage("{ \"a\" :\"\u2029\" }")
	p := page{
		Events: []*frame{{Type: "x", Acp: payload}, {Type: "y", Acp: json.RawMessage(`{"plain":true}`)}},
		Extra:  map[string]any{"acp": json.RawMessage(payload), "n": 1},
		Ptr:    &raw,
	}
	got, err = Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte(`\u2028`+" 日本")) || bytes.Count(got, []byte("\u2028")) != 2 || !bytes.Contains(got, []byte(`{"a":"`+"\u2029"+`"}`)) {
		t.Fatalf("nested raw payloads were rewritten: %q", got)
	}
	if bytes.Count(got, []byte(`\u2028 literal`)) != 2 {
		t.Fatalf("an escape the agent wrote was changed: %q", got)
	}
	if !json.Valid(got) {
		t.Fatal("invalid JSON")
	}
	// The caller's value is untouched.
	if !bytes.Equal(p.Events[0].Acp, payload) || !bytes.Equal(*p.Ptr, raw) {
		t.Fatal("Marshal modified its argument")
	}
}

func TestMarshalPlainStringsStillEncode(t *testing.T) {
	got, err := Marshal(map[string]string{"s": "a\u2028b<>"})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]string
	if err := json.Unmarshal(got, &back); err != nil || back["s"] != "a\u2028b<>" {
		t.Fatalf("round trip %q: %v", got, err)
	}
}
