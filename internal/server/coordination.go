package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/journal"
	"github.com/NakliTechie/continuum/internal/protocol"
)

const maxWaits = 1024
const maxPendingWaits = 64

type waitMember struct {
	api.WaitSource
	HostID    string `json:"host_id"`
	Cursor    uint64 `json:"cursor"`
	State     string `json:"state"`
	Transport string `json:"transport,omitempty"`
	Matched   bool   `json:"matched,omitempty"`
	Order     uint64 `json:"completion_order,omitempty"`
	Code      string `json:"code,omitempty"`
	MinTurn   uint64 `json:"min_turn,omitempty"`
}

type waitRecord struct {
	ID              string       `json:"wait_id"`
	RequestID       string       `json:"request_id"`
	Caller          string       `json:"caller"`
	Digest          string       `json:"digest"`
	Mode            string       `json:"mode"`
	State           string       `json:"state"`
	Code            string       `json:"code"`
	CreatedAt       string       `json:"created_at"`
	Deadline        string       `json:"deadline"`
	FinishedAt      string       `json:"finished_at,omitempty"`
	Winner          string       `json:"winner,omitempty"`
	Order           uint64       `json:"completion_order,omitempty"`
	CancelRequestID string       `json:"cancel_request_id,omitempty"`
	Sources         []waitMember `json:"sources"`
}

type waitFetch func(context.Context, api.Peer, api.Request) api.Response

type waitCoordinator struct {
	m       *Modern
	state   string
	fetch   waitFetch
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex // admission count and request ID lookup; bbolt serializes transitions
	busy    map[string]bool
	due     map[string]time.Time
	sem     chan struct{}
	workers sync.WaitGroup
}

func (m *Modern) StartWaits(state string, fetch waitFetch) {
	ctx, cancel := context.WithCancel(context.Background())
	w := &waitCoordinator{m: m, state: state, fetch: fetch, ctx: ctx, cancel: cancel, busy: map[string]bool{}, due: map[string]time.Time{}, sem: make(chan struct{}, 8)}
	m.waits = w
	w.workers.Add(1)
	go w.loop()
}

func (m *Modern) StopWaits() {
	if m.waits != nil {
		m.waits.cancel()
		m.waits.workers.Wait()
	}
}

func waitID(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func validPeerHost(host string) bool {
	if host == "" || len(host) > 255 || strings.HasPrefix(host, "-") {
		return false
	}
	for _, r := range host {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-@:%[]", r)) {
			return false
		}
	}
	return true
}

// A peer is an explicitly pinned SSH bridge, never a destination supplied by
// an ordinary wait. The observation credential stays on the owning host.
func (w *waitCoordinator) addPeer(q api.Request) api.Response {
	p := q.Peer
	if !waitID(q.RequestID) || p == nil || !waitID(p.Name) || p.Name == "local" || !validPeerHost(p.Host) || !filepath.IsAbs(p.State) || !filepath.IsAbs(p.Binary) || p.HostID != "" {
		return api.Error(q.RequestID, "invalid_request", "peer", "name, trusted SSH host, absolute state and executable required; host ID is discovered", "help")
	}
	ctx, cancel := context.WithTimeout(w.ctx, 15*time.Second)
	defer cancel()
	v := w.fetch(ctx, *p, api.Request{Operation: "status"})
	if v.Class != "ok" {
		return api.Error(q.RequestID, v.Class, "peer_preflight", "remote observation failed; peer not saved", "status")
	}
	var status struct {
		HostID       string   `json:"host_id"`
		Capabilities []string `json:"capabilities"`
	}
	if len(v.Result) > 1<<20 || json.Unmarshal(v.Result, &status) != nil || status.HostID == "" || len(status.HostID) > 128 {
		return api.Error(q.RequestID, "indeterminate", "peer_identity", "remote host identity is unavailable", "status")
	}
	capable := false
	for _, cap := range status.Capabilities {
		if cap == "waits_v1" {
			capable = true
		}
	}
	if !capable {
		return api.Error(q.RequestID, "unsupported", "waits_v1", "remote peer does not support durable observation", "status")
	}
	copy := *p
	copy.HostID = status.HostID
	if err := w.m.audit("root", "peer_add", "intent", ""); err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "audit", "peer not registered", "status")
	}
	err := w.m.Store.UpdateObject("peers", copy.Name, 32, func(old json.RawMessage, _ uint64) (json.RawMessage, error) {
		if old != nil {
			var previous api.Peer
			if json.Unmarshal(old, &previous) != nil {
				return nil, errors.New("corrupt peer")
			}
			if previous != copy {
				return nil, errors.New("peer already registered with different identity or route")
			}
			return old, nil
		}
		return json.Marshal(copy)
	})
	if err != nil {
		return api.Error(q.RequestID, "conflict", "peer_register", "peer identity conflict or registry limit", "peer_list")
	}
	return api.Result(q.RequestID, copy)
}

