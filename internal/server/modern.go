package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/NakliTechie/continuum/internal/jsonwire"
	"github.com/NakliTechie/continuum/internal/pty"
	"github.com/NakliTechie/continuum/internal/terminal"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/journal"
	"github.com/NakliTechie/continuum/internal/protocol"
)

// ContractVersion is the /v1 contract's stability version. Within a major, the
// stable operation set and stable capabilities keep their meaning and only grow
// additively; a breaking change bumps this major AND the socket name (v1.sock →
// v2.sock), so an older client meets no socket rather than a changed contract.
const ContractVersion = "1.0"

// Capability tiers advertised by the version and status operations. Stable
// capabilities will not change meaning within contract major 1; experimental
// ones may change or be withdrawn. Changing either set is a deliberate act —
// a pinned test guards it.
var (
	stableCapabilities       = []string{"pty", "observers", "control_lease", "event_replay", "control_renewal", "legacy_1.3"}
	experimentalCapabilities = []string{"terminal_screen_v1", "terminal_input_base64", "recording_policy_v1", "directories_v1"}
	// stableOperations is the frozen /v1 operation vocabulary. version/status/
	// events/screen are reads; the rest mutate through the request-id ledger.
	stableOperations = []string{"version", "status", "events", "screen", "open", "acquire", "renew", "release", "takeover", "input", "resize", "stop", "purge", "retire"}
)

func allCapabilities() []string {
	return append(append([]string{}, stableCapabilities...), experimentalCapabilities...)
}

type lease struct {
	Token   string
	Expires time.Time
}

// Modern uses the legacy server's process registry. It owns durable observation
// and modern leases, never another process manager.
type Modern struct {
	s            *Server
	Store        *journal.Store
	observer     string
	leases       map[string]lease
	degraded     atomic.Bool
	retiredMu    sync.Mutex
	retired      map[string]screenResult
	retiredOrder []string
	directories  *directoryBrowser // immutable after the API starts serving
}

func (s *Server) EnableModern(store *journal.Store, observer string) *Modern {
	m := &Modern{s: s, Store: store, observer: observer, leases: map[string]lease{}, retired: map[string]screenResult{}}
	s.modern = m
	return m
}
func (s *Server) record(id, kind string, payload any) {
	if m := s.modern; m != nil {
		if err := m.Store.Append(id, kind, payload); err != nil {
			m.degraded.Store(true)
		}
	}
}
func (s *Server) recordOutput(id string, b []byte, unobserved bool) {
	if m := s.modern; m != nil {
		e := s.entry(id)
		mode := journal.RecordingFull
		var err error
		if e != nil {
			mode, _, err = journal.NormalizeRecording(e.recording, e.recordingLines)
		} else if block, blockErr := m.Store.Block(id); blockErr != nil {
			err = blockErr
		} else {
			mode = block.Recording
		}
		if err != nil {
			m.degraded.Store(true)
			return
		}
		if mode == journal.RecordingVisible {
			if e == nil {
				// A holder can report an exited block before a restarted daemon
				// has an engine to rebuild its final frame. Preserve the resume
				// offset and mark only this recording incomplete; the store itself
				// remains healthy and usable.
				err = m.Store.AdvanceOutput(id, len(b))
				if err == nil {
					err = m.Store.MarkIncomplete(id)
				}
			} else if sess := s.entrySess(e); sess == nil {
				err = errors.New("visible recording requires a PTY terminal")
			} else if frame, ok := sess.TerminalSnapshot(); !ok {
				err = errors.New("visible recording requires screen-v1")
			} else {
				err = m.Store.AppendOutputScreen(id, b, frame, renderRecordedScreen(frame), unobserved)
			}
		} else {
			err = m.Store.AppendOutput(id, b, unobserved)
		}
		if err != nil {
			m.degraded.Store(true)
		}
	}
}

