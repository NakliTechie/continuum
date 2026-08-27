// Package server implements the relay's WebSocket server.
//
// Sessions are owned by the Server and outlive any single connection: each
// session's output is delivered to its current "subscriber" connection. On a
// client reconnect, the relay advertises live sessions (`sessions`) and the
// client re-attaches (`attach`) — the relay re-issues a fresh session token,
// replays buffered output, and resumes streaming (protocol 1.1).
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/NakliTechie/menagerie/relay-go/internal/acp"
	"github.com/NakliTechie/menagerie/relay-go/internal/config"
	"github.com/NakliTechie/menagerie/relay-go/internal/protocol"
	"github.com/NakliTechie/menagerie/relay-go/internal/pty"
	"github.com/NakliTechie/menagerie/relay-go/internal/shims"
	"github.com/NakliTechie/menagerie/relay-go/internal/tmux"
)

// RelayVersion is reported in the hello message.
const RelayVersion = "0.5.0"

type sessionEntry struct {
	token     string
	sess      *pty.Session // nil for a tmux session adopted-but-not-yet-attached
	acp       *acp.Session // non-nil for structured sessions (sess is nil then)
	agent     string
	startedAt time.Time
	pid       int
	tmuxName  string // non-empty when the agent runs inside a tmux session
	parent    string // protocol 1.3: parent session id (supervisor tree); "" for a root. Guarded by s.mu.

	subMu sync.Mutex
	sub   *conn // current subscriber connection (may be nil between reconnects)

	detMu      sync.Mutex
	recentText []byte // rolling recent output for the loop detector (capped)
	stalled    bool   // stalled event already fired; cleared on user input

	// Structured-session outbound path: bounded queue, drop-with-marker
	// (§C2) — a dropped-frames marker in the stream is honest, an OOM is not.
	outMu   sync.Mutex
	outbox  chan []byte // pre-marshaled menagerie frames, single pump consumer
	seq     int64       // monotonic across session_update/permission_request frames
	tail    [][]byte    // recently delivered frames, replayed on re-attach (capped)
	dropped int         // frames dropped since the last marker was queued
	closed  bool        // outbox closed; sending after this panics, so every send checks it

	// Latest instrument frames (config selectors + last turn's token usage),
	// kept outside the tail ring so a long session still restores them on
	// re-attach after the originals scrolled out. Guarded by outMu.
	lastConfig    []byte
	lastTurnUsage []byte
}

func (e *sessionEntry) setSub(cn *conn)   { e.subMu.Lock(); e.sub = cn; e.subMu.Unlock() }
func (e *sessionEntry) subscriber() *conn { e.subMu.Lock(); defer e.subMu.Unlock(); return e.sub }

// Server holds relay-wide state shared across connections.
type Server struct {
	cfg   *config.Config
	shims map[string]shims.Shim

	mu       sync.Mutex
	sessions map[string]*sessionEntry
}

// New builds a Server for the given config.
func New(cfg *config.Config) *Server {
	cmds := make(map[string]string, len(cfg.Agents))
	for name, a := range cfg.Agents {
		cmds[name] = a.Command
	}
	return &Server{
		cfg:      cfg,
		shims:    shims.NewRegistry(cmds),
		sessions: make(map[string]*sessionEntry),
	}
}

// Handler returns the relay's HTTP handler (a single WebSocket endpoint).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleWS)
	return mux
}

func (s *Server) addSession(e *sessionEntry, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = e
}

func (s *Server) entry(id string) *sessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// entrySess reads e.sess under s.mu — it's written under that lock in handleAttach
// (tmux adopt), so every read takes the same lock to stay race-free.
func (s *Server) entrySess(e *sessionEntry) *pty.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return e.sess
}

// authSession returns the session iff the token matches (constant-time).
func (s *Server) authSession(id, token string) *sessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.sessions[id]
	if e == nil {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(e.token), []byte(token)) != 1 {
		return nil
	}
	return e
}

// getSession returns the PTY session iff the token matches (constant-time).
func (s *Server) getSession(id, token string) (*pty.Session, bool) {
	e := s.authSession(id, token)
	if e == nil {
		return nil, false
	}
	sess := s.entrySess(e)
	return sess, sess != nil
}

func (s *Server) reissueToken(id, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.sessions[id]; e != nil {
		e.token = token
	}
}

func (s *Server) removeSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// listSessions snapshots the live sessions for the `sessions` message.
func (s *Server) listSessions() []protocol.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]protocol.SessionInfo, 0, len(s.sessions))
	for id, e := range s.sessions {
		transport := protocol.TransportPTY
		if e.acp != nil {
			transport = protocol.TransportACP
		}
		out = append(out, protocol.SessionInfo{SessionID: id, Agent: e.agent, StartedAt: e.startedAt.UTC().Format(time.RFC3339), PID: e.pid, Transport: transport, ParentSessionID: e.parent})
	}
	return out
}

// useTmux reports whether new agents should run inside tmux (so they survive a
// relay restart). "auto" uses tmux when it's installed.
func (s *Server) useTmux() bool {
	switch s.cfg.Tmux {
	case "on":
		return true
	case "off":
		return false
	default:
		return tmux.Available()
	}
}

