// Package acp runs structured-session children that speak the pinned Agent
// Client Protocol (JSON-RPC 2.0, newline-delimited) over stdio.
//
// One child per session, one owner goroutine reading its stdout, writes
// serialized behind a mutex — the same discipline the PTY path uses. Every
// frame crossing the wire, both directions, is appended to the session's
// event log before any interpretation: the log is both the replay artifact
// and the debugging artifact (v1.1 handoff §C2). The relay never interprets
// ACP payloads beyond routing: updates pass through verbatim; permission
// requests are correlated and answered by option id.
package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NakliTechie/continuum/internal/jsonwire"
	"github.com/NakliTechie/continuum/internal/pty"
)

// ProtocolVersion is the ACP protocol version negotiated at initialize; it
// must match the unchanged inherited pin recorded in docs/acp-protocol.md.
const ProtocolVersion = 1

const (
	handshakeTimeout = 30 * time.Second
	maxLine          = 8 << 20 // frames can carry large diffs; cap a single frame at 8 MiB
)

// Envelope is one JSON-RPC 2.0 message. ID stays raw so responses echo the
// exact bytes the request used (ids may be numbers or strings per JSON-RPC).
type Envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("acp rpc error %d: %s", e.Code, e.Message) }

func (e *Envelope) hasID() bool      { return len(e.ID) > 0 && string(e.ID) != "null" }
func (e *Envelope) isResponse() bool { return e.Method == "" && e.hasID() }

func idKey(raw json.RawMessage) string { return string(bytes.TrimSpace(raw)) }

// permOption mirrors the slice of the ACP PermissionOption shape needed to
// answer a request.
type permOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

type pendingPerm struct {
	rpcID   json.RawMessage
	options []permOption
	bytes   int
}

// Session is one running ACP agent child.
type Session struct {
	ID        string // menagerie session id
	Agent     string
	StartedAt time.Time
	PID       int

	ACPSessionID string // agent-side id from session/new

	// InitConfig holds the raw `configOptions` array from the session/new
	// result (model / mode / thought_level selectors, with labels). Agents
	// deliver these in the response, not as a notification, so the server
	// re-surfaces them to the browser through the session_update funnel.
	InitConfig json.RawMessage

	stdout   *os.File
	readDone chan struct{}
	readErr  error // read only after readDone closes
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	writeErr error    // first stdin write failure; the stream is desynced after it. Guarded by writeMu.
	cap      *os.File // event log: ~/.menagerie/sessions/<id>.acp.jsonl

	decisionMu sync.Mutex // serializes permission decisions, cancellation, and new prompt admission
	writeMu    sync.Mutex
	capMu      sync.Mutex // serializes event-log (s.cap) writes across reader + writer goroutines

	mu              sync.Mutex
	nextID          int64
	nextReq         int64
	pending         map[string]chan *Envelope // normalized raw id -> waiter
	perms           map[string]*pendingPerm   // menagerie request_id -> pending permission
	closed          bool
	permissionBytes int
	cancelling      bool // guarded by mu; late requests receive cancelled until the next prompt

	// Callbacks fire on the reader goroutine; keep them fast or hand off.
	// Set OnUpdate through SetOnUpdate, never directly: a resumed session's
	// replayed transcript arrives DURING session/load, before the caller can
	// attach, and those frames must not be dropped.
	updateMu            sync.Mutex
	onUpdate            func(params json.RawMessage)
	pendingUpdates      [][]byte
	onPermissionRequest func(requestID string, params json.RawMessage)
	pendingEvents       []startupEvent
	pendingBytes        int
	eventErr            error
}

// ErrLoadUnsupported means the agent does not advertise the loadSession
// capability, so its past conversations cannot be reopened. The caller must
// surface this rather than start a fresh session in its place.
var ErrLoadUnsupported = errors.New("agent does not support acp session/load")

// Start spawns cmd (the agent's ACP server), completes the initialize +
// session/new handshake, and returns the ready session.
func Start(id, agent, cwd string, cmd *exec.Cmd, capture ...*string) (*Session, error) {
	return StartContext(context.Background(), id, agent, cwd, cmd, capture...)
}

