// Package server implements the relay's WebSocket server.
//
// P3 scope: the hello + register handshake, origin enforcement, and live PTY
// sessions — spawn / input / signal / output / event. Reconnect-resume + FSA
// replay land in P4.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"runtime"
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
	token string
	sess  *pty.Session
}

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

func (s *Server) addSession(id, token string, sess *pty.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = &sessionEntry{token: token, sess: sess}
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

func (s *Server) removeSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// Origin check is the gate (§8). Browsers always send Origin; "null" covers
	// file:// dev. Empty Origin = non-browser client (agent face, §16) → allowed
	// to proceed to token-based register.
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
}

// conn is one browser<->relay WebSocket connection. All writes go through
// send(), which serializes them (a session's output goroutine writes
// concurrently with the read loop).
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
	case protocol.TypeInput:
		cn.handleInput(raw)
	case protocol.TypeSignal:
		cn.handleSignal(raw)
	case protocol.TypeResume:
		// Reconnect-resume lands in P4; for now the client falls back to FSA replay.
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
	cn.srv.addSession(id, token, sess)
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
			_ = cn.send(protocol.Output{
				Type:      protocol.TypeOutput,
				SessionID: id,
				Data:      base64.StdEncoding.EncodeToString(b),
				Seq:       seq,
			})
			if shim.DetectNeedsInput(b) {
				cn.writeEvent(id, protocol.EventNeedsInput, nil)
			}
		},
		func(code int) {
			c := code
			cn.writeEvent(id, protocol.EventExited, &c)
			cn.srv.removeSession(id)
			log.Printf("exited %s (code=%d)", id, code)
		},
	)
}

func (cn *conn) writeEvent(id, event string, code *int) {
	_ = cn.send(protocol.Event{
		Type:      protocol.TypeEvent,
		SessionID: id,
		Event:     event,
		ExitCode:  code,
		At:        time.Now().UTC().Format(time.RFC3339),
	})
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

// randomID returns a short hex session id.
func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
