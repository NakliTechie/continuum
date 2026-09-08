// Package protocol defines the Menagerie relay protocol message types.
//
// protocol-v1.3
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
const Version = "1.3"

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
	// TypeSeen: a client reporting that a human actually looked at this session.
	// The ONLY thing that demotes `done` to `idle` — reading a session over the
	// protocol must not (spec §8.1.1 E5), or a supervisor polling its workers
	// would silently clear the human's attention badge.
	TypeSeen = "seen"
	// TypeWait / TypeWaited: park until a session reaches one of the named
	// lifecycle states (spec §8.1). Server-owned and event-driven — the relay
	// resolves on a transition it already tracks, so no client polls.
	// TypeReportStatus: an agent declaring its own lifecycle state (spec §8.3).
	// Authoritative — from the first report the relay stops second-guessing that
	// session with output heuristics (E9: one status authority, never two).
	TypeReportStatus = "report_status"
	TypeWait         = "wait"
	TypeWaited       = "waited"
	TypeAttached     = "attached"

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
	// ErrResumeUnsupported: a spawn asked to reopen an agent's own past
	// conversation, but this agent has no recorded resume invocation. Refused
	// rather than downgraded to a fresh session, so the caller learns the
	// conversation is not coming back instead of silently losing it.
	ErrResumeUnsupported = "resume_unsupported"
	// ErrBadWait: a wait named no states, or named one outside the lifecycle
	// vocabulary. Refused rather than silently narrowed — a wait that can never
	// resolve is indistinguishable from a hang.
	ErrBadWait = "bad_wait"
	// ErrBadStatus: a self-report named a state outside the lifecycle vocabulary,
	// or one a session may not declare about itself.
	ErrBadStatus = "bad_status"
	// ErrSessionBlocked: the session is waiting on a human decision, so a prompt
	// was refused and NOTHING was sent (spec §8.2.1 E6). An approval dialog reads
	// the next input as its answer, so a prompt here would approve or reject a
	// tool call its sender never saw. Inspect the pending request and answer it
	// deliberately instead.
	ErrSessionBlocked = "session_blocked"
	ErrInvalidToken   = "invalid_token"
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
	// EventDone: the agent finished and no one has looked yet. A `seen` frame
	// from a client that actually showed it to a human demotes it to idle.
	// Reading a session over the protocol must never demote it (spec §8.1.1 E5).
	EventDone = "done"
)

// Session lifecycle statuses. The relay tracks these server-side so `wait` has
// something to resolve against; they are the same vocabulary the events carry.
const (
	StatusRunning     = "running"
	StatusIdle        = "idle"
	StatusDone        = "done"
	StatusNeedsInput  = "needs_input"
	StatusStalled     = "stalled"
	StatusRateLimited = "rate_limited"
	StatusExited      = "exited"
	// StatusUnknown: an agent is present but its state cannot be classified.
	// Never a claim of completion, and it never satisfies a wait unless the
	// caller named it (spec §8.1.1 E3).
	StatusUnknown = "unknown"
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
	// ResumeAgents lists the agent ids this relay can restart into one of their
	// own past conversations (spawn.resume_agent_session). Absent/empty ⇒ none.
	ResumeAgents []string `json:"resume_agents,omitempty"`
}

type Registered struct {
	Type string `json:"type"`
}

type Spawned struct {
	Type            string `json:"type"`
	SessionID       string `json:"session_id"`
	ClientID        string `json:"client_id"`
	SessionToken    string `json:"session_token"`
	Agent           string `json:"agent"`
	PID             int    `json:"pid"`
	StartedAt       string `json:"started_at"`
	ParentSessionID string `json:"parent_session_id,omitempty"` // protocol 1.3; set for a child session
	// AgentSessionID is the agent's OWN session reference, when it issued one
	// (structured sessions do at session/new). Persist it: handing it back as
	// spawn.resume_agent_session is what reopens this conversation later.
	AgentSessionID string `json:"agent_session_id,omitempty"`
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
	Type            string            `json:"type"`
	Agent           string            `json:"agent"`
	Cwd             string            `json:"cwd"`
	Args            []string          `json:"args"`
	Env             map[string]string `json:"env"`
	ClientID        string            `json:"client_id"`
	Transport       string            `json:"transport,omitempty"`         // protocol 1.2; absent ⇒ pty
	ParentSessionID string            `json:"parent_session_id,omitempty"` // protocol 1.3; spawn as a child (supervisor tree)
	// ResumeAgentSession asks the agent to reopen one of ITS OWN past
	// conversations, identified by a reference the agent itself issued. Distinct
	// from the `resume` frame, which re-attaches this browser to a live relay
	// session. The relay appends the agent's recorded resume argv; it never
	// guesses a flag, and refuses when the agent has none.
	ResumeAgentSession string `json:"resume_agent_session,omitempty"`
}

