// Package cli is the human and machine client of the same authorized host API.
package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/journal"
	"github.com/NakliTechie/continuum/internal/server"
	"github.com/charmbracelet/x/ansi"
)

// Version may be stamped with the source revision by local artifact builds.
var Version = "0.1.0-alpha.2-dev"

const help = `Continuum — work that outlives its clients

First use (two terminals):
  continuum serve
  continuum open -- /bin/sh -c 'printf "hello\n"; sleep 30'
  continuum status
  continuum events --block BLOCK_ID --follow

Commands:
  serve                    run a foreground local daemon; Ctrl-C stops its work
  status                   inspect up to 20 recorded blocks without taking control
  open -- COMMAND ARGS     launch a PTY; arguments are preserved exactly
  attach --block ID        interactive screen-v1 terminal; Ctrl-] detaches
  screen --block ID        inspect a current/final screen without taking control
  events --block ID        replay bounded recorded events; --follow keeps watching
  acquire --block ID       acquire 60-second input control and save it privately
  takeover --block ID      explicitly fence an existing controller
  renew --block ID         extend your current saved control lease
  release --block ID       release your saved control lease
  input --block ID         send stdin bytes using your saved lease
  resize --block ID --cols N --rows N
  stop --block ID          stop work using your saved lease
  version                  show build version

Common flags: --state ABSOLUTE_DIR (default: user config directory/continuum),
  --json (machine envelope), --request-id ID (reuse only for the same mutation).
Events: --after CURSOR, --follow, --text (decode PTY bytes; trusted output only).
Serve: --listen 127.0.0.1:PORT (default: random free port), --origin URL.
Observation: --observer uses the read-only observer credential.
Terminal: open --terminal screen-v1 -- COMMAND opts into server-owned screens.
Attach: --observer is read-only; --takeover explicitly replaces a controller.
Keyboard only; complex Unicode and advanced TUI compatibility are experimental.

This alpha recovers records after daemon restart; running processes do not survive.
Menagerie can use the same daemon's legacy WebSocket endpoint with operator.token.
No service is installed. Remote listeners and untrusted multi-user hosting are unsupported.
`

func defaultState() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "continuum")
}
func privateRead(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("credential must be a private regular file")
	}
	return os.ReadFile(path)
}
func credential(dir, name string) (string, error) {
	p := filepath.Join(dir, name)
	b, err := privateRead(p)
	if err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	v := journal.ID() + journal.ID()
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(v + "\n")
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	return v, closeErr
}
func atomicWrite(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".continuum-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), path)
}

