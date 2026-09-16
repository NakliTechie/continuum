package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/journal"
	"github.com/NakliTechie/continuum/internal/jsonwire"
)

const streamPageBytes = 16 << 20
const streamRowBytes = 32 << 20
const streamControlBytes = 16 << 10
const streamHelp = `Usage: continuum interleave --source JSON [--source JSON ...] [--type TYPE ...] [--follow] [--control] [--timeout 30s]
Source: {"name":"build","state":"/absolute/state","block_id":"0123456789abcdef","after":0}
Remote source: add "host":"user@host" and optional "remote_binary":"/path/to/continuum".
Observer defaults true; explicit "observer":false selects operator credentials, never control.
Always emits NDJSON. Up to eight sources; exact --type filters only event rows.
--control: stdin NDJSON {"request_id":"1","operation":"pause|continue|cancel","source":"build"}.
Pause/cancel affect observation only; Ctrl-C or timeout exits 130 without stopping work.
See docs/streams.md for cursors, limits, failure rows and Nushell consumption.
`

type streamSource struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Block    string `json:"block_id"`
	After    uint64 `json:"after,omitempty"`
	Host     string `json:"host,omitempty"`
	Binary   string `json:"remote_binary,omitempty"`
	Observer *bool  `json:"observer,omitempty"`
}

func (s streamSource) observing() bool { return s.Observer == nil || *s.Observer }
func streamName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.-", c)) {
			return false
		}
	}
	return true
}
func decodeOne(raw []byte, value any) error {
	if !utf8.Valid(raw) {
		return errors.New("valid UTF-8 JSON required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("one JSON object required")
	}
	return nil
}
func parseStreamSource(raw string) (streamSource, error) {
	var s streamSource
	if len(raw) > 16<<10 || decodeOne([]byte(raw), &s) != nil {
		return s, errors.New("source must be one bounded JSON object with known fields")
	}
	if !streamName(s.Name) || !validBlock(s.Block) || !filepath.IsAbs(s.State) || len(s.State) > 4096 || strings.ContainsRune(s.State, 0) || !utf8.ValidString(s.State) {
		return s, errors.New("source requires unique name (ASCII, 1..64), absolute state and full block_id")
	}
	if s.Host != "" && !validRemoteHost(s.Host) || s.Host == "" && s.Binary != "" || len(s.Binary) > 4096 || strings.ContainsRune(s.Binary, 0) {
		return s, errors.New("invalid host or remote_binary")
	}
	if s.Binary == "" {
		s.Binary = "continuum"
	}
	return s, nil
}

func interleaveCLI(args []string, in io.Reader, out, diag io.Writer) int {
	f := flag.NewFlagSet("interleave", flag.ContinueOnError)
	f.SetOutput(diag)
	f.Usage = func() { fmt.Fprint(diag, streamHelp) }
	var sources []streamSource
	seen := map[string]bool{}
	f.Func("source", "source JSON (repeatable)", func(raw string) error {
		if len(sources) >= 8 {
			return errors.New("at most eight sources")
		}
		s, err := parseStreamSource(raw)
		if err != nil {
			return err
		}
		if seen[s.Name] {
			return errors.New("duplicate source name")
		}
		seen[s.Name] = true
		sources = append(sources, s)
		return nil
	})
	types := map[string]bool{}
	f.Func("type", "exact event type (repeatable)", func(value string) error {
		if len(value) == 0 || len(value) > 128 || len(types) >= 32 {
			return errors.New("at most 32 non-empty type filters of 128 bytes")
		}
		types[value] = true
		return nil
	})
	follow := f.Bool("follow", false, "follow until exit/cancellation")
	control := f.Bool("control", false, "read observer controls from stdin")
	timeout := f.Duration("timeout", 0, "observation deadline")
	f.Bool("json", true, "NDJSON output (always enabled)")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if len(sources) == 0 || f.NArg() != 0 || *timeout < 0 {
		fmt.Fprint(diag, streamHelp)
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Turn a downstream pipe close into the documented output-failure exit,
	// rather than Go's special default SIGPIPE exit for fd 1.
	pipeSignals := make(chan os.Signal, 1)
	signal.Notify(pipeSignals, syscall.SIGPIPE)
	defer signal.Stop(pipeSignals)
	if *timeout > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, *timeout)
		defer stop()
	}
	var controls <-chan streamCommand
	if *control {
		controls = readStreamControls(ctx, in)
		// CLI owns this invocation's stdin; close only on completion to release
		// a scanner blocked on an open pipe. Never change inherited file flags.
		if file, ok := in.(*os.File); ok {
			defer func() { go file.Close() }()
		}
	}
	return runInterleave(ctx, sources, *follow, types, controls, out, diag, func(ctx context.Context, s streamSource, q api.Request) api.Response {
		if s.Host != "" {
			ctx = context.WithValue(ctx, remoteKey{}, remoteTarget{s.Host, s.Binary})
		}
		return call(ctx, s.State, s.observing(), q)
	})
}