func renderRecordedScreen(frame terminal.Snapshot) []byte {
	var out strings.Builder
	out.WriteString("\x1b[2J\x1b[H")
	out.WriteString(strings.Join(frame.ANSI, "\r\n"))
	fmt.Fprintf(&out, "\x1b[%d;%dH", frame.Cursor.Y+1, frame.Cursor.X+1)
	if frame.Cursor.Visible {
		out.WriteString("\x1b[?25h")
	} else {
		out.WriteString("\x1b[?25l")
	}
	return []byte(out.String())
}
func (s *Server) recordStructured(id, kind string, payload any) {
	if m := s.modern; m != nil {
		raw, err := jsonwire.Marshal(payload)
		if err == nil {
			err = m.Store.AppendStructured(id, kind, json.RawMessage(raw))
		}
		if err != nil {
			m.degraded.Store(true)
		}
	}
}

func (s *Server) recordCaptureLoss(id string, cause error) {
	if m := s.modern; m != nil {
		if err := m.Store.MarkIncomplete(id); err != nil {
			m.degraded.Store(true)
		}
		s.record(id, "capture_error", map[string]string{"message": cause.Error()})
	}
}

// activeBlocks counts recorded blocks whose process is still running. Exited
// and interrupted blocks are retired on demand, so only active ones cap opens.
func (m *Modern) activeBlocks() (int, error) {
	blocks, err := m.Store.Blocks()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, b := range blocks {
		if b.State == "active" {
			n++
		}
	}
	return n, nil
}