// reconcileTmux adopts tmux sessions not already tracked. Menagerie's own
// `menagerie-*` sessions are always adopted (this is what re-surfaces agents
// after the relay itself restarts); sessions you started yourself are adopted
// only when adopt_foreign_tmux is on. Adopted entries are lazy (no PTY) until a
// client attaches.
func (s *Server) reconcileTmux() {
	if !s.useTmux() {
		return
	}
	for _, ts := range tmux.List() {
		var id, agent string
		if mid, ok := tmux.IDFromName(ts.Name); ok {
			id, agent = mid, ts.Agent
			if agent == "" {
				agent = "tmux"
			}
		} else if s.cfg.AdoptForeignTmux {
			id, agent = "ext-"+sanitizeID(ts.Name), ts.Name // show the session name as the agent
		} else {
			continue // a foreign session, and adoption is off
		}
		s.mu.Lock()
		if s.sessions[id] == nil {
			s.sessions[id] = &sessionEntry{agent: agent, startedAt: ts.Created, tmuxName: ts.Name}
			log.Printf("adopted tmux session %s (name=%s agent=%s)", id, ts.Name, agent)
		}
		s.mu.Unlock()
	}
}

// sanitizeID makes a tmux session name safe to use as a session id (map key +
// FSA capture filename): only [A-Za-z0-9_-] survive.
func sanitizeID(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// runSession pumps a session's PTY output (+ generic activity heuristics) to its
// subscriber and handles exit. Shared by fresh spawns and adopted attaches.
func (s *Server) runSession(id string, sess *pty.Session) {
	go sess.Run(
		func(seq int, b []byte) {
			s.deliverOutput(id, seq, b)
			if shims.LooksLikeNeedsInput(b) {
				s.deliverEvent(id, protocol.EventNeedsInput, nil)
			} else if shims.LooksLikeRateLimited(b) {
				s.deliverEvent(id, protocol.EventRateLimited, nil)
			}
			s.checkStall(id, b)
		},
		func(code int) {
			c := code
			s.deliverEvent(id, protocol.EventExited, &c)
			s.removeSession(id)
			log.Printf("exited %s (code=%d)", id, code)
		},
	)
}

// checkStall feeds output into the per-session loop detector and fires a single
// `stalled` event when recent output starts repeating (a likely loop). Unlike the
// other heuristics it isn't cleared by more output — a loop keeps printing — so it
// stays until the client sends input (handleInput resets it) or the session exits.
func (s *Server) checkStall(id string, chunk []byte) {
	const maxRecent = 8 * 1024
	e := s.entry(id)
	if e == nil {
		return
	}
	e.detMu.Lock()
	e.recentText = append(e.recentText, chunk...)
	if len(e.recentText) > maxRecent {
		e.recentText = e.recentText[len(e.recentText)-maxRecent:]
	}
	fire := false
	if !e.stalled && shims.LooksLikeStalled(strings.Split(string(e.recentText), "\n")) {
		e.stalled, fire = true, true
	}
	e.detMu.Unlock()
	if fire {
		s.deliverEvent(id, protocol.EventStalled, nil)
		log.Printf("stalled %s (repeating output)", id)
	}
}

// detach clears any session subscription pointing at a closing connection.
func (s *Server) detach(cn *conn) {
	s.mu.Lock()
	entries := make([]*sessionEntry, 0, len(s.sessions))
	for _, e := range s.sessions {
		entries = append(entries, e)
	}
	s.mu.Unlock()
	for _, e := range entries {
		e.subMu.Lock()
		if e.sub == cn {
			e.sub = nil
		}
		e.subMu.Unlock()
	}
}

// deliverOutput routes a session's output to its current subscriber.
func (s *Server) deliverOutput(id string, seq int, b []byte) {
	e := s.entry(id)
	if e == nil {
		return
	}
	if sub := e.subscriber(); sub != nil {
		_ = sub.send(protocol.Output{Type: protocol.TypeOutput, SessionID: id, Data: base64.StdEncoding.EncodeToString(b), Seq: seq})
	}
}

// validParent returns the parent id to record for a NEW session: the requested
// parent iff it's a live session on this relay, else "" (spawn at root — never fail
// a spawn over a stale parent, per spec §2). No cycle check is needed: the new
// session's id doesn't exist yet, so it cannot already be an ancestor of any live
// session; cycles are only reachable via re-parenting, which the protocol has no frame for.
func (s *Server) validParent(parentID string) string {
	if parentID == "" {
		return ""
	}
	s.mu.Lock()
	_, live := s.sessions[parentID]
	s.mu.Unlock()
	if !live {
		log.Printf("spawn: parent %s not live; spawning at root", parentID)
		return ""
	}
	return parentID
}

// emitChildSpawned tells the PARENT's owner a child was spawned. Routed to whichever
// transport the parent uses (PTY → direct subscriber send; ACP → the outbox funnel).
func (s *Server) emitChildSpawned(parentID, childID string) {
	if parentID == "" {
		return
	}
	e := s.entry(parentID)
	if e == nil {
		return
	}
	frame := protocol.Event{Type: protocol.TypeEvent, SessionID: parentID, Event: protocol.EventChildSpawned, ChildSessionID: childID, At: time.Now().UTC().Format(time.RFC3339)}
	if e.acp != nil {
		if b, err := json.Marshal(frame); err == nil {
			_, _ = e.trySend(b)
		}
		return
	}
	if sub := e.subscriber(); sub != nil {
		_ = sub.send(frame)
	}
}

// subtreeLeafFirst returns rootID plus every descendant, deepest first, so a kill
// tears down leaves before their parents. Snapshotted once under s.mu (later
// removals during the kill loop don't matter — we already hold the id list).
func (s *Server) subtreeLeafFirst(rootID string) []string {
	s.mu.Lock()
	children := map[string][]string{}
	for id, e := range s.sessions {
		if e.parent != "" {
			children[e.parent] = append(children[e.parent], id)
		}
	}
	s.mu.Unlock()
	var order []string
	seen := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		if seen[id] {
			return // defensive: a pathological cycle can't stall the walk
		}
		seen[id] = true
		for _, c := range children[id] {
			walk(c)
		}
		order = append(order, id) // post-order → children before their parent
	}
	walk(rootID)
	return order
}

