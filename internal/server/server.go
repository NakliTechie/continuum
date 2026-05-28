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
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/NakliTechie/menagerie/relay-go/internal/config"
	"github.com/NakliTechie/menagerie/relay-go/internal/protocol"
	"github.com/NakliTechie/menagerie/relay-go/internal/pty"
	"github.com/NakliTechie/menagerie/relay-go/internal/shims"
)

// RelayVersion is reported in the hello message.
const RelayVersion = "0.1.0"

type sessionEntry struct {
	token     string
	sess      *pty.Session
	agent     string
	startedAt time.Time
	pid       int

	subMu sync.Mutex
	sub   *conn // current subscriber connection (may be nil between reconnects)
}

func (e *sessionEntry) setSub(cn *conn) { e.subMu.Lock(); e.sub = cn; e.subMu.Unlock() }
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

// getSession returns the session iff the token matches (constant-time).
func (s *Server) getSession(id, token string) (*pty.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(e.token), []byte(token)) != 1 {
		return nil, false
	}
	return e.sess, true
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
		out = append(out, protocol.SessionInfo{SessionID: id, Agent: e.agent, StartedAt: e.startedAt.UTC().Format(time.RFC3339), PID: e.pid})
	}
	return out
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

func (cn *conn) serve() {
	hello := protocol.Hello{
		Type:            protocol.TypeHello,
		ProtocolVersion: protocol.Version,
		RelayVersion:    RelayVersion,
		RelayName:       cn.srv.cfg.Name,
		HostOS:          runtime.GOOS,
		HostArch:        runtime.GOARCH,
		Agents:          cn.srv.cfg.AgentNames(),
		Transports:      []string{"pty"},
		HostsChildren:   false,
	}
	if err := cn.send(hello); err != nil {
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
	// Advertise live sessions so a reconnecting client can re-attach (1.1).
	_ = cn.send(protocol.Sessions{Type: protocol.TypeSessions, Sessions: cn.srv.listSessions()})
	log.Printf("client registered")
}

func (cn *conn) handleSpawn(raw json.RawMessage) {
	var msg protocol.Spawn
	if err := json.Unmarshal(raw, &msg); err != nil {
		cn.sendError("", "bad_message", "malformed spawn")
		return
	}
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
	id := randomID()
	sess, err := pty.Start(id, msg.Agent, cmd)
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
	s := cn.srv
	s.addSession(&sessionEntry{token: token, sess: sess, agent: msg.Agent, startedAt: sess.StartedAt, pid: sess.PID, sub: cn}, id)
	_ = cn.send(protocol.Spawned{
		Type:         protocol.TypeSpawned,
		SessionID:    id,
		ClientID:     msg.ClientID,
		SessionToken: token,
		Agent:        msg.Agent,
		PID:          sess.PID,
		StartedAt:    sess.StartedAt.UTC().Format(time.RFC3339),
	})
	log.Printf("spawned %s (agent=%s pid=%d)", id, msg.Agent, sess.PID)

	go sess.Run(
		func(seq int, b []byte) {
			s.deliverOutput(id, seq, b)
			if shim.DetectNeedsInput(b) {
				s.deliverEvent(id, protocol.EventNeedsInput, nil)
			}
		},
		func(code int) {
			c := code
			s.deliverEvent(id, protocol.EventExited, &c)
			s.removeSession(id)
			log.Printf("exited %s (code=%d)", id, code)
		},
	)
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
	if buf := e.sess.Buffer(); len(buf) > 0 {
		_ = cn.send(protocol.Output{Type: protocol.TypeOutput, SessionID: msg.SessionID, Data: base64.StdEncoding.EncodeToString(buf), Seq: -1})
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
		cn.sendError(msg.SessionID, protocol.ErrInvalidToken, "unknown session or bad token")
		return
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
	sess, ok := cn.srv.getSession(msg.SessionID, msg.SessionToken)
	if !ok {
		cn.sendError(msg.SessionID, protocol.ErrInvalidToken, "unknown session or bad token")
		return
	}
	switch msg.Signal {
	case protocol.SignalKill:
		sess.Kill()
	case protocol.SignalInterrupt:
		sess.Interrupt()
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
func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
