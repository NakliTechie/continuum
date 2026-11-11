package server

import (
	"encoding/json"
	"path/filepath"

	"github.com/NakliTechie/continuum/internal/protocol"
	"github.com/NakliTechie/continuum/internal/repoinfo"
)

// absDir records a spawn directory only when it is absolute and clean; any
// other value leaves the session without a directory for repository reads.
func absDir(dir string) string {
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Clean(dir)
}

type gitStatusResult struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	repoinfo.GitSnapshot
}

type sessionPRResult struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	repoinfo.PRResult
}

type githubInboxResult struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	repoinfo.Inbox
}

type githubItemResult struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	repoinfo.ItemResult
}

// handleRepoRead answers the protocol 1.5 reads. It authenticates under the
// dispatcher's control lock, then runs the read on its own goroutine so a
// slow git or gh call never holds input, takeover or other sessions' control.
// A read never touches lifecycle status, control or `done`.
func (cn *conn) handleRepoRead(typ string, raw json.RawMessage) {
	var msg protocol.RepoRequest
	if err := json.Unmarshal(raw, &msg); err != nil {
		cn.sendError("", "bad_message", "malformed "+typ)
		return
	}
	if l := len(msg.RequestID); l == 0 || l > 64 {
		cn.sendError(msg.SessionID, "bad_message", typ+" requires a request_id of 1-64 characters")
		return
	}
	s := cn.srv
	var dir string
	if typ == protocol.TypeGitStatus || typ == protocol.TypeSessionPR {
		e := s.authSession(msg.SessionID, msg.SessionToken)
		if e == nil {
			cn.sendError(msg.SessionID, protocol.ErrInvalidToken, "unknown session or bad token")
			return
		}
		dir = e.cwd
	}
	go func() {
		ctx := s.ctx
		switch typ {
		case protocol.TypeGitStatus:
			_ = cn.send(gitStatusResult{Type: protocol.TypeGitStatusResult, SessionID: msg.SessionID, RequestID: msg.RequestID,
				GitSnapshot: s.repo.GitStatus(ctx, dir)})
		case protocol.TypeSessionPR:
			_ = cn.send(sessionPRResult{Type: protocol.TypeSessionPRResult, SessionID: msg.SessionID, RequestID: msg.RequestID,
				PRResult: s.repo.SessionPR(ctx, dir)})
		case protocol.TypeGitHubInbox:
			_ = cn.send(githubInboxResult{Type: protocol.TypeGitHubInboxResult, RequestID: msg.RequestID,
				Inbox: s.repo.Inbox(ctx)})
		case protocol.TypeGitHubItem:
			_ = cn.send(githubItemResult{Type: protocol.TypeGitHubItemResult, RequestID: msg.RequestID,
				ItemResult: s.repo.GetItem(ctx, msg.Kind, msg.Repo, msg.Number)})
		}
	}()
}
