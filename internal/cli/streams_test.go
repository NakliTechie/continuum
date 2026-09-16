package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/journal"
)

func sourceFixture(name string) streamSource {
	return streamSource{Name: name, State: "/unused", Block: attachBlock}
}
func sourceStatus(s streamSource, state string) api.Response {
	return api.Result("", map[string]any{"host_id": "host-" + s.Name, "capabilities": []string{"event_replay"}, "blocks": []journal.Block{{ID: s.Block, State: state}}})
}
func sourceEvent(s streamSource, seq uint64, kind string, payload any) journal.Event {
	raw, _ := json.Marshal(payload)
	return journal.Event{Version: 1, Host: "host-" + s.Name, Block: s.Block, Seq: seq, Time: time.Unix(int64(seq), 0).UTC().Format(time.RFC3339Nano), Type: kind, Payload: raw}
}
func outputEvent(s streamSource, seq uint64, data []byte) journal.Event {
	return sourceEvent(s, seq, "output", map[string]any{"encoding": "base64", "data": base64.StdEncoding.EncodeToString(data)})
}
func decodeRows(t *testing.T, raw string) []streamRow {
	t.Helper()
	var rows []streamRow
	dec := json.NewDecoder(strings.NewReader(raw))
	for {
		var r streamRow
		err := dec.Decode(&r)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err, raw)
		}
		rows = append(rows, r)
	}
	return rows
}
func TestStreamSourceValidation(t *testing.T) {
	valid := `{"name":"one","state":"/tmp/state","block_id":"0123456789abcdef"}`
	s, err := parseStreamSource(valid)
	if err != nil || !s.observing() {
		t.Fatal(s, err)
	}
	for _, bad := range []string{`{}`, valid + ` {}`, strings.Replace(valid, "/tmp/state", "relative", 1), strings.Replace(valid, `"one"`, `"shell; command"`, 1), strings.Replace(valid, `"name"`, `"token"`, 1)} {
		if _, err := parseStreamSource(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	var out, diag bytes.Buffer
	if code := interleaveCLI([]string{"--source", valid, "--source", valid}, nil, &out, &diag); code != 2 {
		t.Fatal(code)
	}
	for _, raw := range []string{`{"request_id":"1","operation":"pause","source":"one","extra":true}`, strings.Repeat("x", streamControlBytes+1)} {
		commands := readStreamControls(context.Background(), strings.NewReader(raw+"\n"))
		c := <-commands
		if !c.invalid {
			t.Fatal("accepted control", c)
		}
	}
}

func TestStreamSnapshotFilterUnknownBinaryAndCursor(t *testing.T) {
	spec := sourceFixture("one")
	for _, filter := range []map[string]bool{nil, {"output": true}} {
		var out, diag bytes.Buffer
		fetch := func(ctx context.Context, s streamSource, q api.Request) api.Response {
			if q.Operation == "status" {
				return sourceStatus(s, "exited")
			}
			if q.After == 0 {
				return api.Result("", journal.Page{Next: 3, Last: 5, Recording: "full", Events: []journal.Event{outputEvent(s, 1, []byte("a\n\x1b]52;c;x\a")), sourceEvent(s, 2, "future.event", map[string]any{"large": json.Number("9007199254740993")}), outputEvent(s, 3, []byte{0xff, 0, 0xfe})}})
			}
			return api.Result("", journal.Page{Next: 6, Last: 6, Events: []journal.Event{outputEvent(s, 4, []byte("z")), sourceEvent(s, 5, "exited", map[string]int{"code": 0}), outputEvent(s, 6, []byte("after snapshot"))}})
		}
		if code := runInterleave(context.Background(), []streamSource{spec}, false, filter, nil, &out, &diag, fetch); code != 0 {
			t.Fatal(code, diag.String())
		}
		rows := decodeRows(t, out.String())
		last := uint64(0)
		events := 0
		for _, r := range rows {
			if r.Kind == "event" {
				events++
				if r.Seq <= last || r.Seq > 5 || r.Source != "one" || r.Block != spec.Block {
					t.Fatal(r)
				}
				last = r.Seq
				if r.Seq == 3 && (r.Encoding != "base64" || r.Event[2] != "/wD+") {
					t.Fatal(r)
				}
				if r.Seq == 3 {
					wantDelta := 1.0
					if filter != nil {
						wantDelta = 2
					}
					if r.Event[0] != wantDelta {
						t.Fatal("interval must use last emitted source event", r)
					}
				}
				if r.Seq == 2 && !bytes.Contains(r.Payload, []byte("9007199254740993")) {
					t.Fatal(r)
				}
			}
		}
		want := 5
		if filter != nil {
			want = 3
		}
		if events != want || rows[len(rows)-1].Sources[0].Cursor != 5 {
			t.Fatal(events, rows)
		}
		if bytes.Contains(out.Bytes(), []byte{0x1b}) {
			t.Fatal("raw terminal escape in NDJSON")
		}
	}
}

func TestStreamFollowDrainsExitedHistory(t *testing.T) {
	var out, diag bytes.Buffer
	fetch := func(ctx context.Context, s streamSource, q api.Request) api.Response {
		if q.Operation == "status" {
			return sourceStatus(s, "exited")
		}
		return api.Result("", journal.Page{Next: q.After + 1, Last: 3, Events: []journal.Event{outputEvent(s, q.After+1, []byte("x"))}})
	}
	if code := runInterleave(context.Background(), []streamSource{sourceFixture("one")}, true, nil, nil, &out, &diag, fetch); code != 0 {
		t.Fatal(code)
	}
	rows := decodeRows(t, out.String())
	if rows[len(rows)-1].Sources[0].Cursor != 3 {
		t.Fatal("truncated exited history", out.String())
	}
}

func TestStreamJSONEscapingAndLargeStructuredPayload(t *testing.T) {
	var out bytes.Buffer
	text := "\u007f\u009b31m\u009d52;c;x\u009c\x1b\a"
	r := streamRow{V: 1, Kind: "event", Event: []any{0, "o", text}, Payload: json.RawMessage("{\"text\":\"" + text[:len(text)-2] + "\"}")}
	if err := streamWrite(context.Background(), &out, r); err != nil {
		t.Fatal(err)
	}
	for _, control := range []string{"\u007f", "\u009b", "\u009d", "\u009c", "\x1b", "\a"} {
		if strings.Contains(out.String(), control) {
			t.Fatal("raw terminal control in JSON", control)
		}
	}
	rows := decodeRows(t, out.String())
	if rows[0].Event[2] != text {
		t.Fatal("escaping changed output bytes")
	}
	out.Reset()
	large := strings.Repeat("<", 6<<20)
	r = streamRow{V: 1, Kind: "event", Type: "future.acp", Payload: json.RawMessage(`{"text":"` + large + `","id":9007199254740993}`)}
	if err := streamWrite(context.Background(), &out, r); err != nil {
		t.Fatal("valid structured frame expanded beyond row limit", err)
	}
	if out.Len() > 7<<20 || !bytes.Contains(out.Bytes(), []byte("9007199254740993")) {
		t.Fatal("structured payload expanded or lost integer precision", out.Len())
	}
}

func TestStreamQuietFollowRevalidatesIdentityAndLifecycle(t *testing.T) {
	for _, change := range []string{"host", "retired", "exited", "capability"} {
		t.Run(change, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var out, diag bytes.Buffer
			statuses := 0
			fetch := func(ctx context.Context, s streamSource, q api.Request) api.Response {
				if q.Operation != "status" {
					return api.Result("", journal.Page{Recording: "none"})
				}
				statuses++
				if statuses == 1 {
					return sourceStatus(s, "active")
				}
				status := map[string]any{"host_id": "host-" + s.Name, "capabilities": []string{"event_replay"}, "blocks": []journal.Block{{ID: s.Block, State: "exited"}}}
				switch change {
				case "host":
					status["host_id"] = "replacement"
				case "retired":
					status["blocks"] = []journal.Block{}
				case "capability":
					status["capabilities"] = []string{}
				}
				return api.Result("", status)
			}
			code := runInterleave(ctx, []streamSource{sourceFixture("quiet")}, true, nil, nil, &out, &diag, fetch)
			want, wantCode := 6, "host_changed"
			switch change {
			case "retired":
				wantCode = "block_unavailable"
			case "exited":
				want, wantCode = 0, "ok"
			case "capability":
				want, wantCode = 9, "event_replay"
			}
			if code != want {
				t.Fatal(code, diag.String(), out.String())
			}
			rows := decodeRows(t, out.String())
			if len(rows) > 4 || rows[len(rows)-1].Sources[0].Code != wantCode {
				t.Fatal("quiet checkpoints repeated or lifecycle not checked", out.String())
			}
		})
	}
}

type rowChannel struct {
	rows chan streamRow
	all  safeBuffer
}

func (w *rowChannel) Write(p []byte) (int, error) {
	var r streamRow
	if err := json.Unmarshal(p, &r); err != nil {
		return 0, err
	}
	w.all.Write(p)
	w.rows <- r
	return len(p), nil
}
func nextRow(t *testing.T, ch <-chan streamRow, match func(streamRow) bool) streamRow {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case r := <-ch:
			if match(r) {
				return r
			}
		case <-timer.C:
			t.Fatal("stream row deadline")
			return streamRow{}
		}
	}
}
func TestStreamQuietSourcePauseContinueAndCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out := &rowChannel{rows: make(chan streamRow, 100)}
	var diag bytes.Buffer
	commands := make(chan streamCommand, 10)
	var quietCalls, activeCalls atomic.Int32
	fetch := func(ctx context.Context, s streamSource, q api.Request) api.Response {
		if q.Operation == "status" {
			return sourceStatus(s, "active")
		}
		if s.Name == "quiet" {
			quietCalls.Add(1)
			<-ctx.Done()
			return api.Error("", "unreachable", "cancelled", "", "")
		}
		activeCalls.Add(1)
		return api.Result("", journal.Page{Next: q.After + 1, Last: q.After + 1, Events: []journal.Event{outputEvent(s, q.After+1, []byte("active"))}})
	}
	done := make(chan int, 1)
	go func() {
		done <- runInterleave(ctx, []streamSource{sourceFixture("quiet"), sourceFixture("active")}, true, nil, commands, out, &diag, fetch)
	}()
	nextRow(t, out.rows, func(r streamRow) bool { return r.Kind == "event" && r.Source == "active" })
	commands <- streamCommand{ID: "pause", Operation: "pause", Source: "active"}
	paused := nextRow(t, out.rows, func(r streamRow) bool { return r.RequestID == "pause" })
	if paused.State != "paused" {
		t.Fatal(paused)
	}
	calls := activeCalls.Load()
	time.Sleep(130 * time.Millisecond)
	if activeCalls.Load() > calls+1 || quietCalls.Load() != 1 {
		t.Fatal("paused/quiet sources kept fetching")
	}
	for len(out.rows) > 0 {
		if r := <-out.rows; r.Kind == "event" && r.Source == "active" {
			t.Fatal("event after pause ack", r)
		}
	}
	commands <- streamCommand{ID: "resume", Operation: "continue", Source: "active"}
	nextRow(t, out.rows, func(r streamRow) bool { return r.RequestID == "resume" })
	nextRow(t, out.rows, func(r streamRow) bool { return r.Kind == "event" && r.Source == "active" })
	commands <- streamCommand{ID: "resume", Operation: "continue", Source: "active"}
	if r := nextRow(t, out.rows, func(r streamRow) bool { return r.RequestID == "resume" }); !r.Replayed {
		t.Fatal(r)
	}
	commands <- streamCommand{ID: "resume", Operation: "pause", Source: "active"}
	if r := nextRow(t, out.rows, func(r streamRow) bool { return r.RequestID == "resume" }); r.Class != "conflict" {
		t.Fatal(r)
	}
	commands <- streamCommand{ID: "stop-a", Operation: "cancel", Source: "active"}
	commands <- streamCommand{ID: "stop-q", Operation: "cancel", Source: "quiet"}
	select {
	case code := <-done:
		if code != 6 {
			t.Fatal(code, diag.String())
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not end observation")
	}
}

