// Package holder keeps a PTY block alive across daemon restarts. Each block's
// process is the child of a small holder process in its own session, not of
// the daemon. The holder is the SOLE reader of the PTY master: it copies output
// into a bounded ring and streams it to whichever daemon is attached, over a
// per-block Unix socket. The daemon writes input and resizes through a
// duplicate of the master (passed once by SCM_RIGHTS) but never reads it, so
// output the daemon has not yet journaled cannot be lost from a shared read.
//
// On (re)attach the daemon declares how many output bytes it has already
// committed (its journal length); the holder resumes streaming from exactly
// that offset. A daemon killed between reading a chunk and committing it simply
// asks for the same offset again after restart, and the holder still has those
// bytes in its ring — so output is contiguous across a daemon SIGKILL as long
// as the uncommitted tail stayed within the ring (256 KiB). A child that exits
// while no daemon is attached leaves an exit record, with its unobserved tail,
// beside the socket.
package holder

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NakliTechie/continuum/internal/ipc"
	"github.com/NakliTechie/continuum/internal/pty"
	creackpty "github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// Subcommand is the hidden entry point the daemon re-executes itself with.
const Subcommand = "__holder"

// Dir is the holders directory inside a state directory.
func Dir(state string) string { return filepath.Join(state, "holders") }

// maxRing bounds retained output per block. A daemon that falls this far behind
// on journaling before dying loses the overflow (reported as a gap); in normal
// operation the daemon commits synchronously and the tail is tiny.
const maxRing = 256 * 1024

// Spec is what the daemon hands a new holder on stdin.
type Spec struct {
	Block     string   `json:"block"`
	Agent     string   `json:"agent"`
	Path      string   `json:"path"` // resolved executable
	Argv      []string `json:"argv"`
	Dir       string   `json:"dir"`
	Env       []string `json:"env"`
	Cols      int      `json:"cols"`
	Rows      int      `json:"rows"`
	Socket    string   `json:"socket"`
	Alias     string   `json:"alias"`
	ExitFile  string   `json:"exit_file"`
	StartedAt string   `json:"started_at"`
}