// StartContext cancels startup only; the established process outlives this context.
func StartContext(ctx context.Context, id, agent, cwd string, cmd *exec.Cmd, capture ...*string) (*Session, error) {
	return start(ctx, id, agent, cwd, cmd, "", capture...)
}

// Resume spawns cmd and reopens the agent's own past conversation through ACP
// session/load instead of session/new, so the agent replays that conversation
// rather than starting an empty one. It fails when the agent does not advertise
// the loadSession capability — the caller must not silently fall back to a fresh
// session, which the user would mistake for a resumed one.
func Resume(id, agent, cwd string, cmd *exec.Cmd, acpSessionID string, capture ...*string) (*Session, error) {
	return ResumeContext(context.Background(), id, agent, cwd, cmd, acpSessionID, capture...)
}

// ResumeContext is Resume with cancellable startup.
func ResumeContext(ctx context.Context, id, agent, cwd string, cmd *exec.Cmd, acpSessionID string, capture ...*string) (*Session, error) {
	if acpSessionID == "" {
		return nil, errors.New("acp resume: empty session id")
	}
	return start(ctx, id, agent, cwd, cmd, acpSessionID, capture...)
}

func start(ctx context.Context, id, agent, cwd string, cmd *exec.Cmd, resumeID string, capture ...*string) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Own stdout: exec.Cmd.Wait must not close it before queued frames drain.
	stdoutPipe, stdoutWriter, err := os.Pipe()
	if err != nil {
		stdinPipe.Close()
		return nil, err
	}
	defer stdoutWriter.Close()
	cmd.Stdout = stdoutWriter
	cmd.Stderr = os.Stderr
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		stdoutPipe.Close()
		stdinPipe.Close()
		return nil, err
	}
	stdoutWriter.Close()

	s := &Session{
		ID:        id,
		Agent:     agent,
		StartedAt: time.Now(),
		PID:       cmd.Process.Pid,
		cmd:       cmd,
		stdout:    stdoutPipe,
		readDone:  make(chan struct{}),
		stdin:     stdinPipe,
		pending:   make(map[string]chan *Envelope),
		perms:     make(map[string]*pendingPerm),
	}
	dir, capErr := pty.SessionsDir()
	if len(capture) > 0 && capture[0] != nil {
		dir = *capture[0]
		capErr = nil
	}
	if capErr == nil && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err == nil {
			s.cap, _ = os.OpenFile(filepath.Join(dir, id+".acp.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		}
	}

	go s.readLoop(stdoutPipe)

	initResp, err := s.requestContext(ctx, "initialize", map[string]any{
		"protocolVersion":    ProtocolVersion,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}},
	}, handshakeTimeout)
	if err != nil {
		s.abortStart()
		return nil, fmt.Errorf("acp initialize: %w", err)
	}
	if initResp.Error != nil {
		s.abortStart()
		return nil, fmt.Errorf("acp initialize: %v", initResp.Error)
	}
	// The agent must say it can reload a session before we ask it to.
	if resumeID != "" {
		var initRes struct {
			AgentCapabilities struct {
				LoadSession bool `json:"loadSession"`
			} `json:"agentCapabilities"`
		}
		if err := json.Unmarshal(initResp.Result, &initRes); err != nil || !initRes.AgentCapabilities.LoadSession {
			s.abortStart()
			return nil, fmt.Errorf("%w (agent %s)", ErrLoadUnsupported, agent)
		}
		ld, err := s.requestContext(ctx, "session/load", map[string]any{"sessionId": resumeID, "cwd": cwd, "mcpServers": []any{}}, handshakeTimeout)
		if err != nil {
			s.abortStart()
			return nil, fmt.Errorf("acp session/load: %w", err)
		}
		if ld.Error != nil {
			s.abortStart()
			return nil, fmt.Errorf("acp session/load: %v", ld.Error)
		}
		// The agent replays the conversation as session/update notifications
		// before answering, so by here the transcript is already on its way to
		// the client through OnUpdate.
		s.ACPSessionID = resumeID
		return s, nil
	}
	sn, err := s.requestContext(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}}, handshakeTimeout)
	if err != nil {
		s.abortStart()
		return nil, fmt.Errorf("acp session/new: %w", err)
	}
	if sn.Error != nil {
		s.abortStart()
		return nil, fmt.Errorf("acp session/new: %v", sn.Error)
	}
	var snr struct {
		SessionID     string          `json:"sessionId"`
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	if err := json.Unmarshal(sn.Result, &snr); err != nil || snr.SessionID == "" {
		s.abortStart()
		return nil, fmt.Errorf("acp session/new: bad result %s", string(sn.Result))
	}
	s.ACPSessionID = snr.SessionID
	// Empty arrays and null both mean "no selectors" — leave InitConfig nil so
	// the server skips the config frame rather than emitting an empty one.
	if len(snr.ConfigOptions) > 0 && string(snr.ConfigOptions) != "null" && string(snr.ConfigOptions) != "[]" {
		s.InitConfig = snr.ConfigOptions
	}
	return s, nil
}

