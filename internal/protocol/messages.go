// Package protocol defines the Menagerie relay protocol message types.
//
// protocol-v1.0
//
// This is the Go port of protocol/types.ts, which is the canonical definition.
// Keep the two in sync — if they disagree, types.ts wins.
//
// Transport: WebSocket, JSON text frames only in v1.0. One connection per
// browser<->relay pair; sessions multiplex via session_id.
package protocol

// Version is the protocol version this relay speaks.
const Version = "1.0"

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
)

// Envelope peeks at a message's discriminator before full decoding.
type Envelope struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
}

// ---- Relay -> Browser ----

type Hello struct {
	Type            string   `json:"type"`
	ProtocolVersion string   `json:"protocol_version"`
	RelayVersion    string   `json:"relay_version"`
	RelayName       string   `json:"relay_name"`
	HostOS          string   `json:"host_os"`
	HostArch        string   `json:"host_arch"`
	Agents          []string `json:"agents"`
	Transports      []string `json:"transports"`
	HostsChildren   bool     `json:"hosts_children"`
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
	Type     string            `json:"type"`
	Agent    string            `json:"agent"`
	Cwd      string            `json:"cwd"`
	Args     []string          `json:"args"`
	Env      map[string]string `json:"env"`
	ClientID string            `json:"client_id"`
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
