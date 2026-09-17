package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/journal"
)

const maxGrants = 256

type scopedGrant struct {
	ID          string            `json:"grant_id"`
	Hash        string            `json:"token_hash"`
	Fingerprint string            `json:"fingerprint"`
	Class       string            `json:"class"`
	Blocks      []string          `json:"blocks"`
	Bindings    map[string]string `json:"block_bindings"`
	CreatedAt   string            `json:"created_at"`
	RevokedAt   string            `json:"revoked_at,omitempty"`
}

func validAccessBlock(id string) bool {
	if len(id) != 16 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			if r < 'a' || r > 'f' {
				return false
			}
		}
	}
	return true
}

func (m *Modern) findGrant(token string) (*scopedGrant, error) {
	if len(token) < 24 || len(token) > 128 {
		return nil, journal.ErrNotFound
	}
	h := sha256.Sum256([]byte(token))
	raw, err := m.Store.Object("grants", hex.EncodeToString(h[:]))
	if err != nil {
		return nil, err
	}
	var g scopedGrant
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, err
	}
	if g.Hash != hex.EncodeToString(h[:]) || g.RevokedAt != "" {
		return nil, journal.ErrNotFound
	}
	return &g, nil
}

func (m *Modern) sharing(block string) (string, error) {
	raw, err := m.Store.Object("sharing", block)
	if errors.Is(err, journal.ErrNotFound) {
		return "private", nil
	}
	if err != nil {
		return "", err
	}
	var record struct {
		Policy  string `json:"policy"`
		Started string `json:"started_at"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return "", err
	}
	if record.Policy != "private" && record.Policy != "observers" && record.Policy != "controllers" {
		return "", errors.New("invalid sharing policy")
	}
	current, err := m.Store.Block(block)
	if errors.Is(err, journal.ErrNotFound) || err == nil && current.Started != record.Started {
		return "private", nil
	}
	if err != nil {
		return "", err
	}
	return record.Policy, nil
}

func (m *Modern) grantAllows(g *scopedGrant, block string) bool {
	if g == nil || !validAccessBlock(block) {
		return false
	}
	found := false
	for _, allowed := range g.Blocks {
		if allowed == block {
			found = true
		}
	}
	if !found {
		return false
	}
	current, err := m.Store.Block(block)
	if err != nil || g.Bindings[block] != current.Started {
		return false
	}
	policy, err := m.sharing(block)
	if err != nil {
		return false
	}
	if g.Class == "observer" {
		return policy == "observers" || policy == "controllers"
	}
	return policy == "controllers"
}

func (m *Modern) audit(fingerprint, op, decision, block string) error {
	return m.Store.AppendAudit(journal.AuditEntry{Fingerprint: fingerprint, Operation: op, Decision: decision, Block: block})
}

func (m *Modern) scoped(q api.Request, g *scopedGrant) api.Response {
	deny := func() api.Response {
		_ = m.audit(g.Fingerprint, q.Operation, "denied", q.Block)
		return api.Error(q.RequestID, "access_denied", "scope", "operation or block is not shared with this grant", "status")
	}
	if q.Operation == "version" {
		if m.audit(g.Fingerprint, q.Operation, "read", "") != nil {
			return api.Error(q.RequestID, "resource_exhausted", "audit", "access audit unavailable", "status")
		}
		return m.read(q)
	}
	if q.Operation == "status" {
		if m.audit(g.Fingerprint, q.Operation, "read", q.Block) != nil {
			return api.Error(q.RequestID, "resource_exhausted", "audit", "access audit unavailable", "status")
		}
		return m.scopedStatus(q, g)
	}
	if !m.grantAllows(g, q.Block) {
		return deny()
	}
	switch q.Operation {
	case "events", "screen", "observe":
		if m.audit(g.Fingerprint, q.Operation, "read", q.Block) != nil {
			return api.Error(q.RequestID, "resource_exhausted", "audit", "access audit unavailable", "status")
		}
		return m.read(q)
	case "acquire", "renew", "release", "input", "resize", "stop":
		if g.Class == "observer" {
			return deny()
		}
	case "takeover":
		if g.Class != "moderator" {
			return deny()
		}
	default:
		return deny()
	}
	if m.s.closing.Load() || m.degraded.Load() {
		return api.Error(q.RequestID, "resource_exhausted", "capture_degraded", "daemon cannot accept control", "status")
	}
	if m.audit(g.Fingerprint, q.Operation, "allowed", q.Block) != nil {
		return api.Error(q.RequestID, "resource_exhausted", "audit", "access audit unavailable", "status")
	}
	return m.mutate(q)
}

func (m *Modern) scopedStatus(q api.Request, g *scopedGrant) api.Response {
	blocks, err := m.Store.Blocks()
	if err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "store_read", "cannot read state", "status")
	}
	visible := make([]journal.Block, 0)
	for _, b := range blocks {
		if m.grantAllows(g, b.ID) {
			visible = append(visible, b)
		}
	}
	active := 0
	for _, b := range visible {
		if b.State == "active" {
			active++
		}
	}
	page := make([]journal.Block, 0, 20)
	next := ""
	more := false
	for _, b := range visible {
		if q.Block != "" && q.Block != b.ID || b.ID <= q.Cursor {
			continue
		}
		if len(page) == 20 {
			more = true
			break
		}
		page = append(page, b)
		next = b.ID
	}
	return api.Result(q.RequestID, map[string]any{"host_id": m.Store.Host, "protocol": "continuum.local-alpha.1", "capabilities": m.capabilities(), "contract_version": ContractVersion, "blocks": page, "total": len(visible), "active": active, "truncated": more, "next_cursor": next, "capture_degraded": m.degraded.Load(), "observed_at": time.Now().UTC().Format(time.RFC3339Nano)})
}

func (m *Modern) admin(q api.Request) api.Response {
	if q.Operation == "grant_list" {
		all, err := m.Store.Objects("grants")
		if err != nil {
			return api.Error(q.RequestID, "resource_exhausted", "grant_store", "cannot list grants", "status")
		}
		out := make([]map[string]any, 0, len(all))
		for _, raw := range all {
			var g scopedGrant
			if json.Unmarshal(raw, &g) != nil {
				return api.Error(q.RequestID, "indeterminate", "grant_store", "grant registry is corrupt", "status")
			}
			out = append(out, map[string]any{"grant_id": g.ID, "fingerprint": g.Fingerprint, "class": g.Class, "blocks": g.Blocks, "created_at": g.CreatedAt, "revoked_at": g.RevokedAt})
		}
		return api.Result(q.RequestID, out)
	}
	if q.Operation == "audit_list" {
		page, next, first, gap, err := m.Store.AuditPage(q.After)
		if err != nil {
			return api.Error(q.RequestID, "resource_exhausted", "audit_store", "cannot read audit", "status")
		}
		if gap {
			return api.Error(q.RequestID, "history_gap", "audit_cursor", "audit entries before first_available were rotated", "audit_list")
		}
		return api.Result(q.RequestID, map[string]any{"entries": page, "next_cursor": next, "first_available": first})
	}
	if q.Operation == "share_get" {
		if !validAccessBlock(q.Block) {
			return api.Error(q.RequestID, "invalid_request", "block_id", "valid block ID required", "help")
		}
		policy, err := m.sharing(q.Block)
		if err != nil {
			return api.Error(q.RequestID, "resource_exhausted", "sharing_store", "cannot read sharing", "status")
		}
		return api.Result(q.RequestID, map[string]any{"block_id": q.Block, "policy": policy})
	}
	if q.RequestID == "" || len(q.RequestID) > 128 {
		return api.Error(q.RequestID, "invalid_request", "request_id", "stable request ID required", "help")
	}
	encoded, _ := json.Marshal(q)
	hash := sha256.Sum256(encoded)
	digest := hex.EncodeToString(hash[:])
	old, err := m.Store.Begin(q.RequestID, digest)
	if err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "intent_store", "cannot commit admin intent", "status")
	}
	if old != nil {
		if old.Digest != digest {
			return api.Error(q.RequestID, "conflict", "request_reused", "request ID names another admin operation", "status")
		}
		if len(old.Result) == 0 {
			return api.Error(q.RequestID, "indeterminate", "unresolved_intent", "admin effect may have occurred; inspect before retrying", "status")
		}
		var v api.Response
		if json.Unmarshal(old.Result, &v) != nil {
			return api.Error(q.RequestID, "indeterminate", "intent_store", "saved result is invalid", "status")
		}
		return v
	}
	var v api.Response
	switch q.Operation {
	case "grant_create":
		v = m.createGrant(q)
	case "grant_revoke":
		v = m.revokeGrant(q)
	case "share_set":
		v = m.setSharing(q)
	default:
		v = api.Error(q.RequestID, "unsupported", "operation", "unsupported admin operation", "help")
	}
	saved := v
	if q.Operation == "grant_create" && v.Class == "ok" {
		saved = api.Error(q.RequestID, "indeterminate", "token_not_replayed", "grant was created but its secret was shown only once; list or revoke by ID", "grant_list")
	}
	raw, _ := json.Marshal(saved)
	if err := m.Store.Finish(q.RequestID, digest, raw); err != nil {
		m.degraded.Store(true)
		return api.Error(q.RequestID, "indeterminate", "result_commit_failed", "admin effect may have occurred", "status")
	}
	return v
}

func (m *Modern) createGrant(q api.Request) api.Response {
	spec := q.Grant
	if spec == nil || (spec.Class != "observer" && spec.Class != "controller" && spec.Class != "moderator") || len(spec.Blocks) < 1 || len(spec.Blocks) > 64 {
		return api.Error(q.RequestID, "invalid_request", "grant", "class and 1..64 blocks required", "help")
	}
	seen := map[string]bool{}
	bindings := map[string]string{}
	for _, id := range spec.Blocks {
		if !validAccessBlock(id) || seen[id] {
			return api.Error(q.RequestID, "invalid_request", "grant_block", "unique valid block IDs required", "help")
		}
		seen[id] = true
		block, err := m.Store.Block(id)
		if err != nil {
			return api.Error(q.RequestID, "invalid_request", "grant_block", "block is unavailable", "status")
		}
		bindings[id] = block.Started
	}
	if err := m.audit("root", "grant_create", "intent", ""); err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "audit", "grant was not created", "status")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "entropy", "cannot generate grant token", "status")
	}
	token := "ctg_" + base64.RawURLEncoding.EncodeToString(secret)
	h := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(h[:])
	g := scopedGrant{ID: journal.ID(), Hash: key, Fingerprint: key[:12], Class: spec.Class, Blocks: append([]string{}, spec.Blocks...), Bindings: bindings, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := m.Store.UpdateObject("grants", key, maxGrants, func(old json.RawMessage, _ uint64) (json.RawMessage, error) {
		if old != nil {
			return nil, errors.New("token collision")
		}
		return json.Marshal(g)
	}); err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "grant_store", "grant did not commit", "status")
	}
	return api.Result(q.RequestID, map[string]any{"grant_id": g.ID, "token": token, "fingerprint": g.Fingerprint, "class": g.Class, "blocks": g.Blocks})
}

func (m *Modern) revokeGrant(q api.Request) api.Response {
	if q.GrantID == "" || len(q.GrantID) > 128 {
		return api.Error(q.RequestID, "invalid_request", "grant_id", "grant ID required", "help")
	}
	all, err := m.Store.Objects("grants")
	if err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "grant_store", "cannot inspect grants", "status")
	}
	key := ""
	for k, raw := range all {
		var g scopedGrant
		if json.Unmarshal(raw, &g) == nil && g.ID == q.GrantID {
			key = k
			break
		}
	}
	if key == "" {
		return api.Error(q.RequestID, "conflict", "grant_unknown", "grant not found", "grant_list")
	}
	if err := m.audit("root", "grant_revoke", "intent", ""); err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "audit", "revocation did not start", "status")
	}
	err = m.Store.UpdateObject("grants", key, maxGrants, func(old json.RawMessage, _ uint64) (json.RawMessage, error) {
		var g scopedGrant
		if json.Unmarshal(old, &g) != nil {
			return nil, errors.New("corrupt grant")
		}
		if g.RevokedAt != "" {
			return old, nil
		}
		g.RevokedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return json.Marshal(g)
	})
	if err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "grant_store", "revocation did not commit", "status")
	}
	return api.Result(q.RequestID, map[string]any{"grant_id": q.GrantID, "revoked": true})
}

func (m *Modern) setSharing(q api.Request) api.Response {
	if !validAccessBlock(q.Block) || (q.Sharing != "private" && q.Sharing != "observers" && q.Sharing != "controllers") {
		return api.Error(q.RequestID, "invalid_request", "sharing", "block and private|observers|controllers required", "help")
	}
	block, err := m.Store.Block(q.Block)
	if err != nil {
		return api.Error(q.RequestID, "conflict", "block_unavailable", "block is unavailable", "status")
	}
	if err := m.audit("root", "share_set", "intent", q.Block); err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "audit", "sharing was not changed", "status")
	}
	err = m.Store.UpdateObject("sharing", q.Block, 1024, func(_ json.RawMessage, _ uint64) (json.RawMessage, error) {
		return json.Marshal(map[string]string{"policy": q.Sharing, "started_at": block.Started})
	})
	if err != nil {
		return api.Error(q.RequestID, "resource_exhausted", "sharing_store", "sharing did not commit", "status")
	}
	return api.Result(q.RequestID, map[string]any{"block_id": q.Block, "policy": q.Sharing})
}