// Run blocks until the child exits, then reaps it. Mirrors pty.Session.Run's
// contract minus output callbacks — run it in a goroutine.
func (s *Session) Run(onExit func(code int)) {
	err := s.cmd.Wait()
	// The leader exiting ends its managed process group. Escaped descendants
	// may retain stdout; bound that drain and surface any incomplete capture.
	_ = syscall.Kill(-s.PID, syscall.SIGKILL)
	_ = s.stdout.SetReadDeadline(time.Now().Add(2 * time.Second))
	<-s.readDone
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	s.closeFiles()
	if onExit != nil {
		onExit(code)
	}
}

// readLoop consumes the child's stdout until EOF. Frames that fail to parse
// still land in the event log first; interpretation never gates capture.
func (s *Session) readLoop(r io.ReadCloser) {
	defer r.Close()
	defer close(s.readDone)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxLine)
	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		line := make([]byte, len(raw))
		copy(line, raw)
		s.logFrame("a>c", line)

		var env Envelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		s.dispatchFrame(&env, line)
		if err := s.EventError(); err != nil {
			s.readErr = err
			s.Kill()
			return
		}
	}
	s.readErr = scanner.Err()
	if s.readErr != nil {
		s.Kill()
	}
}

// ReadError is valid after Run returns or from its exit callback.
func (s *Session) ReadError() error { return s.readErr }

func (s *Session) dispatch(env *Envelope) {
	raw, _ := jsonwire.Marshal(env)
	s.dispatchFrame(env, raw)
}

func (s *Session) dispatchFrame(env *Envelope, raw []byte) {
	switch {
	case env.isResponse():
		key := idKey(env.ID)
		s.mu.Lock()
		ch := s.pending[key]
		delete(s.pending, key)
		s.mu.Unlock()
		if ch != nil {
			ch <- env
		}
	case env.hasID() && env.Method == "session/request_permission":
		s.handlePermissionRequest(env, raw)
	case env.hasID():
		// An agent->client request we do not implement (fs reads, terminal…).
		_ = s.write(Envelope{JSONRPC: "2.0", ID: env.ID, Error: &RPCError{Code: -32601, Message: "not supported by relay"}})
	case env.Method == "session/update":
		s.deliverOrHold(startupEvent{payload: raw})
	default:
		// Unknown notification: already captured in the event log; ignore.
	}
}

func (s *Session) handlePermissionRequest(env *Envelope, raw []byte) {
	var pr struct {
		Options []permOption `json:"options"`
	}
	_ = json.Unmarshal(env.Params, &pr)

	s.decisionMu.Lock()
	s.mu.Lock()
	if s.cancelling {
		s.mu.Unlock()
		err := s.write(Envelope{JSONRPC: "2.0", ID: env.ID, Result: permissionResult("cancelled", "")})
		s.decisionMu.Unlock()
		if err != nil {
			s.failEvents(fmt.Errorf("cancel late permission: %w", err))
		}
		return
	}
	if len(s.perms) >= maxPendingPermissions || s.permissionBytes+len(raw) > maxPendingPermissionBytes {
		s.mu.Unlock()
		s.decisionMu.Unlock()
		s.failEvents(fmt.Errorf("ACP pending permission budget exceeded (%d requests / %d bytes)", maxPendingPermissions, maxPendingPermissionBytes))
		return
	}
	s.nextReq++
	reqID := fmt.Sprintf("pr-%d", s.nextReq)
	s.perms[reqID] = &pendingPerm{rpcID: append(json.RawMessage(nil), env.ID...), options: pr.Options, bytes: len(raw)}
	s.permissionBytes += len(raw)
	s.mu.Unlock()
	s.decisionMu.Unlock()

	s.deliverOrHold(startupEvent{requestID: reqID, payload: raw})
}

