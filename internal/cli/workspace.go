package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/NakliTechie/continuum/materialise"
	"github.com/NakliTechie/continuum/workspace"
)

const workspaceHelp = `Usage: continuum workspace plan|run --state DIR --spec FILE --name NAME [--trust SHA256]
       continuum workspace status --state DIR [--name NAME]
       continuum workspace resume --state DIR --name NAME --trust SHA256
       continuum workspace stop --state DIR --name NAME [--retry-uncertain]
       continuum workspace destroy --state DIR --name NAME --confirm NAME [--retry-uncertain]
Managed shell requires the exact spec hash printed by plan. Stop and destroy
do not automatically retry a command whose effect was uncertain.
`

func workspaceCLI(args []string, out, diag io.Writer) int {
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(out, workspaceHelp)
		return 0
	}
	cmd := args[0]
	if cmd != "plan" && cmd != "run" && cmd != "status" && cmd != "resume" && cmd != "stop" && cmd != "destroy" {
		fmt.Fprint(diag, workspaceHelp)
		return 2
	}
	f := flag.NewFlagSet("workspace "+cmd, flag.ContinueOnError)
	f.SetOutput(diag)
	state := f.String("state", defaultState(), "private state directory")
	specPath := f.String("spec", "", "fleet spec")
	name := f.String("name", "", "workspace name")
	trust := f.String("trust", "", "exact reviewed SHA256")
	confirm := f.String("confirm", "", "confirm workspace destruction by name")
	retry := f.Bool("retry-uncertain", false, "explicitly rerun an uncertain lifecycle command")
	if err := f.Parse(args[1:]); err != nil {
		return 2
	}
	if f.NArg() != 0 || !filepath.IsAbs(*state) || (cmd != "status" && workspace.ValidName(*name) != nil) || ((cmd == "plan" || cmd == "run") && *specPath == "") {
		fmt.Fprint(diag, workspaceHelp)
		return 2
	}
	if cmd == "destroy" && *confirm != *name {
		fmt.Fprintln(diag, "destroy requires --confirm matching the exact workspace name")
		return 2
	}
	p := workspace.New(*state)
	e := materialise.New(p)
	switch cmd {
	case "plan":
		spec, res, hash, err := e.PlanManaged(*specPath, *name)
		if err != nil {
			fmt.Fprintln(diag, "workspace plan unavailable:", err)
			return 2
		}
		fmt.Fprintf(out, "spec_sha256=%s\n", hash)
		fmt.Fprint(out, materialise.RenderPlan(spec, res))
	case "run":
		_, _, hash, err := materialise.ManagedSpec(*specPath)
		if err != nil {
			fmt.Fprintln(diag, "workspace spec unavailable:", err)
			return 2
		}
		if *trust != hash {
			fmt.Fprintf(diag, "review the spec and pass --trust %s\n", hash)
			return 2
		}
		res, err := e.RunManaged(*specPath, *name, *trust)
		if err != nil {
			fmt.Fprintln(diag, "workspace run failed; inspect its private state and operator logs before retrying")
			return 1
		}
		_ = json.NewEncoder(out).Encode(res)
		if res.State != workspace.StateReady {
			return 1
		}
	case "status":
		if *name != "" {
			r, err := p.Load(*name)
			if err != nil || r == nil {
				fmt.Fprintln(diag, "workspace not found")
				return 1
			}
			_ = json.NewEncoder(out).Encode(r)
			return 0
		}
		r, err := p.Managed()
		if err != nil {
			fmt.Fprintln(diag, "workspace records unavailable")
			return 1
		}
		_ = json.NewEncoder(out).Encode(r)
	case "resume":
		r, err := p.Load(*name)
		if err != nil || r == nil || r.ManagedHash == "" {
			fmt.Fprintln(diag, "managed workspace not found")
			return 1
		}
		_, _, hash, err := materialise.ManagedSpec(r.ManagedSpec)
		if err != nil || hash != r.ManagedHash || hash != *trust {
			fmt.Fprintln(diag, "spec changed or trust hash does not match; supervision remains suspended")
			return 2
		}
		if err := p.ArmManaged(*name, hash); err != nil {
			fmt.Fprintln(diag, "could not resume managed supervision:", err)
			return 1
		}
		fmt.Fprintln(out, "supervision armed; the owning daemon will check on its next cadence")
	case "stop":
		if err := e.StopManaged(*name, *retry); err != nil {
			fmt.Fprintln(diag, "stop failed or uncertain; inspect state before retrying:", err)
			return 1
		}
		fmt.Fprintln(out, "workspace stopped")
	case "destroy":
		if err := e.DestroyManaged(*name, *retry); err != nil {
			fmt.Fprintln(diag, "destroy stopped safely; inspect state before retrying:", err)
			return 1
		}
		fmt.Fprintln(out, "clean workspace removed; branch retained or deleted according to the trusted spec")
	}
	return 0
}
