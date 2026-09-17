package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"github.com/NakliTechie/continuum/api"
)

const accessHelp = `Usage: continuum access grant --state DIR --request-id ID --class observer|controller|moderator --block ID [--block ID ...]
       continuum access revoke --state DIR --request-id ID --grant-id ID
       continuum access grants|audit --state DIR [--after N]
       continuum access share --state DIR --request-id ID --block ID --policy private|observers|controllers
       continuum access sharing --state DIR --block ID
Grant tokens are returned once. Save them in a private file; never put them in argv.
Only root operator authority can administer grants or sharing.
`

func accessCLI(args []string, out, diag io.Writer) int {
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(out, accessHelp)
		return 0
	}
	cmd := args[0]
	if cmd != "grant" && cmd != "revoke" && cmd != "grants" && cmd != "audit" && cmd != "share" && cmd != "sharing" {
		fmt.Fprint(diag, accessHelp)
		return 2
	}
	f := flag.NewFlagSet("access "+cmd, flag.ContinueOnError)
	f.SetOutput(diag)
	state := f.String("state", defaultState(), "private daemon state")
	request := f.String("request-id", "", "stable admin request ID")
	class := f.String("class", "", "grant class")
	grantID := f.String("grant-id", "", "grant ID for revocation")
	policy := f.String("policy", "", "sharing policy")
	after := f.String("after", "0", "audit cursor")
	var blocks []string
	f.Func("block", "block ID (repeatable for grant)", func(s string) error {
		if len(blocks) >= 64 {
			return fmt.Errorf("at most 64 blocks")
		}
		blocks = append(blocks, s)
		return nil
	})
	if err := f.Parse(args[1:]); err != nil {
		return 2
	}
	if f.NArg() != 0 || !filepath.IsAbs(*state) || ((cmd == "grant" || cmd == "revoke" || cmd == "share") && *request == "") || (cmd == "grant" && (len(blocks) == 0 || *class == "")) || (cmd == "revoke" && *grantID == "") || ((cmd == "share" || cmd == "sharing") && len(blocks) != 1) {
		fmt.Fprint(diag, accessHelp)
		return 2
	}
	ctx := context.Background()
	if v := requireCapability(ctx, *state, false, "access_grants_v1"); v.Class != "ok" {
		_ = json.NewEncoder(out).Encode(v)
		return api.Exit(v.Class)
	}
	q := api.Request{RequestID: *request}
	switch cmd {
	case "grant":
		q.Operation = "grant_create"
		q.Grant = &api.GrantSpec{Class: *class, Blocks: blocks}
	case "revoke":
		q.Operation = "grant_revoke"
		q.GrantID = *grantID
	case "grants":
		q.Operation = "grant_list"
	case "audit":
		q.Operation = "audit_list"
		n, err := strconv.ParseUint(*after, 10, 64)
		if err != nil {
			fmt.Fprint(diag, accessHelp)
			return 2
		}
		q.After = n
	case "share":
		q.Operation = "share_set"
		q.Block = blocks[0]
		q.Sharing = *policy
	case "sharing":
		q.Operation = "share_get"
		q.Block = blocks[0]
	}
	v := call(ctx, *state, false, q)
	if err := json.NewEncoder(out).Encode(v); err != nil {
		fmt.Fprintln(diag, "could not write response")
		return 5
	}
	return api.Exit(v.Class)
}