// RespondPermission answers a pending permission request. explicitOptionID
// wins when non-empty; otherwise outcome maps onto the agent's offered kinds:
// approve → allow_once (fallback allow_always), approve_always → allow_always,
// reject → any reject_*.
func (s *Session) RespondPermission(requestID, outcome, explicitOptionID string) error {
	s.decisionMu.Lock()
	defer s.decisionMu.Unlock()
	s.mu.Lock()
	p := s.perms[requestID]
	if p == nil {
		s.mu.Unlock()
		return fmt.Errorf("unknown permission request %q", requestID)
	}
	optionID := explicitOptionID
	if optionID == "" {
		optionID = resolveOutcome(outcome, p.options)
	}
	valid := false
	for _, option := range p.options {
		if option.OptionID == optionID && optionID != "" {
			valid = true
		}
	}
	if !valid {
		s.mu.Unlock()
		return fmt.Errorf("no offered option for outcome %q", outcome)
	}
	rpcID := append(json.RawMessage(nil), p.rpcID...)
	s.mu.Unlock()

	if err := s.write(Envelope{JSONRPC: "2.0", ID: rpcID, Result: permissionResult("selected", optionID)}); err != nil {
		return err
	}
	s.retirePermission(requestID, p)
	return nil
}

func resolveOutcome(outcome string, options []permOption) string {
	preferred := map[string][]string{
		"approve":        {"allow_once", "allow_always"},
		"approve_always": {"allow_always", "allow_once"},
		"reject":         {"reject_once", "reject_always", "reject"},
	}[outcome]
	if preferred == nil {
		return "" // unknown/empty outcome fails CLOSED — caller errors, never auto-approve
	}
	for _, want := range preferred {
		for _, o := range options {
			if o.Kind == want {
				return o.OptionID
			}
		}
	}
	// Requested disposition not offered under a known kind: fall back only among
	// reject kinds (denying is always safe). Never pick the first option — agents
	// list allow-options first, so that would auto-approve a malformed response.
	if outcome == "reject" {
		for _, o := range options {
			if strings.HasPrefix(o.Kind, "reject") {
				return o.OptionID
			}
		}
	}
	return ""
}

// Prompt submits a user prompt to the structured session and returns the
// channel on which the turn's final response will arrive (stopReason et al).
// Turns can run long — there is deliberately no timeout here.
func (s *Session) Prompt(text string) (<-chan *Envelope, error) {
	s.decisionMu.Lock()
	defer s.decisionMu.Unlock()
	s.mu.Lock()
	s.cancelling = false
	s.mu.Unlock()
	params := map[string]any{
		"sessionId": s.ACPSessionID,
		"prompt":    []map[string]any{{"type": "text", "text": text}},
	}
	ch, _, err := s.sendRequest("session/prompt", params)
	return ch, err
}

// Cancel asks the agent to stop the current turn (ACP cancel notification).
func (s *Session) Cancel() error { return s.cancelPermissions() }

// Kill hard-stops the child process (the `signal kill` path).
func (s *Session) Kill() {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed || s.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
}

func (s *Session) closeFiles() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	// Unblock every in-flight request (e.g. handlePrompt waiting on a turn) whose
	// child just died — otherwise the waiter goroutine and its pending slot leak.
	for key, ch := range s.pending {
		select {
		case ch <- nil:
		default:
		}
		delete(s.pending, key)
	}
	s.mu.Unlock()

	// Flush/close the child pipe under writeMu (write() uses it too — different
	// lock from s.mu, so take it here to avoid racing a late Cancel/RespondPermission).
	s.writeMu.Lock()
	_ = s.stdin.Close()
	s.writeMu.Unlock()
	s.capMu.Lock()
	if s.cap != nil {
		_ = s.cap.Close()
	}
	s.capMu.Unlock()
}

