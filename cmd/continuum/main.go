package main

import (
	"github.com/NakliTechie/continuum/internal/cli"
	"github.com/NakliTechie/continuum/legacy"
	"os"
	"path/filepath"
)

func main() {
	if filepath.Base(os.Args[0]) == "menagerie-relay" {
		legacy.Main()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "legacy" {
		if len(os.Args) == 2 {
			// A bare `continuum legacy` must not start a relay and write a
			// config as a side effect of exploring; the subcommand is explicit.
			os.Stderr.WriteString("continuum legacy needs a subcommand: serve, service, init, agents, token, materialise (run `continuum legacy help`)\n")
			os.Exit(2)
		}
		os.Args = append(os.Args[:1], os.Args[2:]...)
		legacy.Main()
		return
	}
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
