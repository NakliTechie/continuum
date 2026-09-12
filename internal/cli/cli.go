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
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/asciicast"
	"github.com/NakliTechie/continuum/internal/config"
	"github.com/NakliTechie/continuum/internal/holder"
	"github.com/NakliTechie/continuum/internal/ipc"
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
  status                   inspect up to 20 recorded blocks; --cursor CURSOR pages, --block ID selects one
  open -- COMMAND ARGS     launch a PTY; arguments are preserved exactly; --cwd DIR sets its directory
  attach --block ID        interactive screen-v1 terminal; Ctrl-] detaches
  screen --block ID        inspect a current/final screen without taking control; --json for frames
  contract                 print the /v1 contract version and capabilities (--json for the full record)
  events --block ID        replay bounded recorded events; --follow keeps watching
  export --block ID        write the block's output as an asciicast v3 recording to stdout
  acquire --block ID       acquire 60-second input control and save it privately
  takeover --block ID      explicitly fence an existing controller
  renew --block ID         extend your current saved control lease
  release --block ID       release your saved control lease
  input --block ID         send stdin bytes using your saved lease
  resize --block ID --cols N --rows N
  stop --block ID          stop work using your saved lease
  version                  show build version
  service SUB              install/uninstall/status the always-on Continuum daemon (launchd/systemd)
  legacy SUBCOMMAND        Menagerie relay commands (serve, agents, token, service, materialise)

Common flags: --state ABSOLUTE_DIR (default: user config directory/continuum),
  --json (machine envelope), --request-id ID (reuse only for the same mutation).
Events: --after CURSOR, --follow, --text (printable text and colour only),
  --raw (byte-exact PTY replay including control sequences; trusted output only).
Serve: --listen 127.0.0.1:PORT (default: random free port), --origin URL.
Observation: --observer uses the read-only observer credential.
Terminal: open --terminal screen-v1 [--cols N --rows N] -- COMMAND opts into server-owned screens.
Attach: --observer is read-only; --takeover explicitly replaces a controller.
Keyboard only; complex Unicode and advanced TUI compatibility are experimental.