// killEntry hard-stops one session by whichever transport it runs on. Each kill
// drives that session's own exit path (an `exited` event + cleanup).
func (s *Server) killEntry(id string) {
	e := s.entry(id)
	if e == nil {
		return
	}
	if e.acp != nil {
		e.acp.Kill()
		return
	}
	if e.tmuxName != "" {
		_ = tmux.Kill(e.tmuxName)
	}
	if sess := s.entrySess(e); sess != nil {
		sess.Kill()
	}
}

// deliverEvent routes a lifecycle event to a session's current subscriber.
func (s *Server) deliverEvent(id, event string, code *int) {
	e := s.entry(id)
	if e == nil {
		return
	}
	if sub := e.subscriber(); sub != nil {
		_ = sub.send(protocol.Event{Type: protocol.TypeEvent, SessionID: id, Event: event, ExitCode: code, At: time.Now().UTC().Format(time.RFC3339)})
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// A plain browser visit (no Upgrade header) gets a friendly info page,
	// not a cryptic WebSocket-protocol error.
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		s.serveInfo(w)
		return
	}
	// Origin check is the gate (§8). Browsers always send Origin; "null" must be
	// opted in (it also matches sandboxed iframes). Empty Origin = non-browser
	// client (agent face, §16) → allowed to proceed to token-based register.
	origin := r.Header.Get("Origin")
	if origin != "" && !s.cfg.OriginAllowed(origin) {
		log.Printf("rejected upgrade from disallowed origin %q", origin)
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // we enforce the origin allowlist ourselves above
	})
	if err != nil {
		log.Printf("ws accept: %v", err)
		return
	}
	defer c.CloseNow()

	cn := &conn{srv: s, ws: c, ctx: r.Context()}
	cn.serve()
	s.detach(cn)
}

// conn is one browser<->relay WebSocket connection. All writes go through
// send(), which serializes them (session output goroutines write concurrently
// with the read loop, and a connection can subscribe to multiple sessions).
type conn struct {
	srv        *Server
	ws         *websocket.Conn
	ctx        context.Context
	registered bool
	writeMu    sync.Mutex
}

func (cn *conn) send(v any) error {
	cn.writeMu.Lock()
	defer cn.writeMu.Unlock()
	return wsjson.Write(cn.ctx, cn.ws, v)
}

func (cn *conn) sendError(sessionID, code, message string) {
	_ = cn.send(protocol.NewError(sessionID, code, message))
}

// sendRaw writes a pre-marshaled JSON frame verbatim (structured-session path).
func (cn *conn) sendRaw(b []byte) error {
	cn.writeMu.Lock()
	defer cn.writeMu.Unlock()
	return cn.ws.Write(cn.ctx, websocket.MessageText, b)
}

func (cn *conn) serve() {
	if err := cn.send(cn.srv.hello()); err != nil {
		return
	}
	for {
		var raw json.RawMessage
		if err := wsjson.Read(cn.ctx, cn.ws, &raw); err != nil {
			return // connection closed or context done
		}
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			cn.sendError("", "bad_message", "malformed message")
			continue
		}
		cn.dispatch(env, raw)
	}
}

// hello builds the relay's greeting: capabilities, agents, and — protocol 1.2 —
// which spawn form each agent supports.
func (s *Server) hello() protocol.Hello {
	set := map[string]bool{"pty": true}
	agentTransports := make(map[string][]string, len(s.cfg.Agents))
	for name, ag := range s.cfg.Agents {
		ts := ag.TransportsOrDefault()
		agentTransports[name] = ts
		for _, t := range ts {
			set[t] = true
		}
	}
	transports := make([]string, 0, len(set))
	for t := range set {
		transports = append(transports, t)
	}
	sort.Strings(transports)
	return protocol.Hello{
		Type:            protocol.TypeHello,
		ProtocolVersion: protocol.Version,
		RelayVersion:    RelayVersion,
		RelayName:       s.cfg.Name,
		HostOS:          runtime.GOOS,
		HostArch:        runtime.GOARCH,
		Agents:          s.cfg.AgentNames(),
		Transports:      transports,
		HostsChildren:   true, // protocol 1.3: parentage + subtree kill
		AgentTransports: agentTransports,
	}
}