func TestStreamSourceFailureAndPageValidation(t *testing.T) {
	for _, failure := range []string{"offline", "gap", "degraded", "wrong_host", "duplicate_seq", "nonprogress", "invalid_output", "unsupported", "absent", "unknown_class", "page_limit", "invalid_utf8"} {
		t.Run(failure, func(t *testing.T) {
			var out, diag bytes.Buffer
			fetch := func(ctx context.Context, s streamSource, q api.Request) api.Response {
				if q.Operation == "status" {
					if s.Name == "bad" && failure == "unsupported" {
						return api.Result("", map[string]any{"host_id": "host-bad"})
					}
					if s.Name == "bad" && failure == "absent" {
						return api.Result("", map[string]any{"host_id": "host-bad", "capabilities": []string{"event_replay"}})
					}
					return sourceStatus(s, "exited")
				}
				p := journal.Page{Next: 1, Last: 1, Events: []journal.Event{outputEvent(s, 1, []byte("valid"))}}
				if s.Name == "bad" {
					switch failure {
					case "offline":
						return api.Error("", "unreachable", "offline", "", "")
					case "gap":
						p = journal.Page{Gap: true, First: 8, Last: 10}
						r := api.Result("", p)
						r.Class, r.Code = "history_gap", "cursor_unavailable"
						return r
					case "degraded":
						p.Incomplete = true
					case "wrong_host":
						p.Events[0].Host = "replacement"
					case "duplicate_seq":
						p.Events = append(p.Events, p.Events[0])
					case "nonprogress":
						p.Next = 0
						p.Events = nil
					case "invalid_output":
						p.Events[0].Payload = json.RawMessage(`{"encoding":"base64","data":"broken!"}`)
					case "unknown_class":
						return api.Error("", "mystery", "future", "", "")
					case "page_limit":
						p.Events = make([]journal.Event, 257)
					case "invalid_utf8":
						p.Events[0].Payload = json.RawMessage("{\"text\":\"\xff\"}")
					}
				}
				return api.Result("", p)
			}
			code := runInterleave(context.Background(), []streamSource{sourceFixture("bad"), sourceFixture("good")}, false, nil, nil, &out, &diag, fetch)
			if code == 0 {
				t.Fatal("failure reported success")
			}
			rows := decodeRows(t, out.String())
			good := false
			for _, r := range rows {
				good = good || r.Kind == "event" && r.Source == "good"
			}
			if !good || rows[len(rows)-1].Sources[1].State != "completed" {
				t.Fatal("failed source blocked good source", out.String())
			}
		})
	}
}

type blockedStreamWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedStreamWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}
func TestStreamBackpressureBoundsAndInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &blockedStreamWriter{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(w.release)
	var calls atomic.Int32
	var diag bytes.Buffer
	fetch := func(ctx context.Context, s streamSource, q api.Request) api.Response {
		calls.Add(1)
		return sourceStatus(s, "active")
	}
	done := make(chan int, 1)
	go func() {
		done <- runInterleave(ctx, []streamSource{sourceFixture("a"), sourceFixture("b")}, true, nil, nil, w, &diag, fetch)
	}()
	<-w.entered
	time.Sleep(40 * time.Millisecond)
	if calls.Load() > 2 {
		t.Fatal("blocked stdout fetched unbounded pages", calls.Load())
	}
	cancel()
	select {
	case code := <-done:
		if code != 130 {
			t.Fatal(code)
		}
	case <-time.After(time.Second):
		t.Fatal("interrupt stuck on output")
	}
}

func TestStreamControlEOFIgnoredAndPolicyCheckpoint(t *testing.T) {
	var out, diag bytes.Buffer
	controls := readStreamControls(context.Background(), strings.NewReader(""))
	fetch := func(ctx context.Context, s streamSource, q api.Request) api.Response {
		if q.Operation == "status" {
			return sourceStatus(s, "exited")
		}
		return api.Result("", journal.Page{Recording: "none", RecordingPurged: true})
	}
	if code := runInterleave(context.Background(), []streamSource{sourceFixture("a")}, false, nil, controls, &out, &diag, fetch); code != 0 {
		t.Fatal(code)
	}
	rows := decodeRows(t, out.String())
	if len(rows) != 3 || rows[1].Recording != "none" || !rows[1].Purged {
		t.Fatal(rows)
	}
	if fmt.Sprint(rows[len(rows)-1].Sources[0].State) != "completed" {
		t.Fatal(rows)
	}
}