This alpha recovers records and running PTY processes after daemon restart (structured agent sessions do not survive yet).
Menagerie can use the same daemon's legacy WebSocket endpoint with operator.token.
No service is installed. Remote listeners and untrusted multi-user hosting are unsupported.
`

// splitHostPortLoose splits an address without failing on a missing port.
func splitHostPortLoose(addr string) (host, port string, err error) {
	return net.SplitHostPort(addr)
}
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
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("%s is empty; delete it to generate a new credential", p)
		}
		return v, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	// Written whole then renamed: an interrupted write never leaves an empty
	// file that the next start would accept as a credential.
	v := journal.ID() + journal.ID()
	if err := atomicWrite(p, []byte(v+"\n")); err != nil {
		return "", err
	}
	return v, nil
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
	if args[0] == holder.Subcommand {
		// The daemon re-executes itself as a block holder; never a user command.
		return holder.Main(in)
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
	plain := f.Bool("text", false, "decode PTY bytes as printable text and colour")
	rawOut := f.Bool("raw", false, "byte-exact PTY replay; trusted output only")
	observer := f.Bool("observer", false, "read-only credential")
	request := f.String("request-id", "", "stable mutation ID")
	cwd := f.String("cwd", "", "working directory")
	cols := f.Int("cols", 0, "terminal columns (resize: required; open --terminal screen-v1: default 80)")
	rows := f.Int("rows", 0, "terminal rows (resize: required; open --terminal screen-v1: default 24)")
	listen := f.String("listen", "127.0.0.1:0", "loopback address")
	origin := f.String("origin", "", "additional trusted Menagerie origin")
	profile := f.String("terminal", "", "open terminal profile: screen-v1 (experimental)")
	take := f.Bool("takeover", false, "explicitly take control when attaching")
	format := f.String("format", "asciicast", "export format: asciicast (v3)")
	f.Usage = func() {} // parse errors get one line below; -h prints the full help
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
		fmt.Fprintf(diag, "usage: continuum %s [flags]; run continuum help for the flag list\n", command)
		return 2
	}
	set := map[string]bool{}
	f.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	if !filepath.IsAbs(*state) {
		fmt.Fprintln(diag, "--state must be an absolute directory")
		return 2
	}
	if command != "serve" && command != "service" && (set["listen"] || set["origin"]) {
		fmt.Fprintln(diag, "--listen and --origin are for serve and service")
		return 2
	}
	if (set["cols"] || set["rows"]) && command != "resize" && command != "open" {
		fmt.Fprintln(diag, "--cols and --rows are for resize and open --terminal screen-v1")
		return 2
	}
	if command == "resize" && (*cols < 1 || *rows < 1) {
		fmt.Fprintln(diag, "resize requires --cols N and --rows N")
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
	if command == "service" {
		return service(*state, *listen, *origin, f.Args(), out, diag)
	}
	switch command {
	case "status", "contract", "open", "screen", "attach", "events", "export", "acquire", "takeover", "renew", "release", "input", "resize", "stop":
	default:
		fmt.Fprintln(diag, "unknown command; run continuum help")
		return 2
	}
	if f.NArg() != 0 && command != "open" {
		fmt.Fprintln(diag, "unexpected arguments; use --block ID")
		return 2
	}
	if (*plain || *rawOut) && (command != "events" || *machine) {
		fmt.Fprintln(diag, "--text and --raw are for events and cannot combine with --json")
		return 2
	}
	if *plain && *rawOut {
		fmt.Fprintln(diag, "choose --text (sanitized) or --raw (byte-exact)")
		return 2
	}
	if *rawOut && *block == "" {
		fmt.Fprintln(diag, "--raw replays one block; use --block ID")
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
	if set["format"] && command != "export" {
		fmt.Fprintln(diag, "--format is for export")
		return 2
	}
	if command == "export" {
		if *block == "" {
			fmt.Fprintln(diag, "export replays one block; use --block ID")
			return 2
		}
		if *format != "asciicast" {
			fmt.Fprintln(diag, "supported export format: asciicast")
			return 2
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		return exportCast(ctx, *state, *block, *observer, out, diag)
	}
	if command == "attach" {
		if *machine || *request != "" {
			fmt.Fprintln(diag, "attach is interactive; use screen --json for machine output")
			return 2
		}
		return attach(*state, *block, *observer, *take, in, out, diag)
	}
	operation := command
	if command == "contract" {
		operation = "version" // /v1 op name; `continuum version` stays the local build-version print
	}
	q := api.Request{Terminal: *profile, Cursor: *cursor, Operation: operation, RequestID: *request, Block: *block, After: *after, Cols: *cols, Rows: *rows}
	mut := !readOperation(operation) // classify by the /v1 op (contract → version is a read)
	if mut && q.RequestID == "" {
		q.RequestID = journal.ID()
	}
	if *block != "" && !validBlock(*block) {
		return render(api.Error(q.RequestID, "invalid_request", "block_id", "use the full 16-character block ID from status or open", "status"), *machine, out, diag)
	}
	if command == "open" {
		q.Args = f.Args()
		q.Cwd = *cwd
		if q.Cwd == "" {
			q.Cwd, _ = os.Getwd()
		} else if abs, err := filepath.Abs(q.Cwd); err == nil {
			q.Cwd = abs // the daemon resolves nothing relative to this client
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
		if *block == "" {
			return render(api.Error(q.RequestID, "invalid_request", "block_id", "use the full block ID from status or open", "status"), *machine, out, diag)
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
	var sanitizer textFilter // --text: one filter across every page, so split escapes stay caught
	warned := false
	degradedClass := "" // the first degraded envelope's class decides the exit code
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
		if command == "contract" && v.Class == "ok" && !*machine {
			var c struct {
				Contract        string `json:"contract"`
				ContractVersion string `json:"contract_version"`
				SchemaVersion   int    `json:"schema_version"`
				Server          string `json:"server"`
				Restart         bool   `json:"process_restart_survival"`
				Capabilities    struct {
					Stable       []string `json:"stable"`
					Experimental []string `json:"experimental"`
				} `json:"capabilities"`
			}
			if err := json.Unmarshal(v.Result, &c); err != nil {
				return render(api.Error(q.RequestID, "indeterminate", "invalid_contract", "invalid version response", "status"), false, out, diag)
			}
			fmt.Fprintf(out, "%s contract %s · schema %d · server %s\n", c.Contract, c.ContractVersion, c.SchemaVersion, c.Server)
			fmt.Fprintf(out, "stable:       %s\n", strings.Join(c.Capabilities.Stable, ", "))
			fmt.Fprintf(out, "experimental: %s\n", strings.Join(c.Capabilities.Experimental, ", "))
			fmt.Fprintf(out, "process restart survival: %s\n", yesno(c.Restart))
			return 0
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
		// Human output still gets the page under a degraded envelope; --json
		// keeps the whole envelope so machine callers see the class themselves.
		degraded := command == "events" && !*machine && (v.Class == "indeterminate" || v.Class == "history_gap") && len(v.Result) > 0
		if command != "events" || (v.Class != "ok" && !degraded) {
			return render(v, *machine, out, diag)
		}
		if degraded && !warned {
			// The page is still delivered; the envelope's warning goes beside
			// it, and the exit code carries the class once the run ends.
			warned, degradedClass = true, v.Class
			fmt.Fprintf(diag, "%s: %s\n", v.Code, v.Message)
		}
		var p journal.Page
		if err := json.Unmarshal(v.Result, &p); err != nil {
			return 8
		}
		exited := false
		for _, e := range p.Events {
			if e.Type == "exited" && q.Block != "" {
				exited = true
			}
			if *plain || *rawOut {
				if e.Type == "output" {
					var data struct {
						Data string `json:"data"`
					}
					_ = json.Unmarshal(e.Payload, &data)
					raw, err := base64.StdEncoding.DecodeString(data.Data)
					if err != nil {
						return 8
					}
					if *plain {
						raw = sanitizer.Write(raw)
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
		if v.Class == "history_gap" {
			return api.Exit(degradedClass) // the first degraded class of this run
		}
		if p.Next < p.Last && !exited {
			continue
		}
		if !*follow || exited {
			if exited && *follow && (*plain || *rawOut) {
				fmt.Fprintf(diag, "Block %s exited.\n", q.Block)
			}
			if degradedClass != "" {
				return api.Exit(degradedClass)
			}
			return 0
		}
		select {
		case <-ctx.Done():
			if degradedClass != "" {
				return api.Exit(degradedClass)
			}
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
	if ipc.Absent(dir) {
		return fail("daemon_not_running", "start continuum serve with the same --state directory")
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
	// The host is a name for the Unix socket, not an address anything resolves.
	req, err := http.NewRequestWithContext(ctx, "POST", "http://continuum/v1", bytes.NewReader(b))
	if err != nil {
		return fail("request", "could not construct request")
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	// Every request is safe to resend: reads by nature, mutations through the
	// daemon's request-id ledger. Saying so lets net/http retry a POST whose
	// reused connection turned out to be closed instead of failing it.
	if q.RequestID != "" {
		req.Header.Set("Idempotency-Key", q.RequestID)
	} else {
		req.Header.Set("Idempotency-Key", journal.ID())
	}
	resp, err := clientFor(dir).Do(req)
	if err != nil {
		if errors.Is(err, ipc.ErrForeignLink) {
			return api.Error(q.RequestID, "access_denied", "socket_link", "the state socket is a symlink this daemon did not create; remove "+ipc.Path(dir)+" and start serve again", "serve")
		}
		if !readOperation(q.Operation) {
			return api.Error(q.RequestID, "indeterminate", "transport_lost", "request may have executed; retry with the same --request-id to reconcile", "status")
		}
		return fail("daemon_unreachable", "socket present but nothing answers; the daemon exited uncleanly, start continuum serve again")
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
	cfg.HoldersState = dir
	cfg.ServerVersion = Version
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
	// Blocks a previous daemon left running come back before any door opens.
	srv.AdoptHolders()
	// Two doors, one registry: the modern API on a private Unix socket in the
	// state directory; the legacy Menagerie WebSocket on a loopback TCP port,
	// which is the only thing a browser can reach.
	sock, err := ipc.Listen(dir)
	if err != nil {
		return err
	}
	defer sock.Close()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	timeouts := func(h http.Handler) *http.Server {
		return &http.Server{Handler: h, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	}
	apiMux := http.NewServeMux()
	apiMux.Handle("/v1", modern)
	apiServer, legacyServer := timeouts(apiMux), timeouts(srv.Handler())
	done := make(chan error, 2)
	go func() { done <- apiServer.Serve(sock) }()
	go func() { done <- legacyServer.Serve(ln) }()
	fmt.Fprintf(diag, "Continuum %s ready\nAPI socket: %s\nLegacy WebSocket (Menagerie): ws://%s\nState: %s\nNext: %s\nCtrl-C stops this daemon and its processes.\n", Version, ipc.Path(dir), ln.Addr(), dir, clientCommand("status", dir, ""))
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
	_ = apiServer.Shutdown(shutdown)
	_ = legacyServer.Shutdown(shutdown)
	srv.StopAll()
	if err := srv.Drain(shutdown); err != nil {
		return err
	}
	return modern.CleanShutdown()
}

// clientFor returns the process-wide client for a state directory's socket,
// so a follow or attach loop reuses one connection instead of opening one per
// poll. No proxy, no redirects, no TCP: the transport dials the private Unix
// socket and nothing else.
func clientFor(dir string) *http.Client {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	if c, ok := clients[dir]; ok {
		return c
	}
	c := &http.Client{
		Transport: &http.Transport{
			Proxy:           nil,
			DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return ipc.Dial(ctx, dir) },
			MaxIdleConns:    2,
			IdleConnTimeout: 30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") },
	}
	clients[dir] = c
	return c
}

var (
	clientsMu sync.Mutex
	clients   = map[string]*http.Client{}
)

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
func readOperation(op string) bool {
	return op == "status" || op == "version" || op == "events" || op == "screen" || op == "export"
}

// exportCast pages a block's journal and writes an asciicast v3 stream. It is a
// client-side read: recording is what the journal already holds, and a gap in
// that history becomes a marker event, never a silent time skip.
func exportCast(ctx context.Context, dir, block string, observer bool, out, diag io.Writer) int {
	cols, rows := 80, 24
	if sv := call(ctx, dir, observer, api.Request{Operation: "screen", Block: block}); sv.Class == "ok" {
		if frame, err := decodeScreen(sv, block); err == nil && frame.Frame.Cols > 0 {
			cols, rows = frame.Frame.Cols, frame.Frame.Rows
		}
	}
	aw, err := asciicast.NewWriter(out, asciicast.Header{Term: asciicast.Term{Cols: cols, Rows: rows, Type: "xterm-256color"}, Timestamp: time.Now().Unix(), Title: "continuum block " + block})
	if err != nil {
		fmt.Fprintln(diag, "export: could not write header")
		return 5
	}
	var after uint64
	degraded := false
	firstPage := true
	for {
		v := call(ctx, dir, observer, api.Request{Operation: "events", Block: block, After: after})
		if v.Class != "ok" && v.Class != "indeterminate" && v.Class != "history_gap" {
			return render(v, false, diag, diag)
		}
		if v.Class == "indeterminate" || v.Class == "history_gap" {
			degraded = true
		}
		var p journal.Page
		if err := json.Unmarshal(v.Result, &p); err != nil {
			fmt.Fprintln(diag, "export: unreadable events page")
			return 8
		}
		if firstPage {
			if p.First > 1 || p.Gap {
				_ = aw.Marker("history before this point was not retained")
			} else if v.Class == "indeterminate" {
				_ = aw.Marker("history may be incomplete after an unclean daemon epoch")
			}
		}
		firstPage = false
		for _, e := range p.Events {
			if e.Type != "output" {
				continue
			}
			var d struct {
				Data       string `json:"data"`
				Unobserved bool   `json:"unobserved"`
			}
			if json.Unmarshal(e.Payload, &d) != nil {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(d.Data)
			if err != nil {
				continue
			}
			at, _ := time.Parse(time.RFC3339Nano, e.Time)
			if d.Unobserved {
				_ = aw.Marker("output produced while no daemon was attached")
			}
			if err := aw.Output(at, raw); err != nil {
				return 5
			}
		}
		if v.Class == "history_gap" {
			_ = aw.Marker("cursor outside retained history")
			break
		}
		after = p.Next
		if p.Next >= p.Last {
			break
		}
	}
	if degraded {
		fmt.Fprintln(diag, "export: history is incomplete; markers show where.")
		return 8
	}
	return 0
}