func (cn *conn) dispatch(env protocol.Envelope, raw json.RawMessage) {
	if env.Type == protocol.TypeRegister {
		cn.handleRegister(raw)
		return
	}
	if !cn.registered {
		cn.sendError(env.SessionID, protocol.ErrAuthFailed, "register before sending "+env.Type)
		return
	}
	switch env.Type {
	case protocol.TypeSpawn:
		cn.handleSpawn(raw)
	case protocol.TypeAttach:
		cn.handleAttach(raw)
	case protocol.TypeInput:
		cn.handleInput(raw)
	case protocol.TypeSignal:
		cn.handleSignal(raw)
	case protocol.TypePrompt:
		cn.handlePrompt(raw)
	case protocol.TypePermissionResponse:
		cn.handlePermissionResponse(raw)
	case protocol.TypeResume:
		// Superseded by sessions+attach (1.1); keep a graceful answer for old clients.
		var msg protocol.Resume
		_ = json.Unmarshal(raw, &msg)
		_ = cn.send(protocol.ResumeFailed{Type: protocol.TypeResumeFailed, SessionID: msg.SessionID})
	default:
		cn.sendError(env.SessionID, "bad_message", "unknown message type: "+env.Type)
	}
}

func (cn *conn) handleRegister(raw json.RawMessage) {
	var msg protocol.Register
	if err := json.Unmarshal(raw, &msg); err != nil {
		cn.sendError("", "bad_message", "malformed register")
		return
	}
	want := cn.srv.cfg.RegistrationToken
	if want == "" || subtle.ConstantTimeCompare([]byte(msg.RegistrationToken), []byte(want)) != 1 {
		cn.sendError("", protocol.ErrAuthFailed, "registration token rejected")
		_ = cn.ws.Close(websocket.StatusPolicyViolation, "auth_failed")
		return
	}
	cn.registered = true
	_ = cn.send(protocol.Registered{Type: protocol.TypeRegistered})
	// Re-adopt any tmux-backed agents that outlived a relay restart, then advertise
	// all live sessions so the client re-attaches (1.1).
	cn.srv.reconcileTmux()
	_ = cn.send(protocol.Sessions{Type: protocol.TypeSessions, Sessions: cn.srv.listSessions()})
	log.Printf("client registered")
}

func (cn *conn) handleSpawn(raw json.RawMessage) {
	var msg protocol.Spawn
	if err := json.Unmarshal(raw, &msg); err != nil {
		cn.sendError("", "bad_message", "malformed spawn")
		return
	}
	transport := msg.Transport
	if transport == "" {
		transport = protocol.TransportPTY // absent ⇒ pty, so stored definitions keep working
	}
	switch {
	case transport == protocol.TransportACP:
		cn.handleSpawnACP(msg)
	case transport == protocol.TransportPTY:
		cn.handleSpawnPTY(msg)
	default:
		// Never silently downgrade an unknown transport (handoff hard NOT #1's spirit).
		cn.sendError("", "unsupported_transport", "relay cannot honor transport "+transport)
	}
}

func (cn *conn) handleSpawnPTY(msg protocol.Spawn) {
	shim, ok := cn.srv.shims[msg.Agent]
	if !ok {
		cn.sendError("", protocol.ErrUnknownAgent, "no shim for agent: "+msg.Agent)
		return
	}
	cmd, err := shim.Spawn(msg.Cwd, msg.Args, msg.Env)
	if err != nil {
		cn.sendError("", protocol.ErrSpawnFailed, err.Error())
		return
	}
	id, err := randomID()
	if err != nil {
		cn.sendError("", protocol.ErrSpawnFailed, "id generation failed")
		return
	}
	s := cn.srv

	// When tmux is in play the agent runs inside a detached tmux session (so it
	// outlives a relay restart) and our PTY just attaches to it.
	tmuxName := ""
	if s.useTmux() {
		tmuxName = tmux.SessionName(id)
		if err := tmux.Create(tmuxName, cmd.Dir, cmd.Env, cmd.Args); err != nil {
			cn.sendError("", protocol.ErrSpawnFailed, "tmux create: "+err.Error())
			return
		}
		tmux.SetAgent(tmuxName, msg.Agent)
		cmd = tmux.AttachCmd(tmuxName)
	}

	sess, err := pty.Start(id, msg.Agent, cmd)
	if err != nil {
		if tmuxName != "" {
			_ = tmux.Kill(tmuxName)
		}
		cn.sendError("", protocol.ErrSpawnFailed, err.Error())
		return
	}
	token, err := config.GenerateToken()
	if err != nil {
		sess.Kill()
		if tmuxName != "" {
			_ = tmux.Kill(tmuxName)
		}
		cn.sendError("", protocol.ErrSpawnFailed, "session token generation failed")
		return
	}
	parent := s.validParent(msg.ParentSessionID)
	s.addSession(&sessionEntry{token: token, sess: sess, agent: msg.Agent, startedAt: sess.StartedAt, pid: sess.PID, sub: cn, tmuxName: tmuxName, parent: parent}, id)
	_ = cn.send(protocol.Spawned{
		Type:            protocol.TypeSpawned,
		SessionID:       id,
		ClientID:        msg.ClientID,
		SessionToken:    token,
		Agent:           msg.Agent,
		PID:             sess.PID,
		StartedAt:       sess.StartedAt.UTC().Format(time.RFC3339),
		ParentSessionID: parent,
	})
	s.emitChildSpawned(parent, id)
	log.Printf("spawned %s (agent=%s pid=%d tmux=%q parent=%q)", id, msg.Agent, sess.PID, tmuxName, parent)
	s.runSession(id, sess)
}