func (w *waitCoordinator) removePeer(q api.Request) api.Response {
	if !waitID(q.RequestID) || !waitID(q.PeerName) || q.PeerName == "local" {
		return api.Error(q.RequestID, "invalid_request", "peer_name", "registered peer name and request ID required", "help")
	}
	if err := w.m.audit("root", "peer_remove", "intent", ""); err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "audit", "peer not removed", "status")
	}
	err := w.m.Store.UpdateObject("peers", q.PeerName, 32, func(old json.RawMessage, _ uint64) (json.RawMessage, error) {
		if old == nil {
			return nil, journal.ErrNotFound
		}
		return nil, nil
	})
	if errors.Is(err, journal.ErrNotFound) {
		return api.Error(q.RequestID, "conflict", "peer_unknown", "peer is not registered", "peer_list")
	}
	if err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "peer_store", "peer removal did not commit", "status")
	}
	return api.Result(q.RequestID, map[string]any{"peer_name": q.PeerName, "removed": true})
}

func (w *waitCoordinator) listPeers(q api.Request) api.Response {
	all, err := w.m.Store.Objects("peers")
	if err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "peer_store", "cannot list peers", "status")
	}
	peers := make([]api.Peer, 0, len(all))
	for _, raw := range all {
		var p api.Peer
		if json.Unmarshal(raw, &p) != nil {
			return api.Error(q.RequestID, "indeterminate", "peer_store", "corrupt peer registry", "status")
		}
		peers = append(peers, p)
	}
	return api.Result(q.RequestID, peers)
}

func validWaitSpec(spec *api.WaitSpec) bool {
	if spec == nil || (spec.Mode != "any" && spec.Mode != "all") || spec.DeadlineS < 1 || spec.DeadlineS > 86400 || len(spec.Sources) < 1 || len(spec.Sources) > 8 {
		return false
	}
	seen := map[string]bool{}
	for _, s := range spec.Sources {
		if !waitID(s.Name) || seen[s.Name] || !waitID(s.Peer) || len(s.Block) != 16 || len(s.Until) < 1 || len(s.Until) > 8 {
			return false
		}
		seen[s.Name] = true
		for _, c := range s.Block {
			if !strings.ContainsRune("0123456789abcdef", c) {
				return false
			}
		}
		for _, u := range s.Until {
			if !journal.Lifecycle(u) {
				return false
			}
		}
	}
	return true
}

func (w *waitCoordinator) peer(name string) (api.Peer, error) {
	if name == "local" {
		return api.Peer{Name: name, State: w.state, HostID: w.m.Store.Host}, nil
	}
	raw, err := w.m.Store.Object("peers", name)
	if err != nil {
		return api.Peer{}, err
	}
	var p api.Peer
	if json.Unmarshal(raw, &p) != nil || p.Name != name || p.Host == "" || p.State == "" || p.HostID == "" {
		return api.Peer{}, errors.New("invalid registered peer")
	}
	return p, nil
}

func (w *waitCoordinator) call(ctx context.Context, p api.Peer, q api.Request) api.Response {
	if p.Name == "local" {
		switch q.Operation {
		case "observe":
			return w.m.read(q)
		case "events":
			return w.m.read(q)
		}
	}
	return w.fetch(ctx, p, q)
}

func matched(s api.WaitSource, state string) bool {
	for _, v := range s.Until {
		if v == state {
			return true
		}
	}
	return false
}

func settleWait(r *waitRecord, seq uint64) {
	if r.State != "pending" {
		return
	}
	matchedCount := 0
	possible := 0
	anyExit := false
	anyFailed := false
	for _, m := range r.Sources {
		if m.Matched {
			matchedCount++
			if r.Mode == "any" && (r.Order == 0 || m.Order < r.Order) {
				r.Winner, r.Order = m.Name, m.Order
			}
		}
		if m.Code == "" {
			possible++
		}
		if m.Code == "exited" {
			anyExit = true
		}
		if m.Code != "" && m.Code != "exited" {
			anyFailed = true
		}
	}
	switch {
	case r.Mode == "any" && matchedCount > 0, r.Mode == "all" && matchedCount == len(r.Sources):
		r.State, r.Code = "satisfied", "condition_met"
	case r.Mode == "all" && possible < len(r.Sources), r.Mode == "any" && possible == 0:
		if anyFailed {
			r.State, r.Code = "failed", "source_failed"
		} else if anyExit {
			r.State, r.Code = "exited", "unmatched_exit"
		}
	}
	if r.State != "pending" {
		r.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if r.Order == 0 {
			r.Order = seq
		}
	}
}

