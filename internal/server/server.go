// Package server implements the relay's WebSocket server.
//
// P2 scope: the hello + register handshake, origin enforcement, and the
// session-token map scaffold. PTY spawn/input/signal/resume land in P3.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"runtime"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/NakliTechie/menagerie/relay-go/internal/config"
	"github.com/NakliTechie/menagerie/relay-go/internal/protocol"
)

// RelayVersion is reported in the hello message.
const RelayVersion = "0.1.0"

// Server holds relay-wide state shared across connections.
type Server struct {
	cfg *config.Config

	mu       sync.Mutex
	sessions map[string]string // session_id -> session_token (populated in P3)
}

// New builds a Server for the given config.
func New(cfg *config.Config) *Server {
	return &Server{cfg: cfg, sessions: make(map[string]string)}
}

// Handler returns the relay's HTTP handler (a single WebSocket endpoint).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleWS)
	return mux
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// Origin check is the gate (§8). Browsers always send Origin on WS
	// upgrades; "null" covers file:// dev. An empty Origin means a non-browser
	// client (e.g. a supervisor agent — the agent face, §16), which we allow to
	// proceed to token-based register.
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

	(&conn{srv: s, ws: c}).serve(r.Context())
}

// conn is one browser<->relay WebSocket connection.
type conn struct {
	srv        *Server
	ws         *websocket.Conn
	registered bool
}

func (cn *conn) serve(ctx context.Context) {
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
	if err := wsjson.Write(ctx, cn.ws, hello); err != nil {
		return
	}

	for {
		var raw json.RawMessage
		if err := wsjson.Read(ctx, cn.ws, &raw); err != nil {
			return // connection closed or context done
		}
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			cn.sendError(ctx, "", "bad_message", "malformed message")
			continue
		}
		cn.dispatch(ctx, env, raw)
	}
}

func (cn *conn) dispatch(ctx context.Context, env protocol.Envelope, raw json.RawMessage) {
	if env.Type == protocol.TypeRegister {
		cn.handleRegister(ctx, raw)
		return
	}
	if !cn.registered {
		cn.sendError(ctx, env.SessionID, protocol.ErrAuthFailed, "register before sending "+env.Type)
		return
	}
	switch env.Type {
	case protocol.TypeSpawn:
		// PTY support lands in P3.
		cn.sendError(ctx, env.SessionID, protocol.ErrSpawnFailed, "spawn not implemented yet (PTY lands in P3)")
	case protocol.TypeInput, protocol.TypeSignal, protocol.TypeResume:
		cn.sendError(ctx, env.SessionID, "unknown_session", "no active sessions (P2 skeleton)")
	default:
		cn.sendError(ctx, env.SessionID, "bad_message", "unknown message type: "+env.Type)
	}
}

func (cn *conn) handleRegister(ctx context.Context, raw json.RawMessage) {
	var msg protocol.Register
	if err := json.Unmarshal(raw, &msg); err != nil {
		cn.sendError(ctx, "", "bad_message", "malformed register")
		return
	}
	want := cn.srv.cfg.RegistrationToken
	if want == "" || subtle.ConstantTimeCompare([]byte(msg.RegistrationToken), []byte(want)) != 1 {
		cn.sendError(ctx, "", protocol.ErrAuthFailed, "registration token rejected")
		_ = cn.ws.Close(websocket.StatusPolicyViolation, "auth_failed")
		return
	}
	cn.registered = true
	_ = wsjson.Write(ctx, cn.ws, protocol.Registered{Type: protocol.TypeRegistered})
	log.Printf("client registered")
}

func (cn *conn) sendError(ctx context.Context, sessionID, code, message string) {
	_ = wsjson.Write(ctx, cn.ws, protocol.NewError(sessionID, code, message))
}