// handleSpawnACP starts a structured session: the agent child speaks the pinned
// ACP over stdio; JSON-RPC frames bridge to the WebSocket as opaque menagerie
// frames. Deliberately NOT tmux-wrapped — a PTY would mangle the framing.
func (cn *conn) handleSpawnACP(msg protocol.Spawn) {
	s := cn.srv
	ag, ok := s.cfg.Agents[msg.Agent]
	if !ok {
		cn.sendError("", protocol.ErrUnknownAgent, "no agent configured: "+msg.Agent)
		return
	}
	if !ag.SupportsACP() {
		cn.sendError("", "unsupported_transport", "agent "+msg.Agent+" does not speak acp")
		return
	}
	cmd := exec.Command(ag.Command, append(ag.ACPArgsOrDefault(), msg.Args...)...)
	cmd.Dir = msg.Cwd
	cmd.Env = shims.MergeEnv(msg.Env)

	id, err := randomID()
	if err != nil {
		cn.sendError("", protocol.ErrSpawnFailed, "id generation failed")
		return
	}
	sess, err := acp.Start(id, msg.Agent, msg.Cwd, cmd)
	if err != nil {
		cn.sendError("", protocol.ErrSpawnFailed, err.Error())
		return
	}
	token, err := config.GenerateToken()
	if err != nil {
		sess.Kill()
		cn.sendError("", protocol.ErrSpawnFailed, "session token generation failed")
		return
	}
	parent := s.validParent(msg.ParentSessionID)
	e := &sessionEntry{
		token:     token,
		acp:       sess,
		agent:     msg.Agent,
		startedAt: sess.StartedAt,
		pid:       sess.PID,
		sub:       cn,
		outbox:    make(chan []byte, outboxCapacity),
		parent:    parent,
	}
	s.addSession(e, id)

	sess.OnUpdate = func(params json.RawMessage) { s.deliverStructured(id, params) }
	sess.OnPermissionRequest = func(reqID string, params json.RawMessage) {
		s.deliverPermissionRequest(id, reqID, params)
		s.queueStructuredEvent(e, id, protocol.EventNeedsInput, nil)
	}

	_ = cn.send(protocol.Spawned{
		Type:            protocol.TypeSpawned,
		SessionID:       id,
		ClientID:        msg.ClientID,
		SessionToken:    token,
		Agent:           msg.Agent,
		PID:             sess.PID,
		StartedAt:       sess.StartedAt.UTC().Format(time.RFC3339),
		ParentSessionID: parent,
	})
	log.Printf("spawned %s (agent=%s pid=%d transport=acp parent=%q)", id, msg.Agent, sess.PID, parent)

	go s.pumpStructured(e)
	s.emitChildSpawned(parent, id)

	// Re-surface the session/new config selectors (model / mode / thought_level)
	// to the browser through the session_update funnel — agents deliver them in
	// the response, which the browser never sees. Kept on the entry so re-attach
	// restores them even after they scroll out of the tail ring.
	if len(sess.InitConfig) > 0 {
		frame := configOptionUpdate(id, sess.InitConfig)
		e.outMu.Lock()
		e.lastConfig = frame
		e.outMu.Unlock()
		s.deliverStructured(id, frame)
	}

	go func() {
		sess.Run(func(code int) {
			c := code
			s.queueStructuredEvent(e, id, protocol.EventExited, &c)
			s.removeSession(id)
			e.closeOutbox() // ends the pump; guarded so late sends drop, not panic
			log.Printf("exited %s (code=%d)", id, code)
		})
	}()
}

// queueStructuredEvent routes a lifecycle event through the session's outbound
// queue so it never overtakes the frames that caused it.
func (s *Server) queueStructuredEvent(e *sessionEntry, id, event string, code *int) {
	b, _ := json.Marshal(protocol.Event{Type: protocol.TypeEvent, SessionID: id, Event: event, ExitCode: code, At: time.Now().UTC().Format(time.RFC3339)})
	_, _ = e.trySend(b)
}

const (
	outboxCapacity = 384 // ≥ tailCapacity so a re-attach replay always fits
	tailCapacity   = 256
)

// pumpStructured is the single consumer of a session's outbound queue.
func (s *Server) pumpStructured(e *sessionEntry) {
	for b := range e.outbox {
		if sub := e.subscriber(); sub != nil {
			if err := sub.sendRaw(b); err != nil {
				log.Printf("structured send %s: %v", e.agent, err)
			}
		}
	}
}

func (e *sessionEntry) nextSeq() int64 {
	e.outMu.Lock()
	defer e.outMu.Unlock()
	e.seq++
	return e.seq
}

