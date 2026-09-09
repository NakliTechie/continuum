package materialise

import (
	"sync"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/fleet"
	"github.com/NakliTechie/continuum/workspace"
)

type auditBarrierExec struct {
	entered chan struct{}
	release chan struct{}
}

func (b *auditBarrierExec) Run(string, []string, string, time.Duration) ([]byte, error) {
	b.entered <- struct{}{}
	<-b.release
	return nil, nil
}

func TestOvernightSharedServiceConcurrentStart(t *testing.T) {
	home := t.TempDir()
	ex := &auditBarrierExec{entered: make(chan struct{}, 2), release: make(chan struct{})}
	engines := []*Engine{New(workspace.New(home)), New(workspace.New(home))}
	svc := fleet.Service{Name: "shared-db", Run: "local-test-service"}
	var wg sync.WaitGroup
	for i, e := range engines {
		e.Exec = ex
		wg.Add(1)
		go func(i int, e *Engine) {
			defer wg.Done()
			_, _ = e.startService(svc, &workspace.Record{Name: []string{"a", "b"}[i], Path: home, Vars: map[string]string{}}, "/audit/repo")
		}(i, e)
	}
	n := 0
	timer := time.NewTimer(300 * time.Millisecond)
collect:
	for n < 2 {
		select {
		case <-ex.entered:
			n++
		case <-timer.C:
			break collect
		}
	}
	close(ex.release)
	wg.Wait()
	if n != 1 {
		t.Fatalf("shared per-repository service started %d times concurrently; want one", n)
	}
}
