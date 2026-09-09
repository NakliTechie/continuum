// fleetcheck validates fleet specs from the command line. With -json it prints
// one object keyed by "<dir>/<file>" mapping to that fixture's issues, which is
// what the client mirror-check compares against.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/NakliTechie/menagerie/relay-go/fleet"
)

func main() {
	asJSON := flag.Bool("json", false, "print every fixture's issues as one JSON object")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: fleetcheck [-json] <spec.json | testdata dir>")
		os.Exit(2)
	}
	target := flag.Arg(0)
	info, err := os.Stat(target)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	if !info.IsDir() {
		b, err := os.ReadFile(target)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		_, issues := fleet.ValidateBytes(b)
		for _, is := range issues {
			fmt.Printf("%s\t%s\t%s\n", is.Path, is.Code, is.Message)
		}
		if len(issues) > 0 {
			os.Exit(1)
		}
		return
	}

	out := map[string][]fleet.Issue{}
	for _, dir := range []string{"valid", "invalid", "secrets", "normalises"} {
		entries, err := os.ReadDir(filepath.Join(target, dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(target, dir, e.Name()))
			if err != nil {
				continue
			}
			_, issues := fleet.ValidateBytes(b)
			if issues == nil {
				issues = []fleet.Issue{}
			}
			out[dir+"/"+e.Name()] = issues
		}
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "")
		_ = enc.Encode(out)
	}
}