func Run(args []string, in io.Reader, out, diag io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(out, help)
		return 0
	}
	if args[0] == "version" {
		fmt.Fprintln(out, "continuum "+Version)
		return 0
	}
	command := args[0]
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(diag)
	state := f.String("state", defaultState(), "private state directory")
	machine := f.Bool("json", false, "JSON response")
	block := f.String("block", "", "block ID")
	cursor := f.String("cursor", "", "status page cursor")
	after := f.Uint64("after", 0, "history cursor")
	follow := f.Bool("follow", false, "watch events")
	plain := f.Bool("text", false, "decode trusted PTY bytes")
	observer := f.Bool("observer", false, "read-only credential")
	request := f.String("request-id", "", "stable mutation ID")
	cwd := f.String("cwd", "", "working directory")
	cols := f.Int("cols", 80, "terminal columns")
	rows := f.Int("rows", 24, "terminal rows")
	listen := f.String("listen", "127.0.0.1:0", "loopback address")
	origin := f.String("origin", "", "additional trusted Menagerie origin")
	profile := f.String("terminal", "", "open terminal profile: screen-v1 (experimental)")
	take := f.Bool("takeover", false, "explicitly take control when attaching")
	if command == "attach" {
		f.Usage = func() { fmt.Fprint(diag, attachHelp) }
		for _, arg := range args[1:] {
			if arg == "--help" || arg == "-h" {
				fmt.Fprint(out, attachHelp)
				return 0
			}
		}
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, help)
			return 0
		}
		return 2
	}
	if !filepath.IsAbs(*state) {
		fmt.Fprintln(diag, "--state must be an absolute directory")
		return 2
	}
	if command == "serve" {
		if f.NArg() != 0 {
			fmt.Fprintln(diag, "serve accepts flags only")
			return 2
		}
		if err := serve(*state, *listen, *origin, diag); err != nil {
			fmt.Fprintln(diag, err)
			return 5
		}
		return 0
	}
	switch command {
	case "status", "open", "screen", "attach", "events", "acquire", "takeover", "renew", "release", "input", "resize", "stop":
	default:
		fmt.Fprintln(diag, "unknown command; run continuum help")
		return 2
	}
	if f.NArg() != 0 && command != "open" {
		fmt.Fprintln(diag, "unexpected arguments; use --block ID")
		return 2
	}
	if *plain && (command != "events" || *machine) {
		fmt.Fprintln(diag, "--text is for events and cannot combine with --json")
		return 2
	}
	if *follow && command != "events" {
		fmt.Fprintln(diag, "--follow is for events")
		return 2
	}
	if *profile != "" && command != "open" {
		fmt.Fprintln(diag, "--terminal is for open")
		return 2
	}
	if *take && command != "attach" {
		fmt.Fprintln(diag, "--takeover is for attach")
		return 2
	}
	if command == "attach" {
		if *machine || *request != "" {
			fmt.Fprintln(diag, "attach is interactive; use screen --json for machine output")
			return 2
		}
		return attach(*state, *block, *observer, *take, in, out, diag)
	}
	q := api.Request{Terminal: *profile, Cursor: *cursor, Operation: command, RequestID: *request, Block: *block, After: *after, Cols: *cols, Rows: *rows}
	mut := !readOperation(command)
	if mut && q.RequestID == "" {
		q.RequestID = journal.ID()
	}
	if command == "open" {
		q.Args = f.Args()
		q.Cwd = *cwd
		if q.Cwd == "" {
			q.Cwd, _ = os.Getwd()
		}
		if len(q.Args) == 0 {
			return render(api.Error(q.RequestID, "invalid_request", "command", "open requires -- COMMAND ARGS", "help"), *machine, out, diag)
		}
	}
	if command == "input" {
		data, err := io.ReadAll(io.LimitReader(in, 64<<10+1))
		if err != nil || len(data) > 64<<10 {
			return render(api.Error(q.RequestID, "invalid_request", "input_size", "stdin must be at most 64 KiB", "help"), *machine, out, diag)
		}
		if !utf8.Valid(data) {
			return render(api.Error(q.RequestID, "invalid_request", "input_encoding", "input supports UTF-8 text only", "help"), *machine, out, diag)
		}
		q.Data = string(data)
	}
	leasePath := ""
	if command == "input" || command == "resize" || command == "stop" || command == "release" || command == "renew" || command == "acquire" || command == "takeover" {
		if len(*block) != 16 {
			return render(api.Error(q.RequestID, "invalid_request", "block_id", "use the full block ID from status or open", "status"), *machine, out, diag)
		}
		for _, r := range *block {
			if !strings.ContainsRune("0123456789abcdef", r) {
				return render(api.Error(q.RequestID, "invalid_request", "block_id", "invalid block ID", "status"), *machine, out, diag)
			}
		}
		leasePath = filepath.Join(*state, "lease-"+*block)
		if command != "acquire" && command != "takeover" {
			b, err := privateRead(leasePath)
			if err != nil {
				return render(api.Error(q.RequestID, "conflict", "control_required", "no saved lease; run acquire --block "+*block, "acquire"), *machine, out, diag)
			}
			q.Lease = strings.TrimSpace(string(b))
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	for {
		v := call(ctx, *state, *observer, q)
		if v.Class == "ok" && (command == "acquire" || command == "takeover") {
			var result struct {
				Lease   string `json:"lease"`
				Expires string `json:"expires_at"`
			}
			if err := json.Unmarshal(v.Result, &result); err != nil || result.Lease == "" {
				v = api.Error(q.RequestID, "indeterminate", "invalid_lease", "server returned an invalid lease", "status")
			} else if err := atomicWrite(leasePath, []byte(result.Lease)); err != nil {
				v = api.Error(q.RequestID, "indeterminate", "lease_save_failed", "control acquired but could not save lease; explicit takeover can recover", "takeover")
			} else {
				v.Result, _ = json.Marshal(map[string]any{"block_id": *block, "expires_at": result.Expires, "lease_saved": true})
			}
		}
		if v.Class == "ok" && command == "release" {
			_ = os.Remove(leasePath)
		}
		if command == "screen" && v.Class == "ok" && !*machine {
			frame, err := decodeScreen(v, *block)
			if err != nil {
				return render(api.Error(q.RequestID, "indeterminate", "invalid_screen", "invalid screen response", "status"), false, out, diag)
			}
			fmt.Fprintf(out, "Block %s • %s • %dx%d • revision %d (volatile)\n", frame.Block, frame.State, frame.Frame.Cols, frame.Frame.Rows, frame.Frame.Revision)
			for _, line := range frame.Frame.ANSI {
				fmt.Fprintln(out, strings.TrimRight(ansi.Strip(displayRow(line, frame.Frame.Cols)), " "))
			}
			return 0
		}
		if command != "events" || v.Class != "ok" {
			return render(v, *machine, out, diag)
		}
		var p journal.Page
		if err := json.Unmarshal(v.Result, &p); err != nil {
			return 8
		}
		for _, e := range p.Events {
			if *plain {
				if e.Type == "output" {
					var data struct {
						Data string `json:"data"`
					}
					_ = json.Unmarshal(e.Payload, &data)
					raw, err := base64.StdEncoding.DecodeString(data.Data)
					if err != nil {
						return 8
					}
					if _, err = out.Write(raw); err != nil {
						return 5
					}
				}
			} else {
				if err := json.NewEncoder(out).Encode(e); err != nil {
					return 5
				}
			}
		}
		q.After = p.Next
		if p.Next < p.Last {
			continue
		}
		if !*follow {
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func render(v api.Response, machine bool, out, diag io.Writer) int {
	if machine {
		_ = json.NewEncoder(out).Encode(v)
	} else if v.Class != "ok" {
		fmt.Fprintf(diag, "%s: %s\nNext: continuum %s\n", v.Code, v.Message, v.Next.Operation)
		if v.RequestID != "" {
			fmt.Fprintf(diag, "Request ID: %s (reuse --request-id %s only with the same command and arguments)\n", v.RequestID, v.RequestID)
		}
		if len(v.Result) > 0 {
			fmt.Fprintln(diag, string(v.Result))
		}
	} else {
		fmt.Fprintln(out, string(v.Result))
	}
	return api.Exit(v.Class)
}
func call(ctx context.Context, dir string, observer bool, q api.Request) api.Response {
	fail := func(code, msg string) api.Response { return api.Error(q.RequestID, "unreachable", code, msg, "serve") }
	endpoint, err := privateRead(filepath.Join(dir, "endpoint"))
	if err != nil {
		return fail("daemon_not_running", "start continuum serve with the same --state directory")
	}
	addr := strings.TrimSpace(string(endpoint))
	host, port, err := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	p, pe := strconv.Atoi(port)
	if err != nil || ip == nil || !ip.IsLoopback() || pe != nil || p < 1 || p > 65535 {
		return fail("invalid_endpoint", "endpoint must name a loopback listener")
	}
	name := "operator.token"
	if observer {
		name = "observer.token"
	}
	token, err := privateRead(filepath.Join(dir, name))
	if err != nil {
		return api.Error(q.RequestID, "access_denied", "credential", "could not read private credential", "help")
	}
	b, _ := json.Marshal(q)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+addr+"/v1", bytes.NewReader(b))
	if err != nil {
		return fail("request", "could not construct request")
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		if !readOperation(q.Operation) {
			return api.Error(q.RequestID, "indeterminate", "transport_lost", "request may have executed; retry with the same --request-id to reconcile", "status")
		}
		return fail("daemon_unreachable", "daemon is unreachable; inspect or start it")
	}
	defer resp.Body.Close()
	var v api.Response
	limit := int64(1 << 20)
	if q.Operation == "screen" {
		limit = 32 << 20
	} else if q.Operation == "events" {
		limit = 16 << 20
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(&v); err != nil || v.Version != 1 || v.Class == "" {
		return api.Error(q.RequestID, "indeterminate", "invalid_response", "response contract is unavailable", "status")
	}
	return v
}
func serve(dir, addr, origin string, diag io.Writer) error {
	host, _, err := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return errors.New("serve accepts an explicit loopback IP only")
	}
	store, err := journal.Open(dir)
	if err != nil {
		return err
	}
	defer store.Close()
	token, err := credential(dir, "operator.token")
	if err != nil {
		return err
	}
	observer, err := credential(dir, "observer.token")
	if err != nil {
		return err
	}
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	cfg.RegistrationToken = token
	cfg.Tmux = "off"
	cfg.AdoptForeignTmux = false
	disabled := ""
	cfg.CaptureDir = &disabled
	cfg.Listen = addr
	allowLocal := false
	cfg.AllowLocalhostOrigins = &allowLocal
	if origin != "" {
		cfg.AllowedOrigins = append(cfg.AllowedOrigins, origin)
	}
	cfg.ResolveAgents(nil)
	srv := server.New(cfg)
	modern := srv.EnableModern(store, observer)
	mux := http.NewServeMux()
	mux.Handle("/v1", modern)
	mux.Handle("/", srv.Handler())
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	endpoint := filepath.Join(dir, "endpoint")
	if err := atomicWrite(endpoint, []byte(ln.Addr().String())); err != nil {
		return err
	}
	defer os.Remove(endpoint)
	h := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- h.Serve(ln) }()
	fmt.Fprintf(diag, "Continuum %s ready at %s\nState: %s\nNext: continuum status --state %q\nCtrl-C stops this daemon and its processes.\n", Version, ln.Addr(), dir, dir)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.BeginShutdown()
	_ = h.Shutdown(shutdown)
	srv.StopAll()
	if err := srv.Drain(shutdown); err != nil {
		return err
	}
	return modern.CleanShutdown()
}

func readOperation(op string) bool { return op == "status" || op == "events" || op == "screen" }