type Input struct {
	Type         string `json:"type"`
	SessionID    string `json:"session_id"`
	SessionToken string `json:"session_token"`
	Data         string `json:"data"`
}

// ReportStatus (agent -> relay) declares the session's own lifecycle state.
// More reliable than the relay's LooksLike* heuristics, which is the point: an
// agent knows whether it is working, and guessing from output has
// false-positives. Message is display-only and never drives a wait.
type ReportStatus struct {
	Type         string `json:"type"`
	SessionID    string `json:"session_id"`
	SessionToken string `json:"session_token"`
	State        string `json:"state"`
	Message      string `json:"message,omitempty"`
}

// Wait (client -> relay) parks until the named session reaches one of `Until`.
// A wait whose condition is ALREADY true resolves immediately (§8.1.1 E1) —
// otherwise a supervisor that misses a transition by a millisecond parks until
// timeout on something that already happened.
type Wait struct {
	Type         string   `json:"type"`
	SessionID    string   `json:"session_id"`
	SessionToken string   `json:"session_token"`
	Until        []string `json:"until"`
	TimeoutMS    int      `json:"timeout_ms,omitempty"`
	// WaitID is echoed back, so one client can hold several waits on one session.
	WaitID string `json:"wait_id,omitempty"`
}

// Waited (relay -> client) resolves a Wait.
type Waited struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	WaitID    string `json:"wait_id,omitempty"`
	State     string `json:"state"`
	TimedOut  bool   `json:"timed_out"`
	// Brokered marks a wait that some client composed on the relay's behalf
	// rather than one this relay owns. Always false from a relay; a broker sets
	// it so a supervisor knows the wait dies with the broker instead of assuming
	// the durability a relay-owned wait has.
	Brokered bool `json:"brokered,omitempty"`
}

// Seen (client -> relay) marks a session as looked-at by a human.
type Seen struct {
	Type         string `json:"type"`
	SessionID    string `json:"session_id"`
	SessionToken string `json:"session_token"`
}

type Signal struct {
	Type         string `json:"type"`
	SessionID    string `json:"session_id"`
	SessionToken string `json:"session_token"`
	Signal       string `json:"signal"`
	Cols         int    `json:"cols,omitempty"`
	Rows         int    `json:"rows,omitempty"`
	Subtree      bool   `json:"subtree,omitempty"` // protocol 1.3; kill only — also kill descendants (leaf-first)
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
	// Wait arms a wait in the SAME frame as the prompt (spec §8.2), so a
	// supervisor cannot miss a transition that happens between a separate prompt
	// and wait. The relay arms it before dispatching the prompt.
	Wait *WaitSpec `json:"wait,omitempty"`
}

// WaitSpec is the wait half of an atomic prompt+wait.
type WaitSpec struct {
	Until     []string `json:"until"`
	TimeoutMS int      `json:"timeout_ms,omitempty"`
	WaitID    string   `json:"wait_id,omitempty"`
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
	SessionID       string `json:"session_id"`
	Agent           string `json:"agent"`
	StartedAt       string `json:"started_at"`
	PID             int    `json:"pid"`
	Transport       string `json:"transport,omitempty"`         // protocol 1.2; absent ⇒ pty (so re-attach doesn't guess)
	ParentSessionID string `json:"parent_session_id,omitempty"` // protocol 1.3; present for a child (re-attach rebuilds the tree)
	AgentSessionID  string `json:"agent_session_id,omitempty"`  // the agent's OWN session reference; feed back as spawn.resume_agent_session
	Status          string `json:"status,omitempty"`            // the relay's tracked lifecycle status, so a re-attach restores done/idle instead of assuming running
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