type streamCommand struct {
	ID        string `json:"request_id"`
	Operation string `json:"operation"`
	Source    string `json:"source"`
	invalid   bool
}

func readStreamControls(ctx context.Context, in io.Reader) <-chan streamCommand {
	out := make(chan streamCommand) // one scanner-held command, no unbounded queue
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 1024), streamControlBytes)
		send := func(c streamCommand) bool {
			select {
			case out <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for scanner.Scan() {
			var c streamCommand
			c.invalid = decodeOne(scanner.Bytes(), &c) != nil
			if !send(c) {
				return
			}
		}
		if scanner.Err() != nil {
			send(streamCommand{invalid: true})
		}
	}()
	return out
}

type streamRow struct {
	V              int             `json:"v"`
	Kind           string          `json:"kind"`
	Source         string          `json:"source,omitempty"`
	Host           string          `json:"host_id,omitempty"`
	Block          string          `json:"block_id,omitempty"`
	Cursor         uint64          `json:"cursor"`
	Seq            uint64          `json:"seq,omitempty"`
	Time           string          `json:"time,omitempty"`
	Type           string          `json:"type,omitempty"`
	Event          []any           `json:"event,omitempty"`
	Encoding       string          `json:"encoding,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	RequestID      string          `json:"request_id,omitempty"`
	Operation      string          `json:"operation,omitempty"`
	Replayed       bool            `json:"replayed,omitempty"`
	State          string          `json:"state,omitempty"`
	Class          string          `json:"class,omitempty"`
	Code           string          `json:"code,omitempty"`
	First          uint64          `json:"first_available,omitempty"`
	Last           uint64          `json:"last_available,omitempty"`
	Sources        []streamSummary `json:"sources,omitempty"`
	ExitCode       int             `json:"exit_code,omitempty"`
	Recording      string          `json:"recording,omitempty"`
	RecordingLines int             `json:"recording_lines,omitempty"`
	Purged         bool            `json:"recording_purged,omitempty"`
}
type streamSummary struct {
	Source string `json:"source"`
	Host   string `json:"host_id,omitempty"`
	Block  string `json:"block_id"`
	State  string `json:"state"`
	Class  string `json:"class"`
	Code   string `json:"code"`
	Cursor uint64 `json:"cursor"`
}
type streamState struct {
	spec                                                                    streamSource
	ctx                                                                     context.Context
	cancel                                                                  context.CancelFunc
	jobs                                                                    chan api.Request
	busy, initialized, paused, done, exited, exitSeen, hasWatermark, warned bool
	host, state, class, code                                                string
	after, cursor, watermark                                                uint64
	page                                                                    *journal.Page
	index                                                                   int
	lastTime                                                                time.Time
	checkpoint                                                              string
	due, statusDue                                                          time.Time
}

func (s *streamState) row(kind string) streamRow {
	return streamRow{V: 1, Kind: kind, Source: s.spec.Name, Host: s.host, Block: s.spec.Block, Cursor: s.cursor}
}

type streamResult struct {
	index    int
	request  api.Request
	response api.Response
}
type streamFetch func(context.Context, streamSource, api.Request) api.Response

// encoding/json escapes C0 but leaves DEL and Unicode C1 controls literal.
// Keep NDJSON safe to display without changing the parsed strings/payloads.
var streamJSONControls = func() *strings.Replacer {
	var pairs []string
	for c := rune(0x7f); c <= 0x9f; c++ {
		pairs = append(pairs, string(c), fmt.Sprintf(`\u%04x`, c))
	}
	return strings.NewReplacer(pairs...)
}()

// streamWrite retains at most one pending write. Cancellation closes only this
// invocation's output handle (no shared O_NONBLOCK changes). Arbitrary embedders
// must supply a writer that returns; the CLI's pipe/file handles are cancellable.
func streamWrite(ctx context.Context, out io.Writer, row streamRow) error {
	// Transport JSON, not HTML: avoid expanding an accepted ACP frame full of
	// <>& by six before applying the explicit row budget.
	raw, err := jsonwire.Marshal(row)
	if err != nil {
		return err
	}
	if len(raw) > streamRowBytes {
		return errors.New("stream row exceeds 32 MiB")
	}
	encoded := boundedOutput{limit: streamRowBytes}
	if _, err := streamJSONControls.WriteString(&encoded, string(raw)); err != nil {
		return errors.New("stream row exceeds 32 MiB")
	}
	raw = append(encoded.buffer.Bytes(), '\n')
	done := make(chan error, 1)
	go func() {
		n, err := out.Write(raw)
		if err == nil && n != len(raw) {
			err = io.ErrShortWrite
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		if f, ok := out.(*os.File); ok {
			go f.Close() // even a non-pollable embedding must not hold up cancellation
		}
		return ctx.Err()
	}
}

func runInterleave(parent context.Context, specs []streamSource, follow bool, types map[string]bool, controls <-chan streamCommand, out, diag io.Writer, fetch streamFetch) int {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	results := make(chan streamResult, len(specs))
	states := make([]*streamState, len(specs))
	byName := map[string]*streamState{}
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	for i, spec := range specs {
		c, stop := context.WithCancel(ctx)
		s := &streamState{spec: spec, ctx: c, cancel: stop, jobs: make(chan api.Request, 1), state: "running", class: "ok", code: "ok", after: spec.After, cursor: spec.After}
		states[i] = s
		byName[spec.Name] = s
		workers.Add(1)
		go func(i int, s *streamState) {
			defer workers.Done()
			for {
				select {
				case <-s.ctx.Done():
					return
				case q := <-s.jobs:
					r := fetch(s.ctx, s.spec, q)
					select {
					case results <- streamResult{i, q, r}:
					case <-s.ctx.Done():
						return
					}
				}
			}
		}(i, s)
	}
	exit := 0
	account := func(class string) { exit = max(exit, api.Exit(class)) }
	emit := func(row streamRow) error { return streamWrite(ctx, out, row) }
	fail := func(s *streamState, class, code string) error {
		s.class, s.code, s.state, s.done = class, code, "failed", true
		s.page = nil
		s.cancel()
		account(class)
		r := s.row("source")
		r.Class, r.Code, r.State = class, code, s.state
		return emit(r)
	}
	accept := func(result streamResult) error {
		s := states[result.index]
		s.busy = false
		if s.done {
			return nil
		}
		v := result.response
		if v.Version != 1 || v.Code == "" {
			return fail(s, "indeterminate", "stream_response")
		}
		switch v.Class {
		case "ok", "invalid_request", "access_denied", "decision_required", "unreachable", "conflict", "history_gap", "indeterminate", "unsupported", "resource_exhausted":
		default:
			return fail(s, "indeterminate", "stream_response")
		}
		if result.request.Operation == "status" {
			if v.Class != "ok" {
				return fail(s, v.Class, v.Code)
			}
			var status struct {
				Host         string          `json:"host_id"`
				Capabilities []string        `json:"capabilities"`
				Blocks       []journal.Block `json:"blocks"`
			}
			if len(v.Result) > 1<<20 || json.Unmarshal(v.Result, &status) != nil || status.Host == "" || len(status.Host) > 128 {
				return fail(s, "indeterminate", "stream_status")
			}
			if s.initialized && s.host != status.Host {
				return fail(s, "conflict", "host_changed")
			}
			capable := false
			for _, c := range status.Capabilities {
				capable = capable || c == "event_replay"
			}
			if !capable {
				return fail(s, "unsupported", "event_replay")
			}
			if len(status.Blocks) != 1 || status.Blocks[0].ID != s.spec.Block {
				return fail(s, "conflict", "block_unavailable")
			}
			switch status.Blocks[0].State {
			case "active", "exited", "interrupted":
			default:
				return fail(s, "indeterminate", "stream_status")
			}
			s.exited = status.Blocks[0].State != "active"
			s.statusDue = time.Now().Add(time.Second)
			if s.initialized {
				return nil
			}
			s.host, s.initialized = status.Host, true
			r := s.row("source")
			r.State = "ready"
			r.Class, r.Code = "ok", "ok"
			return emit(r)
		}
		if v.Class != "ok" && v.Class != "history_gap" && !(v.Class == "indeterminate" && v.Code == "capture_degraded") {
			return fail(s, v.Class, v.Code)
		}
		var p journal.Page
		if len(v.Result) > streamPageBytes {
			return fail(s, "resource_exhausted", "stream_page_limit")
		}
		if json.Unmarshal(v.Result, &p) != nil {
			return fail(s, "indeterminate", "stream_page")
		}
		if len(p.Events) > 256 {
			return fail(s, "resource_exhausted", "stream_page_limit")
		}
		if v.Class == "history_gap" || p.Gap {
			r := s.row("source")
			r.State, r.Class, r.Code = "failed", "history_gap", "cursor_unavailable"
			r.First, r.Last = p.First, p.Last
			s.state, s.class, s.code, s.done = "failed", r.Class, r.Code, true
			s.cancel()
			account(r.Class)
			return emit(r)
		}
		if err := validateStreamPage(s, &p); err != nil {
			return fail(s, "indeterminate", "stream_page")
		}
		if !s.hasWatermark {
			s.watermark, s.hasWatermark = p.Last, true
		}
		if !follow && p.Next > s.watermark {
			p.Next = s.watermark
			end := 0
			for end < len(p.Events) && p.Events[end].Seq <= s.watermark {
				end++
			}
			p.Events = p.Events[:end]
		}
		if !s.warned && (v.Class == "indeterminate" || p.Incomplete) {
			s.warned = true
			s.class, s.code = "indeterminate", "capture_degraded"
			account(s.class)
			r := s.row("source")
			r.Class, r.Code, r.State = s.class, s.code, "degraded"
			if err := emit(r); err != nil {
				return err
			}
		}
		s.page = &p
		s.index = 0
		return nil
	}
	cache := map[string]struct {
		command streamCommand
		reply   streamRow
	}{}
	var order []string
	control := func(c streamCommand) error {
		r := streamRow{V: 1, Kind: "control", Source: c.Source, RequestID: c.ID, Operation: c.Operation, Class: "ok", Code: "ok"}
		if c.invalid || len(c.ID) == 0 || len(c.ID) > 128 || !utf8.ValidString(c.ID) || strings.ContainsRune(c.ID, 0) {
			r.RequestID = ""
			r.Class, r.Code = "invalid_request", "stream_control"
			account(r.Class)
			return emit(r)
		}
		if old, ok := cache[c.ID]; ok {
			if old.command != c {
				r.Class, r.Code = "conflict", "request_id_reuse"
				account(r.Class)
			} else {
				r = old.reply
				r.Replayed = true
			}
			return emit(r)
		}
		s := byName[c.Source]
		if s == nil {
			r.Class, r.Code = "invalid_request", "stream_source"
		} else {
			r.Host, r.Block, r.Cursor = s.host, s.spec.Block, s.cursor
			switch {
			case c.Operation != "pause" && c.Operation != "continue" && c.Operation != "cancel":
				r.Class, r.Code = "unsupported", "stream_control"
			case s.done:
				r.Class, r.Code = "conflict", "stream_finished"
			case c.Operation == "cancel":
				s.done, s.state = true, "cancelled"
				s.page = nil
				s.cancel()
				r.State = s.state
			case c.Operation == "pause":
				s.paused, s.state = true, "paused"
				r.State = s.state
			case c.Operation == "continue":
				s.paused, s.state = false, "running"
				r.State = s.state
			}
		}
		if r.Class != "ok" {
			account(r.Class)
		}
		cache[c.ID] = struct {
			command streamCommand
			reply   streamRow
		}{c, r}
		order = append(order, c.ID)
		if len(order) > 128 {
			delete(cache, order[0])
			order = order[1:]
		}
		return emit(r)
	}
	writeFailure := func(err error) int {
		if ctx.Err() != nil {
			fmt.Fprintln(diag, "interleave: observation cancelled; workloads continue")
			return 130
		}
		fmt.Fprintln(diag, "interleave: output failed:", err)
		return 5
	}
	next := 0
	for {
		if ctx.Err() != nil {
			return writeFailure(ctx.Err())
		}
		// Bound control handling to one command per turn so a noisy command
		// pipe cannot starve event delivery or RPC completion.
		select {
		case c, ok := <-controls:
			if !ok {
				controls = nil
			} else if err := control(c); err != nil {
				return writeFailure(err)
			}
		default:
		}
		select {
		case r := <-results:
			if err := accept(r); err != nil {
				return writeFailure(err)
			}
		default:
		}
		allDone := true
		for _, s := range states {
			if s.done {
				continue
			}
			allDone = false
			if !s.paused && !s.busy && s.page == nil && !time.Now().Before(s.due) {
				q := api.Request{Operation: "events", Block: s.spec.Block, After: s.after}
				if !s.initialized || follow && !time.Now().Before(s.statusDue) {
					q.Operation = "status"
				}
				s.jobs <- q
				s.busy = true
			}
		}
		if allDone {
			r := streamRow{V: 1, Kind: "end", Class: "ok", Code: "ok"}
			r.ExitCode = exit
			if exit != 0 {
				r.Class, r.Code = "indeterminate", "partial_stream"
			}
			for _, s := range states {
				r.Sources = append(r.Sources, streamSummary{s.spec.Name, s.host, s.spec.Block, s.state, s.class, s.code, s.cursor})
			}
			if err := emit(r); err != nil {
				return writeFailure(err)
			}
			return exit
		}
		progress := false
		for i := 0; i < len(states); i++ {
			idx := (next + i) % len(states)
			s := states[idx]
			if s.done || s.paused || s.page == nil {
				continue
			}
			p := s.page
			progress = true
			next = (idx + 1) % len(states)
			if s.index < len(p.Events) {
				e := p.Events[s.index]
				s.index++
				if e.Type == "exited" {
					s.exitSeen = true
				}
				if len(types) == 0 || types[e.Type] {
					r, at, err := streamEvent(s, e)
					if err != nil {
						if err = fail(s, "indeterminate", "stream_event"); err != nil {
							return writeFailure(err)
						}
						break
					}
					if err = emit(r); err != nil {
						return writeFailure(err)
					}
					s.cursor = e.Seq
					s.lastTime = at
				}
			} else {
				r := s.row("checkpoint")
				r.Cursor = p.Next
				r.Recording, r.RecordingLines, r.Purged = p.Recording, p.RecordingLines, p.RecordingPurged
				signature := fmt.Sprintf("%d:%s:%d:%t", p.Next, p.Recording, p.RecordingLines, p.RecordingPurged)
				if signature != s.checkpoint {
					if err := emit(r); err != nil {
						return writeFailure(err)
					}
					s.checkpoint = signature
				}
				s.cursor, s.after = p.Next, p.Next
				s.page = nil
				if !follow && s.after >= s.watermark || follow && (s.exitSeen || s.exited && s.after >= p.Last) {
					s.done, s.state = true, "completed"
					s.cancel()
				} else if p.Next >= p.Last {
					s.due = time.Now().Add(100 * time.Millisecond)
				}
			}
			break
		}
		if progress {
			continue
		}
		select {
		case <-ctx.Done():
			return writeFailure(ctx.Err())
		case c, ok := <-controls:
			if !ok {
				controls = nil
			} else if err := control(c); err != nil {
				return writeFailure(err)
			}
		case r := <-results:
			if err := accept(r); err != nil {
				return writeFailure(err)
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func validateStreamPage(s *streamState, p *journal.Page) error {
	if p.Next < s.after || p.Next > p.Last || p.Next == s.after && p.Next < p.Last {
		return errors.New("non-progressing page")
	}
	prev := s.after
	for _, e := range p.Events {
		if e.Version != 1 || e.Host != s.host || e.Block != s.spec.Block || e.Seq <= prev || e.Seq > p.Next || len(e.Type) == 0 || len(e.Type) > 128 || !utf8.Valid(e.Payload) || !json.Valid(e.Payload) {
			return errors.New("invalid event identity/order/payload")
		}
		if _, err := time.Parse(time.RFC3339Nano, e.Time); err != nil {
			return err
		}
		prev = e.Seq
	}
	return nil
}
func streamEvent(s *streamState, e journal.Event) (streamRow, time.Time, error) {
	r := s.row("event")
	r.Seq, r.Cursor, r.Time, r.Type, r.Payload = e.Seq, e.Seq, e.Time, e.Type, e.Payload
	at, err := time.Parse(time.RFC3339Nano, e.Time)
	if err != nil {
		return r, at, err
	}
	delta := 0.0
	if !s.lastTime.IsZero() {
		delta = max(0, at.Sub(s.lastTime).Seconds())
	}
	code, data := "m", e.Type
	if e.Type == "output" {
		var payload struct {
			Data     string `json:"data"`
			Encoding string `json:"encoding"`
		}
		if json.Unmarshal(e.Payload, &payload) != nil || payload.Encoding != "base64" {
			return r, at, errors.New("invalid output encoding")
		}
		raw, err := base64.StdEncoding.DecodeString(payload.Data)
		if err != nil {
			return r, at, err
		}
		code = "o"
		r.Encoding = "utf8"
		data = string(raw)
		if !utf8.Valid(raw) {
			r.Encoding = "base64"
			data = payload.Data
		}
	}
	r.Event = []any{delta, code, data}
	return r, at, nil
}