func (w *waitCoordinator) create(q api.Request) api.Response {
	if !waitID(q.RequestID) || !validWaitSpec(q.Wait) {
		return api.Error(q.RequestID, "invalid_request", "wait_spec", "request_id, mode, deadline_s and 1..8 valid sources required", "help")
	}
	digestBytes, _ := json.Marshal(q.Wait)
	digest := sha256.Sum256(digestBytes)
	encodedDigest := hex.EncodeToString(digest[:])
	w.mu.Lock()
	defer w.mu.Unlock()
	if old, err := w.load(q.RequestID); err == nil {
		if old.Digest != encodedDigest {
			return api.Error(q.RequestID, "conflict", "request_reused", "request_id names a different wait", "wait_get")
		}
		return api.Result(q.RequestID, old)
	} else if !errors.Is(err, journal.ErrNotFound) {
		return api.Error(q.RequestID, "resource_exhausted", "wait_store", "cannot read wait ledger", "status")
	}
	all, err := w.m.Store.Objects("waits")
	if err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "wait_store", "cannot list waits", "status")
	}
	pending := 0
	for _, raw := range all {
		var v waitRecord
		if json.Unmarshal(raw, &v) != nil {
			return api.Error(q.RequestID, "indeterminate", "wait_store", "corrupt wait record", "status")
		}
		if v.State == "pending" {
			pending++
		}
	}
	if len(all) >= maxWaits || pending >= maxPendingWaits {
		return api.Error(q.RequestID, "resource_exhausted", "wait_limit", "wait capacity reached", "wait_get")
	}
	ctx, cancel := context.WithTimeout(w.ctx, 8*time.Second)
	defer cancel()
	type initial struct {
		peer     api.Peer
		response api.Response
		err      error
	}
	initials := make([]initial, len(q.Wait.Sources))
	var group sync.WaitGroup
	for i, s := range q.Wait.Sources {
		initials[i].peer, initials[i].err = w.peer(s.Peer)
		if initials[i].err != nil {
			return api.Error(q.RequestID, "invalid_request", "peer_unavailable", "source names an unknown or invalid peer", "help")
		}
		group.Add(1)
		go func(i int, s api.WaitSource) {
			defer group.Done()
			initials[i].response = w.call(ctx, initials[i].peer, api.Request{Operation: "observe", Block: s.Block})
		}(i, s)
	}
	group.Wait()
	for _, initial := range initials {
		if initial.response.Class == "unreachable" {
			// Without an initial atomic observation/cursor, replay after recovery
			// could satisfy this new wait from a pre-admission transient event.
			return api.Error(q.RequestID, "unreachable", "initial_observation", "source was unreachable before the wait could be armed", "wait_create")
		}
	}
	r := waitRecord{ID: q.RequestID, RequestID: q.RequestID, Caller: "root_operator", Digest: encodedDigest, Mode: q.Wait.Mode, State: "pending", Code: "pending", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Deadline: time.Now().Add(time.Duration(q.Wait.DeadlineS) * time.Second).UTC().Format(time.RFC3339Nano)}
	for i, s := range q.Wait.Sources {
		v := initials[i].response
		member := waitMember{WaitSource: s, HostID: initials[i].peer.HostID, State: "unknown"}
		if v.Class != "ok" {
			member.Code = "source_" + v.Class
		} else {
			var o journal.Observation
			if json.Unmarshal(v.Result, &o) != nil || o.Host != member.HostID || o.Block != s.Block || !journal.Lifecycle(o.State) {
				member.Code = "invalid_observation"
			} else if o.Incomplete {
				member.Code = "capture_degraded"
			} else {
				member.State, member.Cursor = o.State, o.Cursor
				if q.Operation == "prompt_wait" {
					member.MinTurn = o.Turn + 1
				} else if matched(s, o.State) {
					member.Matched = true
				} else if o.State == "exited" {
					member.Code = "exited"
				}
			}
		}
		r.Sources = append(r.Sources, member)
	}
	if err := w.m.Store.UpdateObject("waits", r.ID, maxWaits, func(old json.RawMessage, seq uint64) (json.RawMessage, error) {
		if old != nil {
			return nil, errors.New("wait identity changed during admission")
		}
		for i := range r.Sources {
			if r.Sources[i].Matched {
				r.Sources[i].Order = seq + uint64(i)
			}
		}
		settleWait(&r, seq)
		return json.Marshal(r)
	}); err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "wait_commit", "wait was not committed", "status")
	}
	return api.Result(q.RequestID, r)
}

