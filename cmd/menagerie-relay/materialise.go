package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/NakliTechie/menagerie/relay-go/fleet"
	"github.com/NakliTechie/menagerie/relay-go/materialise"
	"github.com/NakliTechie/menagerie/relay-go/workspace"
)

// cmdMaterialise runs one workspace's materialisation, or — with --dry-run —
// prints the plan it would run without touching the box. The dry run is what the
// browser shows before a launch is committed, and what the golden-file test
// compares against.
func cmdMaterialise(args []string) {
	fs := flag.NewFlagSet("materialise", flag.ExitOnError)
	specPath := fs.String("spec", "fleet.json", "path to the fleet spec")
	name := fs.String("name", "w1", "workspace name")
	repo := fs.String("repo", ".", "repository root the workspace is cut from")
	home := fs.String("home", "", "relay home (default ~/.menagerie)")
	dry := fs.Bool("dry-run", false, "print the plan; allocate nothing, write nothing, run nothing")
	_ = fs.Parse(args)

	b, err := os.ReadFile(*specPath)
	if err != nil {
		fatal(err)
	}
	spec, issues := fleet.ValidateBytes(b)
	if len(issues) > 0 {
		for _, is := range issues {
			fmt.Fprintf(os.Stderr, "%s\t%s\t%s\n", is.Path, is.Code, is.Message)
		}
		os.Exit(1)
	}

	h := *home
	if h == "" {
		hd, err := os.UserHomeDir()
		if err != nil {
			fatal(err)
		}
		h = filepath.Join(hd, ".menagerie")
	}
	repoAbs, err := filepath.Abs(*repo)
	if err != nil {
		fatal(err)
	}

	eng := materialise.New(workspace.New(h))
	eng.DryRun = *dry
	res, err := eng.Run(spec, repoAbs, *name)
	if err != nil {
		fatal(err)
	}
	if *dry {
		fmt.Print(materialise.RenderPlan(spec, res))
		return
	}
	fmt.Printf("workspace %s is %s at %s\n", res.Workspace.Name, res.State, res.Workspace.Path)
	for _, f := range res.Failed {
		fmt.Fprintf(os.Stderr, "failed probe: %s\n", f)
	}
	if res.State != workspace.StateReady {
		os.Exit(1)
	}
}