func (m *Modern) recordBlock(id string, e *sessionEntry) {
	if err := m.Store.AddBlock(journal.Block{ID: id, Agent: e.agent, PID: e.pid, State: "active", Started: e.startedAt.UTC().Format(time.RFC3339Nano), Recording: e.recording, RecordingLines: e.recordingLines}); err != nil {
		m.degraded.Store(true)
	}
	m.s.record(id, "opened", map[string]any{"agent": e.agent, "pid": e.pid})
}
func (m *Modern) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	reply := func(v api.Response) { enc := json.NewEncoder(w); enc.SetEscapeHTML(false); _ = enc.Encode(v) }
	if r.Method != "POST" {
		w.WriteHeader(405)
		reply(api.Error("", "invalid_request", "method", "use POST /v1", "help"))
		return
	}
	// No browser origin is accepted by this alpha API. Menagerie uses the
	// separately gated legacy adapter; a read-only bearer never registers there.
	if r.Header.Get("Origin") != "" {
		w.WriteHeader(403)
		reply(api.Error("", "access_denied", "origin", "browser requests use the legacy adapter", "help"))
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	operator := token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(m.s.cfg.RegistrationToken)) == 1
	viewer := token != "" && m.observer != "" && subtle.ConstantTimeCompare([]byte(token), []byte(m.observer)) == 1
	if !operator && !viewer {
		w.WriteHeader(401)
		reply(api.Error("", "access_denied", "authentication", "valid bearer token required", "help"))
		return
	}
	var q api.Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&q); err != nil {
		reply(api.Error("", "invalid_request", "request", "invalid or oversized JSON request", "help"))
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		reply(api.Error("", "invalid_request", "request", "one JSON object required", "help"))
		return
	}
	if len(q.RequestID) > 128 || len(q.Block) > 128 || len(q.Lease) > 128 || len(q.Recording) > 32 {
		reply(api.Error("", "invalid_request", "field_size", "request field exceeds its size limit", "help"))
		return
	}
	read := q.Operation == "version" || q.Operation == "status" || q.Operation == "events" || q.Operation == "screen"
	if !read && !operator {
		w.WriteHeader(403)
		reply(api.Error(q.RequestID, "access_denied", "operator_required", "observer credentials cannot change sessions", "status"))
		return
	}
	// Directory reads require operator authority, but neither a control lease
	// nor a mutation ledger entry. Do not widen the existing observer role.
	if q.Operation == "directories" {
		reply(m.listDirectories(r.Context(), q))
		return
	}
	if read {
		reply(m.read(q))
		return
	}
	if q.Operation == "open" {
		m.s.spawnMu.Lock()
		defer m.s.spawnMu.Unlock()
	}
	m.s.controlMu.Lock()
	defer m.s.controlMu.Unlock()
	if m.s.closing.Load() {
		reply(api.Error(q.RequestID, "unreachable", "shutdown", "daemon is stopping", "status"))
		return
	}
	reply(m.mutate(q))
}
func (m *Modern) read(q api.Request) api.Response {
	switch q.Operation {
	case "version":
		return api.Result(q.RequestID, map[string]any{
			"contract":                 "continuum/v1",
			"contract_version":         ContractVersion,
			"schema_version":           1,
			"protocol":                 "continuum.local-alpha.1",
			"server":                   m.s.cfg.ServerVersion,
			"operations":               append(append([]string{}, stableOperations...), "directories"),
			"capabilities":             map[string]any{"stable": stableCapabilities, "experimental": experimentalCapabilities},
			"process_restart_survival": m.s.cfg.HoldersState != "",
		})
	case "screen":
		return m.screen(q)
	case "status":
		blocks, err := m.Store.Blocks()
		if err != nil {
			return api.Error(q.RequestID, "resource_exhausted", "store_read", "cannot read state", "status")
		}
		active := 0
		for _, b := range blocks {
			if b.State == "active" {
				active++
			}
		}
		total := len(blocks)
		page := []journal.Block{}
		next := ""
		more := false
		for _, b := range blocks {
			if q.Block != "" && b.ID != q.Block {
				continue
			}
			if b.ID <= q.Cursor {
				continue
			}
			if len(page) == 20 {
				more = true
				break
			}
			page = append(page, b)
			next = b.ID
		}
		blocks = page
		v := api.Result(q.RequestID, map[string]any{"host_id": m.Store.Host, "protocol": "continuum.local-alpha.1", "capabilities": allCapabilities(), "contract_version": ContractVersion, "blocks": blocks, "total": total, "active": active, "truncated": more, "next_cursor": next, "capture_degraded": m.degraded.Load(), "observed_at": time.Now().UTC().Format(time.RFC3339Nano), "process_restart_survival": m.s.cfg.HoldersState != ""})
		return v
	case "events":
		p, err := m.Store.Read(q.After, q.Block)
		if err != nil {
			return api.Error(q.RequestID, "resource_exhausted", "store_read", "cannot read events", "status")
		}
		v := api.Result(q.RequestID, p)
		if p.Gap {
			v.Class = "history_gap"
			v.Code = "cursor_unavailable"
			v.Message = "cursor is outside retained history; choose first_available - 1 explicitly"
			v.Next = api.Action{Kind: "command", Operation: "events"}
		}
		if (m.degraded.Load() || p.Incomplete) && !p.Gap {
			v.Class = "indeterminate"
			v.Code = "capture_degraded"
			v.Message = "history may be incomplete after capture failure or an unclean daemon epoch"
			v.Next = api.Action{Kind: "command", Operation: "status"}
		}
		return v
	}
	return api.Error(q.RequestID, "unsupported", "operation", "unsupported operation", "help")
}
func (m *Modern) mutate(q api.Request) api.Response {
	bad := func(code, msg string) api.Response {
		return api.Error(q.RequestID, "invalid_request", code, msg, "help")
	}
	switch q.Operation {
	case "open", "acquire", "takeover", "renew", "release", "input", "resize", "stop", "purge", "retire":
	default:
		return api.Error(q.RequestID, "unsupported", "operation", "unsupported operation", "help")
	}
	if q.RequestID == "" {
		return bad("request_id", "mutations require a stable request_id")
	}
	if q.Operation == "open" {
		mode, _, err := journal.NormalizeRecording(q.Recording, q.RecordingLines)
		if err != nil {
			return bad("recording", err.Error())
		}
		if mode == journal.RecordingVisible && q.Terminal != "screen-v1" {
			return bad("recording", "visible recording requires terminal screen-v1")
		}
		if q.Terminal != "" && q.Terminal != "screen-v1" {
			return bad("terminal_profile", "supported terminal profile: screen-v1")
		}
		if q.Terminal == "screen-v1" {
			if q.Cols == 0 {
				q.Cols = 80
			}
			if q.Rows == 0 {
				q.Rows = 24
			}
			if !terminal.ValidSize(q.Cols, q.Rows) {
				return bad("size", "screen-v1 allows 240 columns, 100 rows and 19200 cells")
			}
		}
		if len(q.Args) == 0 || len(q.Args) > 256 || !filepath.IsAbs(q.Cwd) {
			return bad("command", "open requires an argv array and an absolute cwd")
		}
		if info, err := os.Stat(q.Cwd); err != nil || !info.IsDir() {
			return bad("cwd", "working directory does not exist")
		}
	} else {
		if q.Recording != "" || q.RecordingLines != 0 {
			return bad("recording", "recording fields are only valid for open")
		}
		if q.Block == "" {
			return bad("block_id", "block_id required")
		}
	}
	if q.Operation == "resize" && (q.Cols < 1 || q.Cols > 1000 || q.Rows < 1 || q.Rows > 1000) {
		return bad("size", "columns and rows must be between 1 and 1000")
	}
	if q.Operation == "input" {
		switch q.Encoding {
		case "":
			if len(q.Data) > 64<<10 {
				return bad("input_size", "input supports at most 64 KiB")
			}
		case "base64":
			data, err := base64.StdEncoding.DecodeString(q.Data)
			if err != nil {
				return bad("input_encoding", "invalid base64 input")
			}
			if len(data) > 64<<10 {
				return bad("input_size", "input supports at most 64 KiB")
			}
		default:
			return bad("input_encoding", "supported input encodings: UTF-8 text or base64 for screen-v1")
		}
	} else if q.Encoding != "" {
		return bad("input_encoding", "encoding is only valid for input")
	}
	if m.degraded.Load() {
		return api.Error(q.RequestID, "resource_exhausted", "capture_degraded", "repair storage and restart before new mutations", "status")
	}
	encoded, _ := json.Marshal(q)
	hash := sha256.Sum256(encoded)
	digest := hex.EncodeToString(hash[:])
	old, err := m.Store.Lookup(q.RequestID)
	if err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "intent_store", "cannot read operation intent", "status")
	}
	if old == nil {
		// A request that cannot have an effect never occupies the ledger.
		requiresRunning := q.Operation == "acquire" || q.Operation == "takeover" || q.Operation == "renew" || q.Operation == "release" || q.Operation == "input" || q.Operation == "resize" || q.Operation == "stop"
		if requiresRunning && m.s.entry(q.Block) == nil {
			return api.Error(q.RequestID, "conflict", "not_running", "block has no running process", "status")
		}
		if old, err = m.Store.Begin(q.RequestID, digest); err != nil {
			return api.Error(q.RequestID, "resource_exhausted", "intent_store", "cannot commit operation intent", "status")
		}
	}
	if old != nil {
		if old.Digest != digest {
			return api.Error(q.RequestID, "conflict", "request_reused", "request_id already names different arguments", "status")
		}
		if len(old.Result) == 0 {
			return api.Error(q.RequestID, "indeterminate", "unresolved_intent", "effect may have happened; inspect sessions before another mutation", "status")
		}
		var v api.Response
		if json.Unmarshal(old.Result, &v) != nil {
			return api.Error(q.RequestID, "indeterminate", "invalid_result", "saved result cannot be decoded", "status")
		}
		return v
	}
	v := m.effect(q)
	// Control secrets are returned once and never journaled. Replaying this
	// operation must reconcile via explicit takeover, not recover secret bytes.
	saved := v
	if v.Class == "ok" && (q.Operation == "acquire" || q.Operation == "takeover") {
		saved = api.Error(q.RequestID, "indeterminate", "lease_not_replayed", "control was acquired; its secret is returned only once; use explicit takeover if lost", "takeover")
	}
	raw, _ := json.Marshal(saved)
	if err := m.Store.Finish(q.RequestID, digest, raw); err != nil {
		m.degraded.Store(true)
		return api.Error(q.RequestID, "indeterminate", "result_commit_failed", "effect may have happened; inspect sessions", "status")
	}
	if m.degraded.Load() {
		return api.Error(q.RequestID, "indeterminate", "capture_degraded", "effect may have happened but capture failed", "status")
	}
	return v
}
func (m *Modern) effect(q api.Request) api.Response {
	fail := func(class, code, msg, next string) api.Response {
		return api.Error(q.RequestID, class, code, msg, next)
	}
	if q.Operation == "open" {
		recording, recordingLines, _ := journal.NormalizeRecording(q.Recording, q.RecordingLines)
		if active, err := m.activeBlocks(); err != nil || active >= journal.MaxBlocks {
			return fail("resource_exhausted", "block_limit", "state supports at most 1024 active blocks", "status")
		}
		if q.Terminal == "screen-v1" && m.activeScreens() >= 16 {
			return fail("resource_exhausted", "terminal_limit", "at most 16 server-owned terminals may run concurrently", "status")
		}
		var response map[string]any
		cn := &conn{srv: m.s, ctx: context.Background(), registered: true, recording: recording, recordingLines: recordingLines, sink: func(b []byte) error {
			var v map[string]any
			_ = json.Unmarshal(b, &v)
			if v["type"] == "spawned" || v["type"] == "error" {
				response = v
			}
			return nil
		}}
		if q.Terminal == "screen-v1" {
			cn.terminal = &pty.TerminalOptions{Cols: q.Cols, Rows: q.Rows}
		}
		cn.handleSpawnPTY(protocol.Spawn{Agent: "custom", Args: q.Args, Cwd: q.Cwd})
		m.s.detach(cn)
		if response == nil || response["type"] != "spawned" {
			return fail("invalid_request", "spawn_failed", "could not launch executable in the selected directory", "help")
		}
		return api.Result(q.RequestID, map[string]any{"block_id": response["session_id"], "pid": response["pid"], "terminal": q.Terminal, "recording": recording, "recording_lines": recordingLines})
	}
	if q.Operation == "purge" {
		if err := m.Store.PurgeRecording(q.Block); err != nil {
			switch {
			case errors.Is(err, journal.ErrNotFound):
				return fail("conflict", "not_found", "block is not recorded", "status")
			case errors.Is(err, journal.ErrActive):
				return fail("conflict", "block_active", "only an exited block can be purged; stop active work or reconcile an interrupted block first", "status")
			}
			return fail("resource_exhausted", "store_write", "could not purge recording", "status")
		}
		m.retiredMu.Lock()
		delete(m.retired, q.Block)
		m.retiredMu.Unlock()
		return api.Result(q.RequestID, map[string]any{"block_id": q.Block, "recording_purged": true})
	}
	if q.Operation == "retire" {
		if err := m.Store.Retire(q.Block); err != nil {
			switch {
			case errors.Is(err, journal.ErrNotFound):
				return fail("conflict", "not_found", "block is not recorded", "status")
			case errors.Is(err, journal.ErrActive):
				return fail("conflict", "block_active", "only an exited block can be retired; stop active work or reconcile an interrupted block first", "status")
			default:
				return fail("resource_exhausted", "store_write", "could not retire block", "status")
			}
		}
		m.retiredMu.Lock()
		delete(m.retired, q.Block)
		for i, id := range m.retiredOrder {
			if id == q.Block {
				m.retiredOrder = append(m.retiredOrder[:i], m.retiredOrder[i+1:]...)
				break
			}
		}
		m.retiredMu.Unlock()
		return api.Result(q.RequestID, map[string]any{"block_id": q.Block, "retired": true})
	}
	e := m.s.entry(q.Block)
	if e == nil {
		return fail("conflict", "not_running", "block has no running process", "status")
	}
	current := m.leases[q.Block]
	if q.Operation == "acquire" || q.Operation == "takeover" {
		if q.Operation == "acquire" && (current.Expires.After(time.Now()) || e.subscriber() != nil) {
			return fail("conflict", "controlled", "another controller holds this block; takeover is explicit", "takeover")
		}
		tok := journal.ID()
		m.s.reissueToken(q.Block, tok)
		current = lease{tok, time.Now().Add(60 * time.Second)}
		m.leases[q.Block] = current
		return api.Result(q.RequestID, map[string]any{"block_id": q.Block, "lease": tok, "expires_at": current.Expires.UTC().Format(time.RFC3339Nano)})
	}
	if !current.Expires.After(time.Now()) || q.Lease == "" || subtle.ConstantTimeCompare([]byte(current.Token), []byte(q.Lease)) != 1 || m.s.authSession(q.Block, q.Lease) == nil {
		return fail("conflict", "stale_control", "control expired or was taken over; acquire current control", "acquire")
	}
	if q.Operation == "renew" {
		current.Expires = time.Now().Add(60 * time.Second)
		m.leases[q.Block] = current
		return api.Result(q.RequestID, map[string]any{"block_id": q.Block, "expires_at": current.Expires.UTC().Format(time.RFC3339Nano)})
	}
	if q.Operation == "release" {
		m.s.reissueToken(q.Block, journal.ID())
		delete(m.leases, q.Block)
		return api.Result(q.RequestID, map[string]any{"released": true})
	}
	if q.Operation == "stop" {
		m.s.killEntry(q.Block)
		return api.Result(q.RequestID, map[string]any{"accepted": true})
	}
	sess := m.s.entrySess(e)
	if sess == nil {
		return fail("unsupported", "not_pty", "operation requires a PTY block", "status")
	}
	if q.Operation == "resize" && sess.HasTerminal() && !terminal.ValidSize(q.Cols, q.Rows) {
		return fail("invalid_request", "size", "screen-v1 allows 240 columns, 100 rows and 19200 cells", "help")
	}
	var err error
	if q.Operation == "input" {
		data := []byte(q.Data)
		if q.Encoding == "base64" {
			if !sess.HasTerminal() {
				return fail("unsupported", "terminal_profile_required", "binary input requires screen-v1", "help")
			}
			data, _ = base64.StdEncoding.DecodeString(q.Data) // validated before intent
		}
		err = sess.Write(data)
	} else {
		err = sess.Resize(q.Cols, q.Rows)
	}
	if err != nil {
		return fail("indeterminate", "process_io", "process I/O failed; inspect state before retrying", "status")
	}
	return api.Result(q.RequestID, map[string]any{"accepted": true})
}