func (w *waitCoordinator) load(id string) (waitRecord, error) {
	var r waitRecord
	raw, err := w.m.Store.Object("waits", id)
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	if r.ID != id || r.State == "" {
		return r, errors.New("invalid wait record")
	}
	return r, nil
}

func (w *waitCoordinator) handle(q api.Request) api.Response {
	switch q.Operation {
	case "peer_add":
		return w.addPeer(q)
	case "peer_remove":
		return w.removePeer(q)
	case "peer_list":
		return w.listPeers(q)
	case "wait_create":
		return w.create(q)
	case "wait_get", "wait_output":
		if !waitID(q.WaitID) {
			return api.Error(q.RequestID, "invalid_request", "wait_id", "wait_id required", "help")
		}
		r, err := w.load(q.WaitID)
		if errors.Is(err, journal.ErrNotFound) {
			return api.Error(q.RequestID, "conflict", "wait_unknown", "wait_id not found", "wait_get")
		}
		if err != nil {
			return api.Error(q.RequestID, "resource_exhausted", "wait_store", "cannot read wait", "status")
		}
		if q.Operation == "wait_output" && r.State == "pending" {
			return api.Error(q.RequestID, "conflict", "wait_not_ready", "wait has not resolved", "wait_get")
		}
		return api.Result(q.RequestID, r)
	case "wait_cancel":
		if !waitID(q.WaitID) || !waitID(q.RequestID) {
			return api.Error(q.RequestID, "invalid_request", "wait_id", "wait and request IDs required", "help")
		}
		var r waitRecord
		err := w.m.Store.UpdateObject("waits", q.WaitID, maxWaits, func(old json.RawMessage, seq uint64) (json.RawMessage, error) {
			if old == nil {
				return nil, journal.ErrNotFound
			}
			if err := json.Unmarshal(old, &r); err != nil {
				return nil, err
			}
			if r.CancelRequestID != "" && r.CancelRequestID != q.RequestID {
				return nil, errors.New("already cancelled under another request")
			}
			if r.State != "pending" {
				return old, nil
			}
			r.State, r.Code, r.CancelRequestID, r.Order, r.FinishedAt = "cancelled", "cancelled", q.RequestID, seq, time.Now().UTC().Format(time.RFC3339Nano)
			return json.Marshal(r)
		})
		if errors.Is(err, journal.ErrNotFound) {
			return api.Error(q.RequestID, "conflict", "wait_unknown", "wait_id not found", "wait_get")
		}
		if err != nil {
			return api.Error(q.RequestID, "conflict", "wait_cancel", "wait could not be cancelled", "wait_get")
		}
		return api.Result(q.RequestID, r)
	}
	return api.Error(q.RequestID, "unsupported", "operation", "unsupported wait operation", "help")
}

func (w *waitCoordinator) loop() {
	defer w.workers.Done()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
		}
		all, err := w.m.Store.Objects("waits")
		if err != nil {
			continue
		}
		for id, raw := range all {
			var r waitRecord
			if json.Unmarshal(raw, &r) != nil || r.State != "pending" {
				continue
			}
			deadline, _ := time.Parse(time.RFC3339Nano, r.Deadline)
			if deadline.IsZero() || !time.Now().Before(deadline) {
				w.expire(id)
				continue
			}
			for i, m := range r.Sources {
				if m.Matched || m.Code != "" {
					continue
				}
				key := fmt.Sprintf("%s:%d", id, i)
				w.mu.Lock()
				if w.busy[key] || time.Now().Before(w.due[key]) {
					w.mu.Unlock()
					continue
				}
				select {
				case w.sem <- struct{}{}:
					w.busy[key] = true
				default:
					w.mu.Unlock()
					continue
				}
				w.mu.Unlock()
				w.workers.Add(1)
				go w.poll(id, i, key)
			}
		}
	}
}

