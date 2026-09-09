package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/NakliTechie/continuum/api"
	"github.com/NakliTechie/continuum/internal/journal"
	"github.com/NakliTechie/continuum/internal/terminal"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

type screenView struct {
	Block           string            `json:"block_id"`
	Host            string            `json:"host_id"`
	State           string            `json:"state"`
	ExitCode        *int              `json:"exit_code,omitempty"`
	Frame           terminal.Snapshot `json:"frame"`
	CaptureDegraded bool              `json:"capture_degraded"`
}

func decodeScreen(v api.Response, block string) (screenView, error) {
	var s screenView
	if err := json.Unmarshal(v.Result, &s); err != nil {
		return s, err
	}
	if s.Block != block || s.Host == "" || s.Frame.Engine != terminal.Name || !terminal.ValidSize(s.Frame.Cols, s.Frame.Rows) || len(s.Frame.ANSI) != s.Frame.Rows || len(s.Frame.Lines) != s.Frame.Rows || (s.State != "active" && s.State != "exited") || (s.State == "exited" && s.ExitCode == nil) || s.Frame.Fault != "" {
		return s, errors.New("invalid terminal frame contract")
	}
	return s, nil
}
func validBlock(id string) bool {
	if len(id) != 16 {
		return false
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// attach uses complete server frames throughout. It never replays application
// bytes into a second parser, or asks the user's terminal to answer queries.
func attach(dir, block string, observer, takeover bool, in io.Reader, out, diag io.Writer) int {
	if !validBlock(block) {
		fmt.Fprintln(diag, "attach requires the full --block ID from open or status")
		return 2
	}
	if observer && takeover {
		fmt.Fprintln(diag, "--observer and --takeover cannot be combined")
		return 2
	}
	input, inOK := in.(*os.File)
	output, outOK := out.(*os.File)
	if !inOK || !outOK || !term.IsTerminal(input.Fd()) || !term.IsTerminal(output.Fd()) {
		fmt.Fprintln(diag, "attach requires a terminal for stdin and stdout; use screen --json or events for pipes")
		return 2
	}
	if os.Getenv("TERM") == "dumb" {
		fmt.Fprintf(diag, "attach requires a terminal with cursor controls; inspect output with:\n  %s\n", clientCommand("screen", dir, block))
		return api.Exit("unsupported")
	}
	noColor := os.Getenv("NO_COLOR") != ""
	inputFD, outputFD := input.Fd(), output.Fd()
	cols, rows, err := term.GetSize(outputFD)
	if err != nil {
		fmt.Fprintln(diag, "cannot read terminal size:", err)
		return 5
	}
	size, err := attachViewport(cols, rows)
	if err != nil {
		fmt.Fprintln(diag, err)
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	rpc := func(q api.Request) api.Response {
		timeout := 3 * time.Second
		if q.Operation == "screen" {
			timeout = 1500 * time.Millisecond
		}
		c, stop := context.WithTimeout(ctx, timeout)
		defer stop()
		return call(c, dir, observer, q)
	}
	// Screen-only daemons can serve observers. A controller must establish all
	// input/renewal capabilities before claiming control or changing child size.
	if !observer {
		status := rpc(api.Request{Operation: "status"})
		if status.Class != "ok" {
			return render(status, false, out, diag)
		}
		var advertised struct {
			Capabilities []string `json:"capabilities"`
		}
		if err := json.Unmarshal(status.Result, &advertised); err != nil {
			fmt.Fprintln(diag, "cannot read daemon capabilities; upgrade the daemon before attaching")
			return 8
		}
		missing := []string{}
		for _, required := range []string{"terminal_screen_v1", "terminal_input_base64", "control_renewal"} {
			found := false
			for _, available := range advertised.Capabilities {
				found = found || available == required
			}
			if !found {
				missing = append(missing, required)
			}
		}
		if len(missing) > 0 {
			fmt.Fprintf(diag, "daemon lacks required terminal capabilities: %s; upgrade the daemon or attach with --observer\n", strings.Join(missing, ", "))
			return api.Exit("unsupported")
		}
	}
	first := rpc(api.Request{Operation: "screen", Block: block})
	if first.Class != "ok" {
		return render(first, false, out, diag)
	}
	view, err := decodeScreen(first, block)
	if err != nil {
		fmt.Fprintln(diag, "invalid screen response:", err)
		return 8
	}
	if view.State == "exited" {
		fmt.Fprintln(diag, "block has exited; use screen or events to inspect its final output")
		return 6
	}
	lease := ""
	if !observer {
		op := "acquire"
		if takeover {
			op = "takeover"
		}
		r := rpc(api.Request{Operation: op, Block: block, RequestID: journal.ID()})
		if r.Class != "ok" {
			if r.Code == "controlled" {
				fmt.Fprintf(diag, "Block %s already has a controller.\nWatch: %s --observer\nReplace control: %s --takeover\n", block, clientCommand("attach", dir, block), clientCommand("attach", dir, block))
				return api.Exit(r.Class)
			}
			return render(r, false, out, diag)
		}
		var result struct {
			Lease string `json:"lease"`
		}
		if json.Unmarshal(r.Result, &result) != nil || result.Lease == "" {
			fmt.Fprintln(diag, "control response is invalid; inspect status before takeover")
			return 8
		}
		lease = result.Lease
		// Attach's lease stays in this process; a second attach must acquire or
		// explicitly take over, never accidentally share a saved controller secret.
		defer func() {
			c, stop := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer stop()
			r := call(c, dir, false, api.Request{Operation: "release", Block: block, Lease: lease, RequestID: journal.ID()})
			if r.Class != "ok" && r.Code != "stale_control" && r.Code != "not_running" {
				fmt.Fprintln(diag, "Control release could not be confirmed; the lease will expire.")
			}
		}()
		r = rpc(api.Request{Operation: "resize", Block: block, Lease: lease, Cols: size.cols, Rows: size.rows, RequestID: journal.ID()})
		if r.Class != "ok" {
			return render(r, false, out, diag)
		}
	}

	old, err := term.MakeRaw(inputFD)
	if err != nil {
		fmt.Fprintln(diag, "cannot enter raw terminal mode:", err)
		return 5
	}
	inFlags, err := unix.FcntlInt(inputFD, unix.F_GETFL, 0)
	if err != nil {
		_ = term.Restore(inputFD, old)
		fmt.Fprintln(diag, "cannot read input flags:", err)
		return 5
	}
	outFlags, err := unix.FcntlInt(outputFD, unix.F_GETFL, 0)
	if err != nil {
		_ = term.Restore(inputFD, old)
		fmt.Fprintln(diag, "cannot read output flags:", err)
		return 5
	}
	// stdin/stdout can share an open file description. Save BOTH before changing
	// either, and write through a pollable writer that handles partial/EAGAIN I/O.
	readerStop := func() {}
	cleaned := false
	cleanup := func() {
		if cleaned {
			return
		}
		cleaned = true
		readerStop()
		_, _ = io.WriteString(ttyWriter{context.Background(), int(outputFD), time.Second}, leaveTerminal)
		_, _ = unix.FcntlInt(inputFD, unix.F_SETFL, inFlags)
		_, _ = unix.FcntlInt(outputFD, unix.F_SETFL, outFlags)
		_ = term.Restore(inputFD, old)
	}
	var detached atomic.Bool
	defer func() {
		cleanup()
		if detached.Load() {
			command := clientCommand("attach", dir, block)
			if observer {
				command += " --observer"
			}
			fmt.Fprintf(diag, "Detached from block %s. Reconnect: %s\n", block, command)
		}
	}()
	if _, err = unix.FcntlInt(inputFD, unix.F_SETFL, inFlags|unix.O_NONBLOCK); err != nil {
		fmt.Fprintln(diag, "cannot configure terminal input:", err)
		return 5
	}
	if _, err = unix.FcntlInt(outputFD, unix.F_SETFL, outFlags|unix.O_NONBLOCK); err != nil {
		fmt.Fprintln(diag, "cannot configure terminal output:", err)
		return 5
	}
	frameOut := ttyWriter{ctx, int(outputFD), 2 * time.Second}
	if _, err = io.WriteString(frameOut, enterTerminal); err != nil {
		if ctx.Err() != nil {
			return 0
		}
		return 5
	}
	inputCtx, inputCancel := context.WithCancel(ctx)
	packets := make(chan []byte, 4)
	inputErrors := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		readTerminalInput(inputCtx, func() { detached.Store(true); cancel() }, int(inputFD), observer, packets, inputErrors)
	}()
	readerStop = func() { inputCancel(); <-readerDone }
	fail := func(r api.Response) int { cleanup(); return render(r, false, out, diag) }
	data := []byte(nil)
	ticks := time.NewTicker(75 * time.Millisecond)
	defer ticks.Stop()
	renew := time.NewTicker(20 * time.Second)
	defer renew.Stop()
	lastRevision := uint64(0)
	haveFrame := false
	disconnected := false
	paint := func(message string) error {
		frame := view.Frame
		if noColor {
			frame.ANSI = make([]string, len(view.Frame.ANSI))
			for i, row := range view.Frame.ANSI {
				frame.ANSI[i] = ansi.Strip(row)
			}
		}
		return writeFrame(frameOut, frame, size, message, observer)
	}
	footer := func() string {
		role := "Control"
		if observer {
			role = "Observer"
		}
		text := "Ctrl-] detach • " + role
		if view.Frame.Cols > size.cols || view.Frame.Rows > size.rows {
			text += " • cropped"
		}
		if view.CaptureDegraded {
			text += " • recording degraded"
		}
		return text + " • " + block
	}
	sendPending := func() *api.Response {
		if len(data) == 0 {
			return nil
		}
		q := api.Request{Operation: "input", Block: block, Lease: lease, RequestID: journal.ID(), Data: base64.StdEncoding.EncodeToString(data), Encoding: "base64"}
		r := rpc(q)
		data = nil
		if r.Class != "ok" {
			return &r
		}
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return 0
		case err := <-inputErrors:
			if err == io.EOF {
				return 0
			}
			cleanup()
			fmt.Fprintln(diag, "terminal input failed:", err)
			return 5
		case b := <-packets:
			data = append(data, b...)
			if len(data) >= 4096 {
				if r := sendPending(); r != nil {
					return fail(*r)
				}
			}
		case <-renew.C:
			if observer {
				continue
			}
			r := rpc(api.Request{Operation: "renew", Block: block, Lease: lease, RequestID: journal.ID()})
			if r.Class != "ok" {
				return fail(r)
			}
		case <-ticks.C:
			physicalCols, physicalRows, err := term.GetSize(outputFD)
			if err != nil {
				cleanup()
				fmt.Fprintln(diag, "terminal resize query failed:", err)
				return 5
			}
			next, err := attachViewport(physicalCols, physicalRows)
			if err != nil {
				cleanup()
				fmt.Fprintln(diag, err)
				return 2
			}
			if next != size {
				size = next
				haveFrame = false
				if !observer {
					r := rpc(api.Request{Operation: "resize", Block: block, Lease: lease, Cols: size.cols, Rows: size.rows, RequestID: journal.ID()})
					if r.Class != "ok" {
						return fail(r)
					}
				}
			}
			// Uncertain input stops attachment and is never automatically replayed.
			if r := sendPending(); r != nil {
				return fail(*r)
			}
			r := rpc(api.Request{Operation: "screen", Block: block})
			if ctx.Err() != nil {
				return 0
			}
			if r.Class == "unreachable" {
				if !disconnected {
					disconnected = true
					if err := paint("Disconnected; retrying • Ctrl-] detach"); err != nil {
						return 5
					}
				}
				continue
			}
			if r.Class != "ok" {
				return fail(r)
			}
			nextView, err := decodeScreen(r, block)
			if err != nil {
				return fail(api.Error("", "indeterminate", "invalid_screen", err.Error(), "status"))
			}
			if nextView.Host != view.Host {
				return fail(api.Error("", "conflict", "host_changed", "state directory now names a different host", "status"))
			}
			if haveFrame && nextView.Frame.Revision < lastRevision {
				return fail(api.Error("", "conflict", "screen_reset", "screen revision moved backwards; reconnect explicitly", "status"))
			}
			metaChanged := view.CaptureDegraded != nextView.CaptureDegraded
			view = nextView
			if !haveFrame || disconnected || metaChanged || view.Frame.Revision != lastRevision || view.State == "exited" {
				disconnected = false
				haveFrame = true
				lastRevision = view.Frame.Revision
				if err := paint(footer()); err != nil {
					if ctx.Err() != nil {
						return 0
					}
					return 5
				}
			}
			if view.State == "exited" {
				cleanup()
				code := 0
				if view.ExitCode != nil {
					code = *view.ExitCode
				}
				fmt.Fprintf(diag, "Block %s exited (code %d). Final output: %s\n", block, code, clientCommand("screen", dir, block))
				if code < 0 || code > 255 {
					return 1
				}
				return code
			}
		}
	}
}
func writeFrame(out io.Writer, frame terminal.Snapshot, size viewport, footer string, observer bool) error {
	_, err := io.WriteString(out, renderTerminal(frame, size, footer, observer))
	return err
}

// One bounded reader owns the raw descriptor. Polling makes detach/cancellation
// joinable without closing stdin or leaking a goroutine past terminal restore.
func readTerminalInput(ctx context.Context, detach context.CancelFunc, fd int, observer bool, packets chan<- []byte, errs chan<- error) {
	b := make([]byte, 4096)
	report := func(err error) {
		select {
		case errs <- err:
		case <-ctx.Done():
		}
	}
	for {
		if ctx.Err() != nil {
			return
		}
		p := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(p, 100)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			report(err)
			return
		}
		if n == 0 {
			continue
		}
		n, err = unix.Read(fd, b)
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		if err != nil {
			report(err)
			return
		}
		if n == 0 {
			report(io.EOF)
			return
		}
		if bytes.IndexByte(b[:n], 0x1d) >= 0 {
			detach()
			return
		}
		if observer {
			continue
		}
		packet := append([]byte(nil), b[:n]...)
		select {
		case packets <- packet:
		case <-ctx.Done():
			return
		}
	}
}

// ttyWriter bounds slow outer-terminal writes and remains cancellable even
// when stdin/stdout share the same nonblocking file description.
type ttyWriter struct {
	ctx     context.Context
	fd      int
	timeout time.Duration
}

func (w ttyWriter) Write(b []byte) (int, error) {
	total := 0
	deadline := time.Now().Add(w.timeout)
	for len(b) > 0 {
		if err := w.ctx.Err(); err != nil {
			return total, err
		}
		n, err := unix.Write(w.fd, b)
		if n > 0 {
			total += n
			b = b[n:]
		}
		if err != nil && err != unix.EINTR && err != unix.EAGAIN {
			return total, err
		}
		if len(b) == 0 {
			return total, nil
		}
		if time.Now().After(deadline) {
			return total, fmt.Errorf("terminal output deadline exceeded")
		}
		p := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLOUT}}
		if _, err := unix.Poll(p, 25); err != nil && err != unix.EINTR {
			return total, err
		}
	}
	return total, nil
}