// configOptionUpdate builds a synthetic `session/update` envelope carrying the
// agent's own `configOptions` (from the session/new result) as a real ACP
// `config_option_update`. Re-surfacing, not fabrication: the array is verbatim
// from the agent; only the delivery channel changes (response → notification).
func configOptionUpdate(sessionID string, configOptions json.RawMessage) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sessionID,
			"update": map[string]any{
				"sessionUpdate": "config_option_update",
				"configOptions": configOptions,
			},
		},
	})
	return b
}

// turnUsageUpdate re-surfaces the prompt result's `usage` (per-turn token
// counts) through the funnel. ACP has no notification for turn usage, so this
// rides a menagerie-namespaced update kind (`_menagerie/turn_usage`); clients
// that do not know it render it dim, per ACP's `_`-prefix extension rule.
func turnUsageUpdate(sessionID string, usage json.RawMessage) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sessionID,
			"update": map[string]any{
				"sessionUpdate": "_menagerie/turn_usage",
				"usage":         usage,
			},
		},
	})
	return b
}

// deliverStructured wraps one ACP notification into a session_update frame:
// capture to the re-attach tail first, then queue for delivery; drop honestly
// when a slow socket outruns the bound.
func (s *Server) deliverStructured(id string, params json.RawMessage) {
	e := s.entry(id)
	if e == nil || e.acp == nil {
		return
	}
	frame := protocol.SessionUpdate{Type: protocol.TypeSessionUpdate, SessionID: id, Seq: int(e.nextSeq()), Acp: params}
	b, err := json.Marshal(frame)
	if err != nil {
		return
	}
	e.appendTail(b)
	if sent, closed := e.trySend(b); !sent && !closed {
		s.dropStructured(e, id)
	}
}

func (s *Server) deliverPermissionRequest(id, requestID string, params json.RawMessage) {
	e := s.entry(id)
	if e == nil || e.acp == nil {
		return
	}
	frame := protocol.PermissionRequest{Type: protocol.TypePermissionRequest, SessionID: id, RequestID: requestID, Seq: int(e.nextSeq()), Acp: params}
	b, err := json.Marshal(frame)
	if err != nil {
		return
	}
	e.appendTail(b)
	if sent, closed := e.trySend(b); !sent && !closed {
		s.dropStructured(e, id)
	}
}

func (s *Server) dropStructured(e *sessionEntry, id string) {
	e.outMu.Lock()
	e.dropped++
	n := e.dropped
	e.outMu.Unlock()
	marker, _ := json.Marshal(protocol.NewError(id, "frames_dropped", fmt.Sprintf("%d structured frame(s) dropped by backpressure", n)))
	_, _ = e.trySend(marker)
}

// trySend queues one frame on the outbox under outMu. It NEVER sends on a closed
// outbox (that panics), so a frame produced by the ACP reader goroutine racing the
// child's exit is dropped, not fatal. Returns (sent, closed): !sent && !closed
// means backpressure drop; closed means the session is gone.
func (e *sessionEntry) trySend(b []byte) (sent, closed bool) {
	e.outMu.Lock()
	defer e.outMu.Unlock()
	if e.closed {
		return false, true
	}
	select {
	case e.outbox <- b:
		return true, false
	default:
		return false, false
	}
}

// closeOutbox ends the pump exactly once, under the same lock every sender takes,
// so no send can be in flight when the channel closes.
func (e *sessionEntry) closeOutbox() {
	e.outMu.Lock()
	defer e.outMu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	close(e.outbox)
}

// stampReplaySeq rewrites a frame's seq to -1 for re-attach replay — any frame
// type carrying a seq (session_update, permission_request). Returns b unchanged
// if it doesn't parse or has no seq.
func stampReplaySeq(b []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return b
	}
	if _, ok := m["seq"]; !ok {
		return b
	}
	m["seq"] = json.RawMessage("-1")
	if nb, err := json.Marshal(m); err == nil {
		return nb
	}
	return b
}

func (e *sessionEntry) appendTail(b []byte) {
	e.outMu.Lock()
	defer e.outMu.Unlock()
	cp := make([]byte, len(b))
	copy(cp, b)
	e.tail = append(e.tail, cp)
	if len(e.tail) > tailCapacity {
		e.tail = e.tail[len(e.tail)-tailCapacity:]
	}
}

// tailSnapshot returns copies of the recent frames for re-attach replay.
func (e *sessionEntry) tailSnapshot() [][]byte {
	e.outMu.Lock()
	defer e.outMu.Unlock()
	out := make([][]byte, len(e.tail))
	for i, b := range e.tail {
		out[i] = append([]byte(nil), b...)
	}
	return out
}