// Hello is the holder's first framed message to a connecting daemon; the PTY
// master travels in the ancillary data of the single byte that precedes it.
type Hello struct {
	Block     string `json:"block"`
	Agent     string `json:"agent"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
	Produced  int    `json:"produced"`   // total output bytes the child has emitted
	RingStart int    `json:"ring_start"` // earliest offset still in the ring
	Error     string `json:"error,omitempty"`
}

// Exit is the holder's final framed message, and the shape of the exit record
// it leaves when no daemon is attached.
type Exit struct {
	Code      int    `json:"code"`
	At        string `json:"at"`
	Produced  int    `json:"produced,omitempty"`
	RingStart int    `json:"ring_start,omitempty"`
	Ring      string `json:"ring,omitempty"` // base64 of the retained tail
}

// frame kinds on the socket, after the fd + hello handshake.
const (
	frameResume = 'R' // daemon → holder: 8-byte resume offset
	frameOutput = 'O' // holder → daemon: output bytes
	frameExit   = 'X' // holder → daemon: JSON Exit
)

func writeFrame(w io.Writer, kind byte, payload []byte) error {
	var hdr [5]byte
	hdr[0] = kind
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxRing+64<<10 {
		return 0, nil, fmt.Errorf("frame too large: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return hdr[0], buf, nil
}

// Launch creates the block's socket, starts a holder for spec, and attaches at
// resume offset 0 (a fresh child has produced nothing).
func Launch(state, exe string, spec Spec) (*Attached, error) {
	dir := Dir(state)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	spec.Socket = filepath.Join(dir, spec.Block+".sock")
	spec.ExitFile = filepath.Join(dir, spec.Block+".exit")
	spec.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	ln, err := ipc.ListenPath(spec.Socket)
	if err != nil {
		return nil, err
	}
	spec.Alias = ln.Alias()
	file, err := ln.Listener.(*net.UnixListener).File()
	if err != nil {
		ln.Close()
		return nil, err
	}
	ln.Listener.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Listener.Close()
	body, err := json.Marshal(spec)
	if err != nil {
		file.Close()
		return nil, err
	}
	cmd := exec.Command(exe, Subcommand)
	cmd.Stdin = strings.NewReader(string(body) + "\n")
	cmd.ExtraFiles = []*os.File{file}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	err = cmd.Start()
	file.Close()
	if err != nil {
		removeSocket(spec)
		return nil, err
	}
	go func() { _ = cmd.Wait() }()
	a, err := Adopt(spec.Socket, 0)
	if err != nil {
		removeSocket(spec)
		return nil, err
	}
	return a, nil
}

// Attached is a live connection to a holder plus the PTY master it handed over.
// Output is the child's byte stream from the resume offset onward; the daemon
// reads it exactly as it read a PTY before.
type Attached struct {
	Hello  Hello
	Ptmx   *os.File  // input and resize only; never read
	Output io.Reader // decoded output stream, including Replay retained bytes
	Replay int       // leading Output bytes already committed by the daemon
	Gap    int       // bytes produced before this attach (unobserved downtime)
	conn   net.Conn
	exit   chan int
	once   sync.Once
}

// Adopt connects to a holder's socket, receives its master, declares the
// resume offset, and starts decoding the output stream.
func Adopt(socket string, resume int) (*Attached, error) {
	target, err := ipc.Resolve(socket)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", target, 2*time.Second)
	if err != nil {
		return nil, err
	}
	uc := conn.(*net.UnixConn)
	_ = uc.SetDeadline(time.Now().Add(5 * time.Second))
	fd, herr := recvFD(uc)
	if herr != nil {
		conn.Close()
		return nil, herr
	}
	rd := bufio.NewReader(conn)
	kind, payload, err := readFrame(rd)
	if err != nil || kind != 'H' {
		closeFD(fd)
		conn.Close()
		if err == nil {
			err = fmt.Errorf("unexpected handshake frame %q", kind)
		}
		return nil, err
	}
	var hello Hello
	if err := json.Unmarshal(payload, &hello); err != nil {
		closeFD(fd)
		conn.Close()
		return nil, err
	}
	if hello.Error != "" {
		closeFD(fd)
		conn.Close()
		return nil, errors.New(hello.Error)
	}
	if fd < 0 {
		conn.Close()
		return nil, errors.New("holder sent no terminal descriptor")
	}
	if resume > hello.Produced {
		resume = hello.Produced
	}
	committed := resume
	if committed < hello.RingStart {
		committed = hello.RingStart // the earliest the holder can still supply
	}
	// Start at the retained ring boundary, not merely at the journal offset.
	// A restarted server-owned terminal needs the bounded prefix to rebuild its
	// volatile screen. Attached.Replay tells the session how much of that prefix
	// must feed only the screen, never be journaled or captured a second time.
	streamStart := hello.RingStart
	var off [8]byte
	binary.BigEndian.PutUint64(off[:], uint64(streamStart))
	if err := writeFrame(conn, frameResume, off[:]); err != nil {
		closeFD(fd)
		conn.Close()
		return nil, err
	}
	_ = uc.SetDeadline(time.Time{})
	pr, pw := io.Pipe()
	a := &Attached{Hello: hello, Ptmx: os.NewFile(uintptr(fd), "ptmx"), Output: pr, Replay: committed - streamStart, Gap: hello.Produced - committed, conn: conn, exit: make(chan int, 1)}
	go a.decode(rd, pw)
	return a, nil
}

func (a *Attached) decode(rd *bufio.Reader, pw *io.PipeWriter) {
	for {
		kind, payload, err := readFrame(rd)
		if err != nil {
			pw.CloseWithError(err)
			a.signalExit(-1)
			return
		}
		switch kind {
		case frameOutput:
			if _, werr := pw.Write(payload); werr != nil {
				a.signalExit(-1)
				return
			}
		case frameExit:
			var ex Exit
			_ = json.Unmarshal(payload, &ex)
			pw.Close()
			a.signalExit(ex.Code)
			return
		}
	}
}

func (a *Attached) signalExit(code int) {
	a.once.Do(func() { a.exit <- code })
}

// Wait blocks until the holder reports the child's exit (or the connection is
// lost, yielding -1).
func (a *Attached) Wait() int { return <-a.exit }

// Close drops the daemon's side without touching the child.
func (a *Attached) Close() { a.conn.Close() }

func recvFD(uc *net.UnixConn) (int, error) {
	raw, err := uc.SyscallConn()
	if err != nil {
		return -1, err
	}
	fd, rerr := -1, error(nil)
	if err := raw.Read(func(sfd uintptr) bool {
		buf := make([]byte, 1)
		oob := make([]byte, unix.CmsgSpace(4))
		n, oobn, _, _, e := unix.Recvmsg(int(sfd), buf, oob, 0)
		if e == unix.EAGAIN {
			return false
		}
		if e != nil {
			rerr = e
			return true
		}
		if n == 0 {
			rerr = io.ErrUnexpectedEOF
			return true
		}
		if buf[0] == 'F' && oobn > 0 {
			if msgs, e := unix.ParseSocketControlMessage(oob[:oobn]); e == nil && len(msgs) == 1 {
				if fds, e := unix.ParseUnixRights(&msgs[0]); e == nil && len(fds) == 1 {
					fd = fds[0]
				}
			}
		}
		return true
	}); err != nil {
		return -1, err
	}
	return fd, rerr
}

func closeFD(fd int) {
	if fd >= 0 {
		unix.Close(fd)
	}
}

// Record is a block found in the holders directory at daemon start.
type Record struct {
	Block  string
	Socket string
	Exit   *Exit
}

// Scan lists what previous daemons left behind, exit records included.
func Scan(state string) ([]Record, error) {
	entries, err := os.ReadDir(Dir(state))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	byBlock := map[string]*Record{}
	order := []string{}
	get := func(block string) *Record {
		r := byBlock[block]
		if r == nil {
			r = &Record{Block: block}
			byBlock[block] = r
			order = append(order, block)
		}
		return r
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".sock"):
			get(strings.TrimSuffix(name, ".sock")).Socket = filepath.Join(Dir(state), name)
		case strings.HasSuffix(name, ".exit"):
			raw, err := os.ReadFile(filepath.Join(Dir(state), name))
			if err != nil {
				continue
			}
			var ex Exit
			if json.Unmarshal(raw, &ex) != nil {
				continue
			}
			get(strings.TrimSuffix(name, ".exit")).Exit = &ex
		}
	}
	out := make([]Record, 0, len(order))
	for _, b := range order {
		out = append(out, *byBlock[b])
	}
	return out, nil
}

// Forget removes whatever a block left in the holders directory.
func Forget(state, block string) {
	_ = os.Remove(filepath.Join(Dir(state), block+".exit"))
	socket := filepath.Join(Dir(state), block+".sock")
	if ipc.IsAlias(socket) {
		if target, err := os.Readlink(socket); err == nil {
			_ = os.Remove(target)
			_ = os.RemoveAll(filepath.Dir(target))
		}
	}
	_ = os.Remove(socket)
}

func removeSocket(spec Spec) {
	if spec.Alias != "" {
		_ = os.Remove(filepath.Join(spec.Alias, filepath.Base(spec.Socket)))
		_ = os.RemoveAll(spec.Alias)
	}
	_ = os.Remove(spec.Socket)
}

// Main is the holder process. It never returns to the CLI's normal flow.
func Main(stdin io.Reader) int {
	var spec Spec
	if err := json.NewDecoder(stdin).Decode(&spec); err != nil {
		return 2
	}
	if os.Stdin != nil {
		_ = os.Stdin.Close()
	}
	ln, err := net.FileListener(os.NewFile(3, "socket"))
	if err != nil {
		return 2
	}
	defer removeSocket(spec)
	h := &holder{spec: spec, ln: ln}
	return h.run()
}

type holder struct {
	spec Spec
	ln   net.Listener
	ptmx *os.File
	cmd  *exec.Cmd

	mu       sync.Mutex
	ring     []byte
	produced int // total bytes read from the PTY
	start    int // offset of ring[0] (produced - len(ring))
}

func (h *holder) ringStart() int { h.mu.Lock(); defer h.mu.Unlock(); return h.start }

func (h *holder) run() int {
	spawnErr := h.spawn()
	conns := make(chan net.Conn)
	go func() {
		for {
			c, err := h.ln.Accept()
			if err != nil {
				close(conns)
				return
			}
			conns <- c
		}
	}()
	if spawnErr != nil {
		select {
		case c := <-conns:
			if c != nil {
				h.sayHello(c, spawnErr)
				c.Close()
			}
		case <-time.After(5 * time.Second):
			h.writeExit(Exit{Code: -1, At: time.Now().UTC().Format(time.RFC3339Nano)})
		}
		return 1
	}

	dataCh := make(chan struct{}, 1)
	exitCh := make(chan int, 1)
	detachCh := make(chan net.Conn, 4)
	go h.pump(dataCh, exitCh)

	var attached net.Conn
	var sent int
	for {
		select {
		case c, ok := <-conns:
			if !ok {
				return 1
			}
			if attached != nil {
				attached.Close()
			}
			attached, sent = h.accept(c, detachCh)
		case <-dataCh:
			if attached != nil && !h.flush(attached, &sent) {
				attached.Close()
				attached = nil
			}
		case gone := <-detachCh:
			if gone == attached {
				attached = nil
			}
		case code := <-exitCh:
			at := time.Now().UTC().Format(time.RFC3339Nano)
			// Always leave the exit record: if the "attached" daemon has in fact
			// died (we may not have seen the detach yet), the frame write is lost,
			// and only the file lets the next daemon settle the block. A daemon
			// that did receive the frame journals `exited` and makes the file's
			// settle idempotent.
			h.mu.Lock()
			ex := Exit{Code: code, At: at, Produced: h.produced, RingStart: h.start, Ring: base64.StdEncoding.EncodeToString(h.ring)}
			h.mu.Unlock()
			h.writeExit(ex)
			if attached != nil {
				h.flush(attached, &sent)
				line, _ := json.Marshal(Exit{Code: code, At: at})
				_ = attached.SetWriteDeadline(time.Now().Add(2 * time.Second))
				_ = writeFrame(attached, frameExit, line)
				attached.Close()
			}
			return 0
		}
	}
}

func (h *holder) spawn() error {
	if h.spec.Path == "" {
		return errors.New("empty path")
	}
	cmd := exec.Command(h.spec.Path)
	cmd.Args = append([]string(nil), h.spec.Argv...)
	cmd.Dir = h.spec.Dir
	cmd.Env = h.spec.Env
	var ptmx *os.File
	var err error
	if h.spec.Cols > 0 && h.spec.Rows > 0 {
		ptmx, err = creackpty.StartWithSize(cmd, &creackpty.Winsize{Cols: uint16(h.spec.Cols), Rows: uint16(h.spec.Rows)})
	} else {
		ptmx, err = creackpty.Start(cmd)
	}
	if err != nil {
		return err
	}
	_ = pty.SetUTF8(ptmx)
	h.cmd, h.ptmx = cmd, ptmx
	return nil
}

// pump is the sole reader of the PTY master: it appends every byte to the ring
// and signals the run loop, then reaps the child.
func (h *holder) pump(dataCh chan<- struct{}, exitCh chan<- int) {
	buf := make([]byte, 32*1024)
	fd := int(h.ptmx.Fd())
	for {
		n, err := unix.Read(fd, buf)
		if n > 0 {
			h.mu.Lock()
			h.ring = append(h.ring, buf[:n]...)
			h.produced += n
			if over := len(h.ring) - maxRing; over > 0 {
				h.ring = h.ring[over:]
				h.start += over
			}
			h.mu.Unlock()
			select {
			case dataCh <- struct{}{}:
			default:
			}
		}
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil || n == 0 {
			break
		}
	}
	code := 0
	if err := h.cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	exitCh <- code
}

// accept performs the fd + hello handshake, reads the resume offset, streams
// the backlog from it, and watches the connection for detach.
func (h *holder) accept(c net.Conn, detachCh chan<- net.Conn) (net.Conn, int) {
	if err := h.sayHello(c, nil); err != nil {
		c.Close()
		return nil, 0
	}
	kind, payload, err := readFrame(c)
	if err != nil || kind != frameResume || len(payload) != 8 {
		c.Close()
		return nil, 0
	}
	sent := int(binary.BigEndian.Uint64(payload))
	if start := h.ringStart(); sent < start {
		sent = start
	}
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := c.Read(buf); err != nil {
				detachCh <- c
				return
			}
		}
	}()
	if !h.flush(c, &sent) {
		c.Close()
		return nil, 0
	}
	return c, sent
}

// flush streams ring bytes from *sent up to produced. Returns false on write
// error (the daemon detached).
func (h *holder) flush(c net.Conn, sent *int) bool {
	for {
		h.mu.Lock()
		if *sent < h.start {
			*sent = h.start
		}
		if *sent >= h.produced {
			h.mu.Unlock()
			return true
		}
		chunk := append([]byte(nil), h.ring[*sent-h.start:]...)
		*sent = h.produced
		h.mu.Unlock()
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := writeFrame(c, frameOutput, chunk); err != nil {
			return false
		}
	}
}

func (h *holder) sayHello(c net.Conn, spawnErr error) error {
	h.mu.Lock()
	hello := Hello{Block: h.spec.Block, Agent: h.spec.Agent, StartedAt: h.spec.StartedAt, Cols: h.spec.Cols, Rows: h.spec.Rows, Produced: h.produced, RingStart: h.start}
	h.mu.Unlock()
	if h.cmd != nil && h.cmd.Process != nil {
		hello.PID = h.cmd.Process.Pid
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return errors.New("not a unix socket")
	}
	_ = uc.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer uc.SetWriteDeadline(time.Time{})
	if spawnErr != nil {
		hello.Error = spawnErr.Error()
		if _, err := uc.Write([]byte{'E'}); err != nil {
			return err
		}
	} else {
		rights := unix.UnixRights(int(h.ptmx.Fd()))
		if _, _, err := uc.WriteMsgUnix([]byte{'F'}, rights, nil); err != nil {
			return err
		}
	}
	line, _ := json.Marshal(hello)
	return writeFrame(uc, 'H', line)
}

func (h *holder) writeExit(ex Exit) {
	raw, err := json.Marshal(ex)
	if err != nil {
		return
	}
	tmp := h.spec.ExitFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, h.spec.ExitFile)
}

// String renders a record for logs.
func (r Record) String() string {
	if r.Exit != nil {
		return fmt.Sprintf("%s exited %d at %s", r.Block, r.Exit.Code, r.Exit.At)
	}
	return r.Block + " held"
}
