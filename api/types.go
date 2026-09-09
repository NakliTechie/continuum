// Package api defines the versioned Continuum local-alpha contract.
package api

import "encoding/json"

type Request struct {
	Cursor    string   `json:"cursor,omitempty"`
	Terminal  string   `json:"terminal,omitempty"`
	Operation string   `json:"operation"`
	RequestID string   `json:"request_id,omitempty"`
	Block     string   `json:"block_id,omitempty"`
	Args      []string `json:"args,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	Data      string   `json:"data,omitempty"`
	Lease     string   `json:"lease,omitempty"`
	After     uint64   `json:"after,omitempty"`
	Cols      int      `json:"cols,omitempty"`
	Rows      int      `json:"rows,omitempty"`
}
type Action struct {
	Kind      string `json:"kind"`
	Operation string `json:"operation"`
}
type Response struct {
	Version    int             `json:"schema_version"`
	RequestID  string          `json:"request_id,omitempty"`
	Class      string          `json:"class"`
	Code       string          `json:"code"`
	Message    string          `json:"message,omitempty"`
	Durability string          `json:"durability"`
	Next       Action          `json:"next_action"`
	Result     json.RawMessage `json:"result,omitempty"`
}

func Result(id string, v any) Response {
	b, _ := json.Marshal(v)
	return Response{Version: 1, RequestID: id, Class: "ok", Code: "ok", Durability: "committed", Next: Action{"none", ""}, Result: b}
}
func Error(id, class, code, message, next string) Response {
	return Response{Version: 1, RequestID: id, Class: class, Code: code, Message: message, Durability: "none", Next: Action{"command", next}}
}
func Exit(class string) int {
	switch class {
	case "ok":
		return 0
	case "invalid_request":
		return 2
	case "access_denied":
		return 3
	case "decision_required":
		return 4
	case "unreachable":
		return 5
	case "conflict":
		return 6
	case "history_gap":
		return 7
	case "indeterminate":
		return 8
	case "unsupported":
		return 9
	case "resource_exhausted":
		return 10
	}
	return 8
}
