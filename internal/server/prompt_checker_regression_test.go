package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/journal"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const checkerAgent = `import sys,json,time,os
for line in sys.stdin:
 m=json.loads(line); method=m.get('method'); r={'jsonrpc':'2.0','id':m.get('id')}
 if method=='initialize': r['result']={'protocolVersion':1}
 elif method=='session/new': r['result']={'sessionId':'checker-session'}
 elif method=='session/prompt':
  text=m['params']['prompt'][0]['text']
  if text=='exit': os._exit(17)
  if text=='swallow': continue
  if text.startswith('slow'): time.sleep(.22)
  if text=='error': r['error']={'code':-32001,'message':'CHECKER_FAILURE'}
  elif text=='empty': pass
  elif text=='null': r['result']=None
  elif text=='array': r['result']=[]
  elif text=='missing_stop': r['result']={}
  elif text=='invalid_stop': r['result']={'stopReason':7}
  else: r['result']={'stopReason':'end_turn'}
 else: continue
 print(json.dumps(r),flush=True)
`

func checkerServer(t *testing.T, agent *config.Agent) (*Server, *httptest.Server, *journal.Store, string) {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	a := config.Agent{Command: py, Transports: []string{"acp"}, ACPArgs: []string{"-u", "-c", checkerAgent}}
	if agent != nil {
		a = *agent
	}
	disabled := ""
	s := New(&config.Config{Name: "checker", Listen: "127.0.0.1:0", Tmux: "off", CaptureDir: &disabled, RegistrationToken: "checker-registration", Agents: map[string]config.Agent{"fake": a}})
	dir := filepath.Join(t.TempDir(), "journal")
	store, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.EnableModern(store, "checker-observer")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		s.StopAll()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if err := s.Drain(ctx); err != nil {
			t.Error(err)
		}
		ts.Close()
		store.Close()
	})
	return s, ts, store, dir
}
func checkerRead(t *testing.T, c *websocket.Conn) frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	f := frame{}
	if err := wsjson.Read(ctx, c, &f); err != nil {
		t.Fatal(err)
	}
	return f
}
func checkerUntil(t *testing.T, c *websocket.Conn, p func(frame) bool) frame {
	t.Helper()
	for i := 0; i < 100; i++ {
		f := checkerRead(t, c)
		if p(f) {
			return f
		}
	}
	t.Fatal("frame not found")
	return nil
}
func checkerConnect(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	c := dialWS(t, ts)
	checkerUntil(t, c, func(f frame) bool { return f["type"] == "hello" })
	sendMsg(t, c, msg{"type": "register", "registration_token": "checker-registration"})
	checkerUntil(t, c, func(f frame) bool { return f["type"] == "registered" })
	return c
}
func checkerSpawn(t *testing.T, ts *httptest.Server, args []string) (*websocket.Conn, string, string) {
	t.Helper()
	c := checkerConnect(t, ts)
	sendMsg(t, c, msg{"type": "spawn", "agent": "fake", "transport": "acp", "cwd": t.TempDir(), "args": args})
	f := checkerUntil(t, c, func(f frame) bool { return f["type"] == "spawned" || f["type"] == "error" })
	if f["type"] != "spawned" {
		t.Fatalf("spawn: %v", f)
	}
	return c, f["session_id"].(string), f["session_token"].(string)
}
func checkerPrompt(t *testing.T, c *websocket.Conn, sid, tok, text, id, state string, ms int) {
	t.Helper()
	m := msg{"type": "prompt", "session_id": sid, "session_token": tok, "text": text}
	if id != "" {
		m["wait"] = msg{"wait_id": id, "until": []string{state}, "timeout_ms": ms}
	}
	sendMsg(t, c, m)
}

func TestCheckerTurnIsolationAndBusy(t *testing.T) {
	s, ts, _, _ := checkerServer(t, nil)
	c, sid, tok := checkerSpawn(t, ts, nil)
	for _, prior := range []string{"done", "idle"} {
		checkerPrompt(t, c, sid, tok, "ok", "", "", 0)
		checkerUntil(t, c, func(f frame) bool { return f["event"] == "done" })
		if prior == "idle" {
			sendMsg(t, c, msg{"type": "seen", "session_id": sid, "session_token": tok})
			checkerUntil(t, c, func(f frame) bool { return f["event"] == "idle" })
		}
		begin := time.Now()
		checkerPrompt(t, c, sid, tok, "slow-one", "own", "done", 2000)
		checkerPrompt(t, c, sid, tok, "must-be-refused", "bad", "done", 100)
		busy, own, done := false, false, false
		for !busy || !own || !done {
			f := checkerRead(t, c)
			if f["event"] == "done" {
				done = true
			}
			if f["code"] == "prompt_busy" {
				busy = true
			}
			if f["type"] == "waited" {
				if f["wait_id"] != "own" || f["timed_out"] != false || f["state"] != "done" {
					t.Fatalf("invalid wait %v", f)
				}
				if time.Since(begin) < 180*time.Millisecond {
					t.Fatal("prior turn satisfied new wait")
				}
				own = true
			}
		}
		e := s.entry(sid)
		e.statusMu.Lock()
		turn := e.turn
		e.statusMu.Unlock()
		if turn != map[string]uint64{"done": 2, "idle": 4}[prior] {
			t.Fatalf("busy prompt changed generation: %d", turn)
		}
	}
}

func TestCheckerMalformedWaitHasNoEffect(t *testing.T) {
	s, ts, _, _ := checkerServer(t, nil)
	c, sid, tok := checkerSpawn(t, ts, nil)
	checkerPrompt(t, c, sid, tok, "ok", "", "", 0)
	checkerUntil(t, c, func(f frame) bool { return f["event"] == "done" })
	checkerPrompt(t, c, sid, tok, "error", "invalid", "invented", 500)
	f := checkerUntil(t, c, func(f frame) bool { return f["type"] == "error" })
	if f["code"] != "bad_wait" {
		t.Fatalf("%v", f)
	}
	e := s.entry(sid)
	e.statusMu.Lock()
	turn, status := e.turn, e.status
	e.statusMu.Unlock()
	if turn != 1 || status != "done" {
		t.Fatalf("malformed prompt changed state turn=%d status=%s", turn, status)
	}
	sendMsg(t, c, msg{"type": "wait", "session_id": sid, "session_token": tok, "until": []string{"done"}, "wait_id": "ordinary", "timeout_ms": 500})
	f = checkerUntil(t, c, func(f frame) bool { return f["type"] == "waited" })
	if f["timed_out"] != false || f["state"] != "done" {
		t.Fatal(f)
	}
}

func TestCheckerUnmatchedWaitCannotBorrowLaterTurn(t *testing.T) {
	_, ts, _, _ := checkerServer(t, nil)
	c, sid, tok := checkerSpawn(t, ts, nil)
	checkerPrompt(t, c, sid, tok, "ok", "old", "needs_input", 240)
	checkerUntil(t, c, func(f frame) bool { return f["event"] == "done" })
	checkerPrompt(t, c, sid, tok, "swallow", "new", "done", 500)
	sendMsg(t, c, msg{"type": "report_status", "session_id": sid, "session_token": tok, "state": "needs_input"})
	f := checkerUntil(t, c, func(f frame) bool { return f["type"] == "waited" })
	if f["wait_id"] != "old" || f["timed_out"] != true {
		t.Fatalf("old wait borrowed later turn %v", f)
	}
}

func TestCheckerFailuresDurableAndReplayable(t *testing.T) {
	for _, mode := range []string{"error", "empty", "null", "array", "missing_stop", "invalid_stop"} {
		t.Run(mode, func(t *testing.T) {
			_, ts, store, dir := checkerServer(t, nil)
			c, sid, tok := checkerSpawn(t, ts, nil)
			checkerPrompt(t, c, sid, tok, mode, "failed-wait", "done", 1500)
			errFrame, waitErr, unknown := frame(nil), frame(nil), false
			for errFrame == nil || waitErr == nil || !unknown {
				f := checkerRead(t, c)
				if f["event"] == "done" || f["event"] == "idle" || f["type"] == "waited" {
					t.Fatalf("failure signalled success: %v", f)
				}
				if f["code"] == "acp_prompt_failed" {
					errFrame = f
				}
				if f["code"] == "prompt_wait_failed" {
					waitErr = f
				}
				if f["event"] == "unknown" {
					unknown = true
				}
			}
			if errFrame["turn_id"] != float64(1) || waitErr["turn_id"] != float64(1) || waitErr["wait_id"] != "failed-wait" {
				t.Fatalf("bad correlation %v %v", errFrame, waitErr)
			}
			if mode == "error" && (errFrame["rpc_code"] != float64(-32001) || !strings.Contains(errFrame["message"].(string), "CHECKER_FAILURE")) {
				t.Fatal(errFrame)
			}
			c.CloseNow()
			again := checkerConnect(t, ts)
			sendMsg(t, again, msg{"type": "attach", "session_id": sid})
			attached := checkerUntil(t, again, func(f frame) bool { return f["type"] == "attached" })
			tok = attached["session_token"].(string)
			replay := checkerUntil(t, again, func(f frame) bool { return f["code"] == "acp_prompt_failed" })
			if replay["turn_id"] != float64(1) {
				t.Fatal(replay)
			}
			page, err := store.Read(0, sid)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, ev := range page.Events {
				if ev.Type == "done" {
					t.Fatal("journal claimed done")
				}
				if ev.Type == "acp_prompt_failed" {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("durable failures=%d", n)
			}
			checkerPrompt(t, again, sid, tok, "ok", "recovery", "done", 1000)
			f := checkerUntil(t, again, func(f frame) bool { return f["type"] == "waited" })
			if f["state"] != "done" || f["timed_out"] != false {
				t.Fatal(f)
			}
			// Copy the on-disk database once the successful turn has been recorded, then reopen the copy.
			saved := filepath.Join(t.TempDir(), "saved")
			if err := os.Mkdir(saved, 0700); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".db") {
					b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(saved, entry.Name()), b, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			reopened, err := journal.Open(saved)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			p, err := reopened.Read(0, sid)
			if err != nil {
				t.Fatal(err)
			}
			n = 0
			for _, ev := range p.Events {
				if ev.Type == "acp_prompt_failed" {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("reopened failures=%d", n)
			}
		})
	}
}

func TestCheckerExitWithoutResponseAlwaysRecordsFailure(t *testing.T) {
	for i := 0; i < 15; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			_, ts, store, _ := checkerServer(t, nil)
			c, sid, tok := checkerSpawn(t, ts, nil)
			sendMsg(t, c, msg{"type": "wait", "session_id": sid, "session_token": tok, "wait_id": "ordinary-exit", "until": []string{"needs_input"}, "timeout_ms": 1500})
			checkerPrompt(t, c, sid, tok, "exit", "prompt-exit", "done", 1500)
			exited, ordinary, promptTerminal := false, false, false
			for !exited || !ordinary || !promptTerminal {
				f := checkerRead(t, c)
				if f["event"] == "done" || f["event"] == "idle" {
					t.Fatalf("exit claimed successful completion %v", f)
				}
				if f["event"] == "exited" {
					exited = true
				}
				if f["type"] == "waited" {
					if f["state"] != "exited" || f["timed_out"] != false {
						t.Fatalf("exit wait %v", f)
					}
					if f["wait_id"] == "ordinary-exit" {
						ordinary = true
					}
					if f["wait_id"] == "prompt-exit" {
						promptTerminal = true
					}
				}
				if f["code"] == "prompt_wait_failed" {
					promptTerminal = true
				}
			}
			page, err := store.Read(0, sid)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			types := []string{}
			for _, ev := range page.Events {
				types = append(types, ev.Type)
				if ev.Type == "acp_prompt_failed" {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("agent exited without a response; durable prompt failures=%d journal=%v", n, types)
			}
		})
	}
}

func TestCheckerStrictEmptyArgvAndSliceIsolation(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "strict-agent")
	if err := os.WriteFile(script, []byte("#!"+py+"\nimport sys\nassert len(sys.argv)==1,sys.argv\n"+checkerAgent), 0700); err != nil {
		t.Fatal(err)
	}
	a := config.Agent{Command: script, Transports: []string{"acp"}, ACPArgs: []string{}}
	_, ts, _, _ := checkerServer(t, &a)
	c, sid, tok := checkerSpawn(t, ts, nil)
	checkerPrompt(t, c, sid, tok, "ok", "strict", "done", 1000)
	f := checkerUntil(t, c, func(f frame) bool { return f["type"] == "waited" })
	if f["state"] != "done" {
		t.Fatal(f)
	}
	shared := make([]string, 3, 8)
	copy(shared, []string{"-u", "-c", checkerAgent})
	sentinel := shared[:cap(shared)]
	for i := 3; i < len(sentinel); i++ {
		sentinel[i] = "do-not-mutate"
	}
	a = config.Agent{Command: py, Transports: []string{"acp"}, ACPArgs: shared}
	_, ts2, _, _ := checkerServer(t, &a)
	checkerSpawn(t, ts2, []string{"extra"})
	for i := 3; i < len(sentinel); i++ {
		if sentinel[i] != "do-not-mutate" {
			t.Fatalf("shared argv mutated at %d: %q", i, sentinel[i])
		}
	}
}

func TestCheckerResponseThenImmediateExitKeepsOutcome(t *testing.T) {
	for _, mode := range []string{"ok", "error"} {
		t.Run(mode, func(t *testing.T) {
			py, err := exec.LookPath("python3")
			if err != nil {
				t.Fatal(err)
			}
			script := strings.Replace(checkerAgent, " print(json.dumps(r),flush=True)", " print(json.dumps(r),flush=True)\n if method=='session/prompt': os._exit(0)", 1)
			agent := config.Agent{Command: py, Transports: []string{"acp"}, ACPArgs: []string{"-u", "-c", script}}
			_, ts, store, _ := checkerServer(t, &agent)
			c, sid, tok := checkerSpawn(t, ts, nil)
			checkerPrompt(t, c, sid, tok, mode, "terminal-wait", "done", 1000)
			exited, terminal := false, false
			outcomes := []string{}
			for !exited || !terminal {
				f := checkerRead(t, c)
				if f["event"] == "done" {
					if exited || mode == "error" {
						t.Fatalf("unexpected done %v", f)
					}
					outcomes = append(outcomes, "done")
				}
				if f["event"] == "exited" {
					exited = true
				}
				if f["type"] == "waited" {
					if mode != "ok" || f["wait_id"] != "terminal-wait" || f["state"] != "done" || f["timed_out"] != false {
						t.Fatalf("outcome mismatch %v", f)
					}
					terminal = true
				}
				if f["code"] == "acp_prompt_failed" {
					if mode != "error" || f["rpc_code"] != float64(-32001) || !strings.Contains(f["message"].(string), "CHECKER_FAILURE") {
						t.Fatalf("response was replaced by an exit error: %v", f)
					}
					outcomes = append(outcomes, "error")
				}
				if f["code"] == "prompt_wait_failed" {
					if mode != "error" || f["wait_id"] != "terminal-wait" || f["turn_id"] != float64(1) {
						t.Fatal(f)
					}
					terminal = true
				}
			}
			if len(outcomes) != 1 || outcomes[0] != map[string]string{"ok": "done", "error": "error"}[mode] {
				t.Fatalf("outcomes=%v", outcomes)
			}
			page, err := store.Read(0, sid)
			if err != nil {
				t.Fatal(err)
			}
			last := ""
			for _, ev := range page.Events {
				if last == "exited" {
					t.Fatalf("post-exit event %s", ev.Type)
				}
				last = ev.Type
			}
			if last != "exited" {
				t.Fatalf("last event %q", last)
			}
		})
	}
}

func TestCheckerPromptWriteFailureCorrelates(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Replace(checkerAgent, "elif method=='session/new': r['result']", "elif method=='session/new': os.close(0); r['result']", 1)
	script = strings.Replace(script, " print(json.dumps(r),flush=True)", " print(json.dumps(r),flush=True)\n if method=='session/new': time.sleep(1); os._exit(0)", 1)
	agent := config.Agent{Command: py, Transports: []string{"acp"}, ACPArgs: []string{"-u", "-c", script}}
	_, ts, store, _ := checkerServer(t, &agent)
	c, sid, tok := checkerSpawn(t, ts, nil)
	checkerPrompt(t, c, sid, tok, "ok", "write-failed", "done", 1000)
	failure, correlated := false, false
	for !failure || !correlated {
		f := checkerRead(t, c)
		if f["type"] == "waited" || f["event"] == "done" {
			t.Fatalf("write failure signalled success %v", f)
		}
		if f["code"] == "acp_prompt_failed" {
			failure = true
		}
		if f["code"] == "prompt_wait_failed" {
			if f["wait_id"] != "write-failed" || f["turn_id"] != float64(1) {
				t.Fatal(f)
			}
			correlated = true
		}
	}
	page, err := store.Read(0, sid)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range page.Events {
		if ev.Type == "acp_prompt_failed" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("durable write failure count=%d", n)
	}
}