// leaseExpired reports whether token is a modern control lease for block that
// has lapsed. The legacy adapter checks it so an expired lease is fenced on
// both doors, not only on /v1. Callers hold controlMu.
func (m *Modern) leaseExpired(block, token string) bool {
	current, ok := m.leases[block]
	if !ok || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(current.Token), []byte(token)) == 1 && !current.Expires.After(time.Now())
}

// BeginShutdown cancels pending handshakes and closes hijacked WebSockets,
// which net/http.Shutdown does not own. Established processes stop in StopAll.
func (s *Server) BeginShutdown() {
	s.closing.Store(true)
	s.cancel()
	s.connMu.Lock()
	defer s.connMu.Unlock()
	for cn, cancel := range s.conns {
		cancel()
		_ = cn.ws.CloseNow()
	}
}

// StopAll is used only for the foreground alpha daemon's explicit shutdown.
func (s *Server) StopAll() {
	s.BeginShutdown()
	s.spawnMu.Lock()
	defer s.spawnMu.Unlock()
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	for _, b := range s.listSessions() {
		s.killEntry(b.SessionID)
	}
}

func (s *Server) Drain(ctx context.Context) error {
	for len(s.listSessions()) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return nil
}
func (m *Modern) CleanShutdown() error {
	if m.degraded.Load() {
		return nil
	}
	return m.Store.CleanShutdown()
}
