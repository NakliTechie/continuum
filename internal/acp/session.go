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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/NakliTechie/menagerie/relay-go/internal/pty"
)

// ProtocolVersion is the ACP protocol version negotiated at initialize; it
// must match the pin recorded in protocol/acp-pin.md.
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

	cmd   *exec.Cmd
	stdin io.WriteCloser
	w     *bufio.Writer
	cap   *os.File // event log: ~/.menagerie/sessions/<id>.acp.jsonl

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	nextReq int64
	pending map[string]chan *Envelope // normalized raw id -> waiter
	perms   map[string]*pendingPerm   // menagerie request_id -> pending permission
	closed  bool

	// Callbacks fire on the reader goroutine; keep them fast or hand off.
	OnUpdate            func(params json.RawMessage)
	OnPermissionRequest func(requestID string, params json.RawMessage)
}

// Start spawns cmd (the agent's ACP server), completes the initialize +
// session/new handshake, and returns the ready session.
func Start(id, agent, cwd string, cmd *exec.Cmd) (*Session, error) {
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	s := &Session{
		ID:        id,
		Agent:     agent,
		StartedAt: time.Now(),
		PID:       cmd.Process.Pid,
		cmd:       cmd,
		stdin:     stdinPipe,
		w:         bufio.NewWriter(stdinPipe),
		pending:   make(map[string]chan *Envelope),
		perms:     make(map[string]*pendingPerm),
	}
	if dir, err := pty.SessionsDir(); err == nil {
		if err := os.MkdirAll(dir, 0o700); err == nil {
			s.cap, _ = os.OpenFile(filepath.Join(dir, id+".acp.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		}
	}

	go s.readLoop(stdoutPipe)

	initResp, err := s.request("initialize", map[string]any{
		"protocolVersion":    ProtocolVersion,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}},
	}, handshakeTimeout)
	if err != nil {
		s.Kill()
		return nil, fmt.Errorf("acp initialize: %w", err)
	}
	if initResp.Error != nil {
		s.Kill()
		return nil, fmt.Errorf("acp initialize: %v", initResp.Error)
	}
	sn, err := s.request("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}}, handshakeTimeout)
	if err != nil {
		s.Kill()
		return nil, fmt.Errorf("acp session/new: %w", err)
	}
	if sn.Error != nil {
		s.Kill()
		return nil, fmt.Errorf("acp session/new: %v", sn.Error)
	}
	var snr struct {
		SessionID     string          `json:"sessionId"`
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	if err := json.Unmarshal(sn.Result, &snr); err != nil || snr.SessionID == "" {
		s.Kill()
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
		s.dispatch(&env)
	}
}

func (s *Session) dispatch(env *Envelope) {
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
	case env.hasID() && strings.HasPrefix(env.Method, "session/request_permission"):
		s.handlePermissionRequest(env)
	case env.hasID():
		// An agent->client request we do not implement (fs reads, terminal…).
		_ = s.write(Envelope{JSONRPC: "2.0", ID: env.ID, Error: &RPCError{Code: -32601, Message: "not supported by relay"}})
	case env.Method == "session/update":
		if s.OnUpdate != nil {
			// Full envelope, verbatim — the browser sees exactly what crossed stdio.
			b, _ := json.Marshal(env)
			s.OnUpdate(b)
		}
	default:
		// Unknown notification: already captured in the event log; ignore.
	}
}

func (s *Session) handlePermissionRequest(env *Envelope) {
	var pr struct {
		Options []permOption `json:"options"`
	}
	_ = json.Unmarshal(env.Params, &pr)

	s.mu.Lock()
	s.nextReq++
	reqID := fmt.Sprintf("pr-%d", s.nextReq)
	s.perms[reqID] = &pendingPerm{rpcID: append(json.RawMessage(nil), env.ID...), options: pr.Options}
	s.mu.Unlock()

	if s.OnPermissionRequest != nil {
		b, _ := json.Marshal(env)
		s.OnPermissionRequest(reqID, b)
	}
}

// RespondPermission answers a pending permission request. explicitOptionID
// wins when non-empty; otherwise outcome maps onto the agent's offered kinds:
// approve → allow_once (fallback allow_always), approve_always → allow_always,
// reject → any reject_*.
func (s *Session) RespondPermission(requestID, outcome, explicitOptionID string) error {
	s.mu.Lock()
	p := s.perms[requestID]
	if p == nil {
		s.mu.Unlock()
		return fmt.Errorf("unknown permission request %q", requestID)
	}
	delete(s.perms, requestID)
	optionID := explicitOptionID
	if optionID == "" {
		optionID = resolveOutcome(outcome, p.options)
	}
	rpcID := append(json.RawMessage(nil), p.rpcID...)
	s.mu.Unlock()

	if optionID == "" {
		return fmt.Errorf("no option for outcome %q", outcome)
	}
	result, _ := json.Marshal(map[string]string{"optionId": optionID})
	return s.write(Envelope{JSONRPC: "2.0", ID: rpcID, Result: result})
}

func resolveOutcome(outcome string, options []permOption) string {
	preferred := map[string][]string{
		"approve":        {"allow_once", "allow_always"},
		"approve_always": {"allow_always", "allow_once"},
		"reject":         {"reject_once", "reject_always", "reject"},
	}[outcome]
	for _, want := range preferred {
		for _, o := range options {
			if o.Kind == want {
				return o.OptionID
			}
		}
	}
	for _, o := range options { // last resort: first offered option
		return o.OptionID
	}
	return ""
}

// Prompt submits a user prompt to the structured session and returns the
// channel on which the turn's final response will arrive (stopReason et al).
// Turns can run long — there is deliberately no timeout here.
func (s *Session) Prompt(text string) (<-chan *Envelope, error) {
	params := map[string]any{
		"sessionId": s.ACPSessionID,
		"prompt":    []map[string]any{{"type": "text", "text": text}},
	}
	ch, _, err := s.sendRequest("session/prompt", params)
	return ch, err
}

// Cancel asks the agent to stop the current turn (ACP cancel notification).
func (s *Session) Cancel() error {
	return s.write(Envelope{
		JSONRPC: "2.0",
		Method:  "session/cancel",
		Params:  mustJSON(map[string]string{"sessionId": s.ACPSessionID}),
	})
}

// Kill hard-stops the child process (the `signal kill` path).
func (s *Session) Kill() {
	s.mu.Lock()
	closed := s.closed
	s.closed = true
	s.mu.Unlock()
	if closed || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Kill()
}

func (s *Session) closeFiles() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	_ = s.w.Flush()
	_ = s.stdin.Close()
	if s.cap != nil {
		_ = s.cap.Close()
	}
}

// request sends a request and waits for its response up to timeout.
func (s *Session) request(method string, params any, timeout time.Duration) (*Envelope, error) {
	ch, cleanup, err := s.sendRequest(method, params)
	if err != nil {
		return nil, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		cleanup()
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

// notify sends a notification (no id, no reply expected).
func (s *Session) notify(method string, params any) error {
	return s.write(Envelope{JSONRPC: "2.0", Method: method, Params: mustJSON(params)})
}

// write serializes one frame out and captures it before the bytes hit the pipe.
func (s *Session) write(env Envelope) error {
	line, err := json.Marshal(env)
	if err != nil {
		return err
	}
	s.logFrame("c>a", line)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.w.Write(append(line, '\n')); err != nil {
		return err
	}
	return s.w.Flush()
}

// logFrame appends one wrapped frame to the event log. Capture happens BEFORE
// any interpretation, for every frame in both directions.
func (s *Session) logFrame(dir string, frame []byte) {
	if s.cap == nil {
		return
	}
	rec, _ := json.Marshal(struct {
		At    string          `json:"at"`
		Dir   string          `json:"dir"`
		Frame json.RawMessage `json:"frame"`
	}{time.Now().UTC().Format(time.RFC3339Nano), dir, json.RawMessage(frame)})
	_, _ = s.cap.Write(append(rec, '\n'))
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}
