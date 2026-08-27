// Package protocol defines the Menagerie relay protocol message types.
//
// protocol-v1.2
//
// This is the Go port of protocol/types.ts, which is the canonical definition.
// Keep the two in sync — if they disagree, types.ts wins.
//
// Transport: WebSocket, JSON text frames. One connection per browser<->relay
// pair; sessions multiplex via session_id. v1.2 adds structured sessions
// ("acp"): ACP payloads ride nested inside Menagerie frames as RawMessage and
// are never interpreted here (see protocol/acp-pin.md).
package protocol

import "encoding/json"

// Version is the protocol version this relay speaks.
const Version = "1.2"

// Message type discriminators.
const (
	TypeHello        = "hello"
	TypeRegister     = "register"
	TypeRegistered   = "registered"
	TypeSpawn        = "spawn"
	TypeSpawned      = "spawned"
	TypeOutput       = "output"
	TypeInput        = "input"
	TypeSignal       = "signal"
	TypeEvent        = "event"
	TypeResume       = "resume"
	TypeResumeFailed = "resume_failed"
	TypeError        = "error"

	// protocol 1.1: live re-attach after a client reconnect
	TypeSessions = "sessions"
	TypeAttach   = "attach"
	TypeAttached = "attached"

	// protocol 1.2: structured sessions (transport "acp")
	TypeSessionUpdate      = "session_update"
	TypePermissionRequest  = "permission_request"
	TypePermissionResponse = "permission_response"
	TypePrompt             = "prompt"
)

// Transports.
const (
	TransportPTY = "pty"
	TransportACP = "acp"
)

// Permission outcomes for permission_response.
const (
	OutcomeApprove       = "approve"
	OutcomeReject        = "reject"
	OutcomeApproveAlways = "approve_always" // session-scoped only; never persisted
)

// Error codes. The set is open-ended (clients must tolerate unknown codes);
// these are the v1.0 named catalog from protocol.md §7.
const (
	ErrAuthFailed   = "auth_failed"
	ErrUnknownAgent = "unknown_agent"
	ErrSpawnFailed  = "spawn_failed"
	ErrInvalidToken = "invalid_token"
)

// Signal kinds.
const (
	SignalKill      = "kill"
	SignalInterrupt = "interrupt"
	SignalResize    = "resize"
)

// Session events.
const (
	EventExited       = "exited"
	EventIdle         = "idle"
	EventNeedsInput   = "needs_input"
	EventChildSpawned = "child_spawned"
	EventRateLimited  = "rate_limited" // protocol 1.1: generic provider rate-limit signal
	EventStalled      = "stalled"      // recent output keeps repeating — likely stuck in a loop
)

// Envelope peeks at a message's discriminator before full decoding.
type Envelope struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
}

// ---- Relay -> Browser ----

type Hello struct {
	Type            string              `json:"type"`
	ProtocolVersion string              `json:"protocol_version"`
	RelayVersion    string              `json:"relay_version"`
	RelayName       string              `json:"relay_name"`
	HostOS          string              `json:"host_os"`
	HostArch        string              `json:"host_arch"`
	Agents          []string            `json:"agents"`
	Transports      []string            `json:"transports"`
	HostsChildren   bool                `json:"hosts_children"`
	AgentTransports map[string][]string `json:"agent_transports,omitempty"` // protocol 1.2
}

type Registered struct {
	Type string `json:"type"`
}

type Spawned struct {
	Type         string `json:"type"`
	SessionID    string `json:"session_id"`
	ClientID     string `json:"client_id"`
	SessionToken string `json:"session_token"`
	Agent        string `json:"agent"`
	PID          int    `json:"pid"`
	StartedAt    string `json:"started_at"`
}

type Output struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Data      string `json:"data"` // base64-encoded raw PTY bytes
	Seq       int    `json:"seq"`
}