func (cn *conn) handlePrompt(raw json.RawMessage) {
	var msg protocol.Prompt
	if err := json.Unmarshal(raw, &msg); err != nil {
		cn.sendError(msg.SessionID, "bad_message", "malformed prompt")
		return
	}
	e := cn.srv.authSession(msg.SessionID, msg.SessionToken)
	if e == nil {
		cn.sendError(msg.SessionID, protocol.ErrInvalidToken, "unknown session or bad token")
		return
	}
	if e.acp == nil {
		cn.sendError(msg.SessionID, "bad_message", "prompt applies to structured sessions only")
		return
	}
	ch, err := e.acp.Prompt(msg.Text)
	if err != nil {
		cn.sendError(msg.SessionID, "bad_message", err.Error())
		return
	}
	id := msg.SessionID
	go func() {
		resp := <-ch
		stop := ""
		var r struct {
			StopReason string          `json:"stopReason"`
			Usage      json.RawMessage `json:"usage"`
		}
		if resp != nil && resp.Result != nil {
			_ = json.Unmarshal(resp.Result, &r)
			stop = r.StopReason
		}
		log.Printf("prompt done %s (stopReason=%q)", id, stop)
		if e := cn.srv.entry(id); e != nil && e.acp != nil {
			// Turn token usage arrives in the prompt result, not a notification —
			// re-surface it through the funnel before the idle event so the
			// instrument bar's token counters never overtake turn-end.
			if len(r.Usage) > 0 && string(r.Usage) != "null" {
				frame := turnUsageUpdate(id, r.Usage)
				e.outMu.Lock()
				e.lastTurnUsage = frame
				e.outMu.Unlock()
				cn.srv.deliverStructured(id, frame)
			}
			cn.srv.queueStructuredEvent(e, id, protocol.EventIdle, nil)
		}
	}()
}

func (cn *conn) handlePermissionResponse(raw json.RawMessage) {
	var msg protocol.PermissionResponse
	if err := json.Unmarshal(raw, &msg); err != nil {
		cn.sendError(msg.SessionID, "bad_message", "malformed permission_response")
		return
	}
	e := cn.srv.authSession(msg.SessionID, msg.SessionToken)
	if e == nil {
		cn.sendError(msg.SessionID, protocol.ErrInvalidToken, "unknown session or bad token")
		return
	}
	if e.acp == nil {
		cn.sendError(msg.SessionID, "bad_message", "permission_response applies to structured sessions only")
		return
	}
	if err := e.acp.RespondPermission(msg.RequestID, msg.Outcome, msg.OptionID); err != nil {
		cn.sendError(msg.SessionID, "unknown_request", err.Error())
	}
}

func (cn *conn) handleAttach(raw json.RawMessage) {
	var msg protocol.Attach
	if err := json.Unmarshal(raw, &msg); err != nil {
		cn.sendError("", "bad_message", "malformed attach")
		return
	}
	e := cn.srv.entry(msg.SessionID)
	if e == nil {
		// Session is gone (relay restarted, or it exited) — client falls back to FSA replay.
		_ = cn.send(protocol.ResumeFailed{Type: protocol.TypeResumeFailed, SessionID: msg.SessionID})
		return
	}

	if e.acp != nil {
		tok, err := config.GenerateToken()
		if err != nil {
			cn.sendError(msg.SessionID, protocol.ErrSpawnFailed, "session token generation failed")
			return
		}
		cn.srv.reissueToken(msg.SessionID, tok)
		e.setSub(cn)
		_ = cn.send(protocol.Attached{
			Type:         protocol.TypeAttached,
			SessionID:    msg.SessionID,
			SessionToken: tok,
			Agent:        e.agent,
			StartedAt:    e.startedAt.UTC().Format(time.RFC3339),
			PID:          e.pid,
		})
		// Instrument state first, at seq -1, so model/mode/usage restore even if
		// the original frames scrolled out of the tail ring on a long session.
		e.outMu.Lock()
		instr := [][]byte{e.lastConfig, e.lastTurnUsage}
		e.outMu.Unlock()
		for _, params := range instr {
			if params == nil {
				continue
			}
			frame, err := json.Marshal(protocol.SessionUpdate{Type: protocol.TypeSessionUpdate, SessionID: msg.SessionID, Seq: -1, Acp: params})
			if err != nil {
				continue
			}
			_, _ = e.trySend(frame)
		}
	replay:
		for _, b := range e.tailSnapshot() {
			// Replayed history carries seq -1 (the PTY-attach convention): clients
			// render it but never re-persist it. Stamp every frame that has a seq —
			// session_update AND permission_request — so a replayed prompt isn't
			// mistaken for a fresh, actionable one.
			b = stampReplaySeq(b)
			sent, closed := e.trySend(b)
			if closed {
				break replay
			}
			if !sent {
				cn.srv.dropStructured(e, msg.SessionID) // queue full; the marker says so
				break replay // stop replaying once we've dropped; don't spam the counter
			}
		}
		log.Printf("reattached %s (structured)", msg.SessionID)
		return
	}

	// A tmux session adopted after a relay restart has no PTY yet — attach one now.
	// tmux redraws the pane on attach, so the client sees current state immediately.
	if cn.srv.entrySess(e) == nil {
		if e.tmuxName == "" || !tmux.Exists(e.tmuxName) {
			cn.srv.removeSession(msg.SessionID)
			_ = cn.send(protocol.ResumeFailed{Type: protocol.TypeResumeFailed, SessionID: msg.SessionID})
			return
		}
		sess, err := pty.Start(msg.SessionID, e.agent, tmux.AttachCmd(e.tmuxName))
		if err != nil {
			cn.sendError(msg.SessionID, protocol.ErrSpawnFailed, err.Error())
			return
		}
		cn.srv.mu.Lock()
		if e.sess == nil {
			e.sess = sess
			e.pid = sess.PID
			cn.srv.mu.Unlock()
			cn.srv.runSession(msg.SessionID, sess)
		} else {
			cn.srv.mu.Unlock()
			sess.Kill() // lost a race with a concurrent attach — keep the first PTY
		}
	}
	tok, err := config.GenerateToken()
	if err != nil {
		cn.sendError(msg.SessionID, protocol.ErrSpawnFailed, "session token generation failed")
		return
	}
	cn.srv.reissueToken(msg.SessionID, tok)
	e.setSub(cn)
	_ = cn.send(protocol.Attached{
		Type:         protocol.TypeAttached,
		SessionID:    msg.SessionID,
		SessionToken: tok,
		Agent:        e.agent,
		StartedAt:    e.startedAt.UTC().Format(time.RFC3339),
		PID:          e.pid,
	})
	if sess := cn.srv.entrySess(e); sess != nil {
		if buf := sess.Buffer(); len(buf) > 0 {
			_ = cn.send(protocol.Output{Type: protocol.TypeOutput, SessionID: msg.SessionID, Data: base64.StdEncoding.EncodeToString(buf), Seq: -1})
		}
	}
	log.Printf("reattached %s", msg.SessionID)
}