func (s *Session) requestContext(ctx context.Context, method string, params any, timeout time.Duration) (*Envelope, error) {
	ch, cleanup, err := s.sendRequest(method, params)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.readDone:
		if s.readErr != nil {
			return nil, fmt.Errorf("agent stdout: %w", s.readErr)
		}
		return nil, errors.New("agent stdout closed during request")
	case resp := <-ch:
		if resp == nil {
			return nil, errors.New("agent exited during request")
		}
		return resp, nil
	case <-timer.C:
		return nil, fmt.Errorf("%s: timed out after %s", method, timeout)
	}
}

// sendRequest registers a pending response slot, writes the request, and
// returns the one-shot response channel. cleanup() removes the slot (for
// timeouts / abandoned prompts).
func (s *Session) sendRequest(method string, params any) (<-chan *Envelope, func(), error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("session %s closed", s.ID)
	}
	s.nextID++
	idBytes, _ := json.Marshal(s.nextID)
	env := Envelope{JSONRPC: "2.0", ID: idBytes, Method: method, Params: mustJSON(params)}
	ch := make(chan *Envelope, 1)
	key := idKey(idBytes)
	s.pending[key] = ch
	s.mu.Unlock()

	if err := s.write(env); err != nil {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
		return nil, nil, err
	}
	cleanup := func() {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
	}
	return ch, cleanup, nil
}

// ErrStdinFaulted marks a session whose agent stopped draining stdin: a bounded
// write timed out mid-line, the framing is unrecoverable, and the child has been
// killed so its exit path can report the fault instead of leaving a session that
// can neither be cancelled nor answered. Errors carry both this sentinel and the
// original failure in their unwrap chain.
var ErrStdinFaulted = errors.New("agent stopped reading stdin")

// write serializes one frame out and captures it before the bytes hit the pipe.
// The write is bounded so a stalled agent cannot pin a control lock; the first
// failure latches, kills the child, and every later write reports the fault.
//
// Fail-stop is deliberate: an agent that is merely slow for a second loses its
// session too. The alternative, resuming a half-written line later, would hand
// the agent interleaved frames; a clean kill with a recorded reason is the
// honest outcome, and real agents drain stdin on a dedicated reader.
func (s *Session) write(env Envelope) error {
	line, err := json.Marshal(env)
	if err != nil {
		return err
	}
	s.logFrame("c>a", line)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.writeErr != nil {
		return fmt.Errorf("%w: %w", ErrStdinFaulted, s.writeErr)
	}
	if pipe, ok := s.stdin.(*os.File); ok {
		if err := pipe.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			return err
		}
	}
	if _, err := s.stdin.Write(append(line, '\n')); err != nil {
		s.writeErr = err
		s.Kill()
		return fmt.Errorf("%w: %w", ErrStdinFaulted, err)
	}
	return nil
}

// WriteError reports the latched stdin fault, if any. Valid at any time; the
// exit callback records it alongside the exit code.
func (s *Session) WriteError() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.writeErr
}

// logFrame appends one wrapped frame to the event log. Capture happens BEFORE
// any interpretation, for every frame in both directions.
func (s *Session) logFrame(dir string, frame []byte) {
	if s.cap == nil {
		return
	}
	// A line that is not JSON (an agent's debug print, a banner) is captured
	// as text: encoding/json refuses an invalid RawMessage and the frame would
	// otherwise vanish from the replay artifact, contradicting capture-first.
	var rec []byte
	if json.Valid(frame) {
		rec, _ = json.Marshal(struct {
			At    string          `json:"at"`
			Dir   string          `json:"dir"`
			Frame json.RawMessage `json:"frame"`
		}{time.Now().UTC().Format(time.RFC3339Nano), dir, json.RawMessage(frame)})
	} else {
		rec, _ = json.Marshal(struct {
			At  string `json:"at"`
			Dir string `json:"dir"`
			Raw string `json:"raw"`
		}{time.Now().UTC().Format(time.RFC3339Nano), dir, string(frame)})
	}
	// Reader goroutine (a>c) and writer path (c>a) both log — serialize so lines
	// never interleave and corrupt the JSONL replay artifact.
	s.capMu.Lock()
	_, _ = s.cap.Write(append(rec, '\n'))
	s.capMu.Unlock()
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}

func (s *Session) abortStart() {
	s.Kill()
	_ = s.cmd.Wait()
	_ = s.stdout.Close()
	<-s.readDone
	s.closeFiles()
}
