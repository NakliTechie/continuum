package acp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestAuditKillDrainsPending(t *testing.T) {
	ch := make(chan *Envelope, 1)
	f, err := os.Create(filepath.Join(t.TempDir(), "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{stdin: f, cmd: exec.Command("true"), pending: map[string]chan *Envelope{"1": ch}}
	s.Kill()
	s.closeFiles()
	select {
	case <-ch:
	default:
		t.Fatal("kill prevented closeFiles from draining pending request")
	}
}

func TestAuditInvalidPermissionPreservesRequest(t *testing.T) {
	s := &Session{perms: map[string]*pendingPerm{"p": {rpcID: json.RawMessage("1"), options: []permOption{{OptionID: "allow", Kind: "allow_once"}}}}}
	if err := s.RespondPermission("p", "unknown", ""); err == nil {
		t.Fatal("expected invalid outcome")
	}
	if s.perms["p"] == nil {
		t.Fatal("invalid permission outcome destroyed retriable request")
	}
}
func TestAuditReplayOrder(t *testing.T) {
	s := &Session{pendingUpdates: [][]byte{[]byte("old1"), []byte("old2")}}
	ready := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var mu sync.Mutex
	var seen []string
	go func() {
		s.SetOnUpdate(func(b json.RawMessage) {
			mu.Lock()
			seen = append(seen, string(b))
			mu.Unlock()
			if string(b) == "old1" {
				close(ready)
				<-release
			}
		})
		close(done)
	}()
	<-ready
	liveDone := make(chan struct{})
	go func() {
		s.dispatch(&Envelope{JSONRPC: "2.0", Method: "session/update", Params: json.RawMessage(`{"live":true}`)})
		close(liveDone)
	}()
	close(release)
	<-done
	<-liveDone
	if len(seen) != 3 || seen[1] != "old2" {
		t.Fatalf("replay reordered: %v", seen)
	}
}