func (cn *conn) handleInput(raw json.RawMessage) {
	var msg protocol.Input
	if err := json.Unmarshal(raw, &msg); err != nil {
		cn.sendError("", "bad_message", "malformed input")
		return
	}
	sess, ok := cn.srv.getSession(msg.SessionID, msg.SessionToken)
	if !ok {
		if e := cn.srv.authSession(msg.SessionID, msg.SessionToken); e != nil && e.acp != nil {
			cn.sendError(msg.SessionID, "bad_message", "input applies to PTY sessions only; use prompt")
		} else {
			cn.sendError(msg.SessionID, protocol.ErrInvalidToken, "unknown session or bad token")
		}
		return
	}
	// User acted — clear any stall flag so a fresh loop can be detected later.
	if e := cn.srv.entry(msg.SessionID); e != nil {
		e.detMu.Lock()
		e.stalled = false
		e.recentText = nil
		e.detMu.Unlock()
	}
	if err := sess.Write([]byte(msg.Data)); err != nil {
		log.Printf("input write %s: %v", msg.SessionID, err)
	}
}

func (cn *conn) handleSignal(raw json.RawMessage) {
	var msg protocol.Signal
	if err := json.Unmarshal(raw, &msg); err != nil {
		cn.sendError("", "bad_message", "malformed signal")
		return
	}
	e := cn.srv.authSession(msg.SessionID, msg.SessionToken)
	if e == nil {
		cn.sendError(msg.SessionID, protocol.ErrInvalidToken, "unknown session or bad token")
		return
	}
	// protocol 1.3: kill + subtree tears down this session and every descendant,
	// leaf-first. Authorized by THIS session's token (the parent owns the subtree).
	if msg.Signal == protocol.SignalKill && msg.Subtree {
		ids := cn.srv.subtreeLeafFirst(msg.SessionID)
		for _, id := range ids {
			cn.srv.killEntry(id)
		}
		log.Printf("subtree kill %s → %d session(s)", msg.SessionID, len(ids))
		return
	}
	if e.acp != nil {
		switch msg.Signal {
		case protocol.SignalKill:
			e.acp.Kill()
		case protocol.SignalInterrupt:
			// Cancellation maps to ACP's cancel, not to a signal; kill stays the hard stop.
			if err := e.acp.Cancel(); err != nil {
				cn.sendError(msg.SessionID, "bad_message", "cancel failed: "+err.Error())
			}
		case protocol.SignalResize:
			// No terminal geometry in a structured session.
		default:
			cn.sendError(msg.SessionID, "bad_message", "unknown signal: "+msg.Signal)
		}
		return
	}
	sess := cn.srv.entrySess(e)
	if sess == nil {
		cn.sendError(msg.SessionID, protocol.ErrInvalidToken, "session has no live PTY")
		return
	}
	inTmux := e.tmuxName != ""
	switch msg.Signal {
	case protocol.SignalKill:
		if inTmux {
			_ = tmux.Kill(e.tmuxName) // ends the agent + its tmux session; the PTY then EOFs
		}
		sess.Kill()
	case protocol.SignalInterrupt:
		if inTmux {
			_ = sess.Write([]byte{0x03}) // ^C through the attach PTY — SIGINT to the client would only detach
		} else {
			sess.Interrupt()
		}
	case protocol.SignalResize:
		_ = sess.Resize(msg.Cols, msg.Rows)
	default:
		cn.sendError(msg.SessionID, "bad_message", "unknown signal: "+msg.Signal)
	}
}

// serveInfo answers a non-WebSocket request (e.g. a browser visiting the relay
// URL) with a friendly plaintext page instead of a protocol error.
func (s *Server) serveInfo(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "Menagerie relay %q\nprotocol v%s · relay v%s · host %s/%s\n\n"+
		"This is a WebSocket endpoint, not a web page.\n"+
		"To use it: open the Menagerie app, Settings -> Add relay, paste this URL\n"+
		"plus the registration token from `menagerie-relay token print`.\n",
		s.cfg.Name, protocol.Version, RelayVersion, runtime.GOOS, runtime.GOARCH)
}

// randomID returns a short hex session id.
func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err // never return an all-zero (predictable/colliding) id
	}
	return hex.EncodeToString(b), nil
}