type Event struct {
	Type           string `json:"type"`
	SessionID      string `json:"session_id"`
	Event          string `json:"event"`
	ExitCode       *int   `json:"exit_code,omitempty"`
	ChildSessionID string `json:"child_session_id,omitempty"`
	At             string `json:"at"`
}

type ResumeFailed struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
}

// ---- Browser -> Relay ----

type Register struct {
	Type              string `json:"type"`
	RegistrationToken string `json:"registration_token"`
}

type Spawn struct {
	Type      string            `json:"type"`
	Agent     string            `json:"agent"`
	Cwd       string            `json:"cwd"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	ClientID  string            `json:"client_id"`
	Transport string            `json:"transport,omitempty"` // protocol 1.2; absent ⇒ pty
}

type Input struct {
	Type         string `json:"type"`
	SessionID    string `json:"session_id"`
	SessionToken string `json:"session_token"`
	Data         string `json:"data"`
}

type Signal struct {
	Type         string `json:"type"`
	SessionID    string `json:"session_id"`
	SessionToken string `json:"session_token"`
	Signal       string `json:"signal"`
	Cols         int    `json:"cols,omitempty"`
	Rows         int    `json:"rows,omitempty"`
}

type Resume struct {
	Type         string `json:"type"`
	SessionID    string `json:"session_id"`
	SessionToken string `json:"session_token"`
	LastSeq      int    `json:"last_seq"`
}

// ---- protocol 1.2: structured sessions ----

// SessionUpdate (relay -> browser) wraps ONE ACP agent->client message, verbatim,
// in Acp. The relay never interprets the payload.
type SessionUpdate struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id"`
	Seq       int             `json:"seq"`
	Acp       json.RawMessage `json:"acp"`
}

// PermissionRequest (relay -> browser) surfaces an ACP agent asking to proceed.
// RequestID is relay-correlated; echo it in PermissionResponse.
type PermissionRequest struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id"`
	RequestID string          `json:"request_id"`
	Seq       int             `json:"seq"`
	Acp       json.RawMessage `json:"acp"`
}

// PermissionResponse (browser -> relay) answers a PermissionRequest.
// ApproveAlways is session-scoped only — never persisted across sessions.
type PermissionResponse struct {
	Type         string `json:"type"`
	SessionID    string `json:"session_id"`
	SessionToken string `json:"session_token"`
	RequestID    string `json:"request_id"`
	Outcome      string `json:"outcome"`
	OptionID     string `json:"option_id,omitempty"` // explicit ACP option id when known
}

// Prompt (browser -> relay) prompts a structured session; the structured
// analogue of Input. Input stays PTY-only.
type Prompt struct {
	Type         string `json:"type"`
	SessionID    string `json:"session_id"`
	SessionToken string `json:"session_token"`
	Text         string `json:"text"`
}

// ---- Either direction ----

type Error struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// NewError builds an Error message.
func NewError(sessionID, code, message string) Error {
	return Error{Type: TypeError, SessionID: sessionID, Code: code, Message: message}
}

// ---- protocol 1.1: live re-attach ----

// SessionInfo describes one live session in a Sessions list.
type SessionInfo struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	StartedAt string `json:"started_at"`
	PID       int    `json:"pid"`
	Transport string `json:"transport,omitempty"` // protocol 1.2; absent ⇒ pty (so re-attach doesn't guess)
}

// Sessions (relay -> browser) lists live sessions, sent right after `registered`.
type Sessions struct {
	Type     string        `json:"type"`
	Sessions []SessionInfo `json:"sessions"`
}

// Attach (browser -> relay) re-attaches a registered client to an existing session.
type Attach struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
}

// Attached (relay -> browser) confirms re-attach and issues a fresh session token.
// The relay then replays the session's buffered output and resumes live streaming.
type Attached struct {
	Type         string `json:"type"`
	SessionID    string `json:"session_id"`
	SessionToken string `json:"session_token"`
	Agent        string `json:"agent"`
	StartedAt    string `json:"started_at"`
	PID          int    `json:"pid"`
}
