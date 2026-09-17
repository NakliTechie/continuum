package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/NakliTechie/continuum/api"
)

const waitHelp = `Usage: continuum wait create --state DIR --request-id ID --mode any|all --deadline 30m --source JSON [--source JSON ...]
       continuum wait output|attach|cancel --state DIR --id ID [--request-id ID]
       continuum wait peer-add --state DIR --request-id ID --name NAME --peer-host SSH --peer-state DIR --peer-binary ABSOLUTE_PATH
       continuum wait peer-list|peer-remove --state DIR [--name NAME --request-id ID]
       continuum wait prompt --state DIR --block ID --text TEXT --request-id ID --source JSON
       continuum wait approve --state DIR --block ID --permission-id ID --outcome approve|approve_always|reject --request-id ID
Source: {"name":"build","peer":"local","block_id":"0123456789abcdef","until":["done","exited"]}
Peer names are registered on the coordinator; they are not arbitrary SSH destinations.
Output is one JSON API envelope. output is nonblocking; attach polls until resolved.
Only cancel changes a durable wait. Client interruption never stops a workload.
`

func waitCLI(args []string, out, diag io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(diag, waitHelp)
		return 2
	}
	cmd := args[0]
	if cmd == "help" || cmd == "--help" {
		fmt.Fprint(out, waitHelp)
		return 0
	}
	if cmd != "create" && cmd != "output" && cmd != "attach" && cmd != "cancel" && cmd != "prompt" && cmd != "approve" && cmd != "peer-add" && cmd != "peer-list" && cmd != "peer-remove" {
		fmt.Fprint(diag, waitHelp)
		return 2
	}
	f := flag.NewFlagSet("wait "+cmd, flag.ContinueOnError)
	f.SetOutput(diag)
	state := f.String("state", defaultState(), "coordinator state")
	host := f.String("host", "", "coordinator SSH host")
	binary := f.String("remote-binary", "continuum", "remote executable")
	f.Bool("json", true, "JSON output")
	observer := f.Bool("observer", false, "read-only credential")
	id := f.String("id", "", "wait ID")
	peerName := f.String("name", "", "registered peer name")
	peerHost := f.String("peer-host", "", "SSH host as seen by coordinator")
	peerState := f.String("peer-state", "", "absolute peer state directory")
	peerBinary := f.String("peer-binary", "", "absolute executable on peer")
	block := f.String("block", "", "block ID for ACP control")
	prompt := f.String("text", "", "ACP prompt text")
	permissionID := f.String("permission-id", "", "pending ACP permission request ID")
	outcome := f.String("outcome", "", "approval outcome")
	optionID := f.String("option-id", "", "offered option ID")
	request := f.String("request-id", "", "stable mutation ID")
	mode := f.String("mode", "any", "any or all")
	deadline := f.Duration("deadline", 30*time.Minute, "persistent wait deadline")
	timeout := f.Duration("timeout", 0, "attach client deadline")
	var sources []api.WaitSource
	f.Func("source", "source JSON (repeatable)", func(raw string) error {
		if len(sources) >= 8 || len(raw) > 16<<10 {
			return errors.New("at most eight bounded sources")
		}
		var s api.WaitSource
		if err := decodeOne([]byte(raw), &s); err != nil {
			return err
		}
		sources = append(sources, s)
		return nil
	})
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if f.NArg() != 0 || !filepath.IsAbs(*state) || (*host != "" && !validRemoteHost(*host)) || *binary == "" || (cmd != "create" && cmd != "prompt" && len(sources) != 0) {
		fmt.Fprint(diag, waitHelp)
		return 2
	}
	if ((cmd == "create" || cmd == "prompt") && (*request == "" || len(sources) == 0 || *deadline < time.Second || *deadline > 24*time.Hour || *id != "")) || ((cmd == "output" || cmd == "attach" || cmd == "cancel") && (*id == "" || cmd == "cancel" && *request == "")) || ((cmd == "prompt" || cmd == "approve") && (!validBlock(*block) || *request == "")) {
		fmt.Fprint(diag, waitHelp)
		return 2
	}
	if cmd == "prompt" && (len(sources) != 1 || sources[0].Peer != "local" || sources[0].Block != *block || *prompt == "") {
		fmt.Fprint(diag, waitHelp)
		return 2
	}
	if cmd == "approve" && *permissionID == "" {
		fmt.Fprint(diag, waitHelp)
		return 2
	}
	if cmd == "peer-add" && (*request == "" || *peerName == "" || !validRemoteHost(*peerHost) || !filepath.IsAbs(*peerState) || !filepath.IsAbs(*peerBinary)) {
		fmt.Fprint(diag, waitHelp)
		return 2
	}
	if cmd == "peer-remove" && (*request == "" || *peerName == "") {
		fmt.Fprint(diag, waitHelp)
		return 2
	}
	if cmd == "attach" && *timeout < 0 {
		fmt.Fprint(diag, waitHelp)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cmd == "attach" && *timeout > 0 {
		var end context.CancelFunc
		ctx, end = context.WithTimeout(ctx, *timeout)
		defer end()
	}
	if *host != "" {
		ctx = context.WithValue(ctx, remoteKey{}, remoteTarget{host: *host, binary: *binary})
	}
	emit := func(v api.Response) int {
		if err := json.NewEncoder(out).Encode(v); err != nil {
			fmt.Fprintln(diag, "wait: output failed:", err)
			return 5
		}
		return api.Exit(v.Class)
	}
	if cmd == "create" || cmd == "cancel" || cmd == "prompt" || cmd == "approve" || strings.HasPrefix(cmd, "peer-") {
		v := requireCapability(ctx, *state, false, "waits_v1")
		if v.Class != "ok" {
			return emit(v)
		}
	}
	q := api.Request{WaitID: *id, RequestID: *request}
	switch cmd {
	case "create":
		q.Operation = "wait_create"
		q.Wait = &api.WaitSpec{Mode: *mode, DeadlineS: int((*deadline + time.Second - 1) / time.Second), Sources: sources}
	case "cancel":
		q.Operation = "wait_cancel"
	case "output":
		q.Operation = "wait_output"
	case "attach":
		q.Operation = "wait_get"
	case "prompt":
		q.Operation = "prompt_wait"
		q.Block = *block
		q.Text = *prompt
		q.Wait = &api.WaitSpec{Mode: *mode, DeadlineS: int((*deadline + time.Second - 1) / time.Second), Sources: sources}
	case "approve":
		q.Operation = "permission_respond"
		q.Block = *block
		q.PermissionID = *permissionID
		q.Outcome = *outcome
		q.OptionID = *optionID
	case "peer-add":
		q.Operation = "peer_add"
		q.Peer = &api.Peer{Name: *peerName, Host: *peerHost, State: *peerState, Binary: *peerBinary}
	case "peer-list":
		q.Operation = "peer_list"
	case "peer-remove":
		q.Operation = "peer_remove"
		q.PeerName = *peerName
	}
	if cmd == "prompt" || cmd == "approve" {
		leaseDir, err := leaseDirectory(ctx, *state)
		if err != nil {
			return emit(api.Error(q.RequestID, "access_denied", "lease_cache", "private lease cache unavailable", "acquire"))
		}
		raw, err := privateRead(filepath.Join(leaseDir, "lease-"+*block))
		if err != nil {
			return emit(api.Error(q.RequestID, "conflict", "control_required", "acquire the block before ACP control", "acquire"))
		}
		q.Lease = strings.TrimSpace(string(raw))
	}
	if cmd != "attach" {
		return emit(call(ctx, *state, cmd == "output" || *observer, q))
	}
	for {
		if ctx.Err() != nil {
			fmt.Fprintln(diag, "wait: observation cancelled; durable wait and workloads continue")
			return 130
		}
		v := call(ctx, *state, true, q)
		if v.Class != "ok" {
			return emit(v)
		}
		var result struct {
			State string `json:"state"`
		}
		if json.Unmarshal(v.Result, &result) != nil {
			return emit(api.Error("", "indeterminate", "wait_response", "invalid wait result", "wait_get"))
		}
		if result.State != "pending" {
			return emit(v)
		}
		select {
		case <-ctx.Done():
		case <-time.After(250 * time.Millisecond):
		}
	}
}