func (w *waitCoordinator) expire(id string) {
	_ = w.m.Store.UpdateObject("waits", id, maxWaits, func(old json.RawMessage, seq uint64) (json.RawMessage, error) {
		if old == nil {
			return nil, journal.ErrNotFound
		}
		var r waitRecord
		if err := json.Unmarshal(old, &r); err != nil {
			return nil, err
		}
		if r.State != "pending" {
			return old, nil
		}
		r.State, r.Code, r.Order, r.FinishedAt = "expired", "deadline", seq, time.Now().UTC().Format(time.RFC3339Nano)
		return json.Marshal(r)
	})
}

func (w *waitCoordinator) poll(id string, index int, key string) {
	defer w.workers.Done()
	defer func() {
		<-w.sem
		w.mu.Lock()
		delete(w.busy, key)
		w.due[key] = time.Now().Add(500 * time.Millisecond)
		w.mu.Unlock()
	}()
	r, err := w.load(id)
	if err != nil || r.State != "pending" || index >= len(r.Sources) {
		return
	}
	m := r.Sources[index]
	peer, err := w.peer(m.Peer)
	if err != nil || peer.HostID != m.HostID {
		w.applyFailure(id, index, "peer_changed")
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	v := w.call(ctx, peer, api.Request{Operation: "events", Block: m.Block, After: m.Cursor})
	if v.Class == "unreachable" {
		w.applyTransport(id, index, "unreachable")
		return
	}
	if v.Class == "history_gap" {
		w.applyFailure(id, index, "history_gap")
		return
	}
	if v.Class != "ok" {
		w.applyFailure(id, index, "source_"+v.Class)
		return
	}
	var p journal.Page
	if len(v.Result) > 16<<20 || json.Unmarshal(v.Result, &p) != nil || len(p.Events) > 256 || p.Gap || p.Next < m.Cursor || p.Next > p.Last {
		w.applyFailure(id, index, "invalid_page")
		return
	}
	previous := m.Cursor
	for _, e := range p.Events {
		if e.Host != m.HostID || e.Block != m.Block || e.Seq <= previous || e.Seq > p.Next || !json.Valid(e.Payload) {
			w.applyFailure(id, index, "invalid_event")
			return
		}
		previous = e.Seq
	}
	if p.Incomplete {
		w.applyFailure(id, index, "capture_degraded")
		return
	}
	if p.Next != m.Cursor || m.Transport != "" {
		_ = w.m.Store.UpdateObject("waits", id, maxWaits, func(old json.RawMessage, seq uint64) (json.RawMessage, error) {
			if old == nil {
				return nil, journal.ErrNotFound
			}
			var latest waitRecord
			if err := json.Unmarshal(old, &latest); err != nil {
				return nil, err
			}
			if latest.State != "pending" || latest.Sources[index].Cursor != m.Cursor {
				return old, nil
			}
			member := &latest.Sources[index]
			member.Transport = ""
			for _, e := range p.Events {
				if e.Type == "acp_prompt_failed" {
					var failure struct {
						TurnID uint64 `json:"turn_id"`
					}
					if json.Unmarshal(e.Payload, &failure) == nil && failure.TurnID >= member.MinTurn {
						member.Code = "prompt_failed"
						break
					}
				}
				if journal.Lifecycle(e.Type) {
					member.State = e.Type
					var detail struct {
						Turn uint64 `json:"turn"`
					}
					if json.Unmarshal(e.Payload, &detail) != nil {
						member.Code = "invalid_event"
						break
					}
					if detail.Turn < member.MinTurn {
						continue
					}
					if matched(member.WaitSource, e.Type) {
						member.Matched = true
						member.Order = seq
						break
					}
					if e.Type == "exited" {
						member.Code = "exited"
						break
					}
				}
			}
			member.Cursor = p.Next
			settleWait(&latest, seq)
			return json.Marshal(latest)
		})
	}
}

func (w *waitCoordinator) applyTransport(id string, index int, code string) {
	_ = w.m.Store.UpdateObject("waits", id, maxWaits, func(old json.RawMessage, _ uint64) (json.RawMessage, error) {
		if old == nil {
			return nil, journal.ErrNotFound
		}
		var r waitRecord
		if err := json.Unmarshal(old, &r); err != nil {
			return nil, err
		}
		if r.State != "pending" || r.Sources[index].Transport == code {
			return old, nil
		}
		r.Sources[index].Transport = code
		return json.Marshal(r)
	})
}

func (w *waitCoordinator) applyFailure(id string, index int, code string) {
	_ = w.m.Store.UpdateObject("waits", id, maxWaits, func(old json.RawMessage, seq uint64) (json.RawMessage, error) {
		if old == nil {
			return nil, journal.ErrNotFound
		}
		var r waitRecord
		if err := json.Unmarshal(old, &r); err != nil {
			return nil, err
		}
		if r.State != "pending" || r.Sources[index].Code != "" {
			return old, nil
		}
		r.Sources[index].Code = code
		settleWait(&r, seq)
		return json.Marshal(r)
	})
}

// The current control lease is checked by Modern.effect before either method.
// The wait commits before ACP dispatch; uncertain dispatch is never retried.
func (m *Modern) promptWaitEffect(q api.Request, e *sessionEntry) api.Response {
	if m.waits == nil {
		return api.Error(q.RequestID, "unsupported", "waits_v1", "coordinator is disabled", "contract")
	}
	if e.acp == nil {
		return api.Error(q.RequestID, "unsupported", "not_acp", "prompt requires a structured block", "status")
	}
	if q.Wait == nil || len(q.Wait.Sources) != 1 || q.Wait.Sources[0].Peer != "local" || q.Wait.Sources[0].Block != q.Block || len(q.Text) == 0 || len(q.Text) > 64<<10 {
		return api.Error(q.RequestID, "invalid_request", "prompt_wait", "one local target and bounded text required", "help")
	}
	e.promptMu.Lock()
	defer e.promptMu.Unlock()
	if blockedGuard(e) {
		return api.Error(q.RequestID, "decision_required", "session_blocked", "answer the pending permission deliberately", "status")
	}
	if e.promptActive {
		return api.Error(q.RequestID, "conflict", "prompt_busy", "previous prompt is active", "status")
	}
	if e.currentStatus() == protocol.StatusExited {
		return api.Error(q.RequestID, "conflict", "not_running", "block exited", "status")
	}
	v := m.waits.create(q)
	if v.Class != "ok" {
		return v
	}
	var accepted waitRecord
	if json.Unmarshal(v.Result, &accepted) != nil || accepted.State != "pending" {
		return api.Error(q.RequestID, "indeterminate", "wait_admission", "wait was not armed for the new turn", "wait_get")
	}
	e.statusMu.Lock()
	e.turn++
	turn := e.turn
	e.status = protocol.StatusRunning
	e.statusN++
	e.statusMu.Unlock()
	e.promptActive = true
	e.resolveWaiters(protocol.StatusRunning)
	m.s.queueStructuredEvent(e, q.Block, protocol.EventRunning, nil)
	ch, err := e.acp.Prompt(q.Text)
	if err != nil {
		m.s.failPrompt(e, q.Block, turn, nil, err, nil)
		e.promptActive = false
		return api.Error(q.RequestID, "indeterminate", "prompt_dispatch", "prompt may have been partially delivered; inspect wait", "wait_get")
	}
	done := make(chan struct{})
	e.promptDone = done
	go func() { defer close(done); m.s.completePrompt(e, q.Block, turn, nil, ch) }()
	return api.Result(q.RequestID, map[string]any{"wait_id": accepted.ID, "accepted": true, "turn": turn})
}

func (m *Modern) permissionRespondEffect(q api.Request, e *sessionEntry) api.Response {
	if e.acp == nil {
		return api.Error(q.RequestID, "unsupported", "not_acp", "permission response requires a structured block", "status")
	}
	if q.PermissionID == "" || len(q.PermissionID) > 128 || (q.Outcome != "selected" && q.Outcome != "approve" && q.Outcome != "approve_always" && q.Outcome != "reject") || q.Outcome == "selected" && (q.OptionID == "" || len(q.OptionID) > 128) || len(q.OptionID) > 128 {
		return api.Error(q.RequestID, "invalid_request", "permission_response", "request ID and valid outcome/option required", "help")
	}
	e.promptMu.Lock()
	defer e.promptMu.Unlock()
	if err := e.acp.RespondPermission(q.PermissionID, q.Outcome, q.OptionID); err != nil {
		return api.Error(q.RequestID, "conflict", "permission_unavailable", "request or option is no longer pending", "status")
	}
	if !e.acp.HasPendingPermissions() && e.currentStatus() == protocol.StatusNeedsInput {
		event := protocol.EventIdle
		if e.promptActive {
			event = protocol.EventRunning
		}
		m.s.queueStructuredEvent(e, q.Block, event, nil)
	}
	return api.Result(q.RequestID, map[string]any{"permission_id": q.PermissionID, "accepted": true})
}
