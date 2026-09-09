package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCheckerArgvLoadSaveResolve(t *testing.T) {
	for _, name := range []string{"omp", "gemini", "claude-code-acp", "unknown-checker"} {
		for _, explicit := range []bool{false, true} {
			t.Run(name+map[bool]string{true: "-empty", false: "-omitted"}[explicit], func(t *testing.T) {
				p := filepath.Join(t.TempDir(), "config.toml")
				body := "[agents." + name + "]\ncommand='checker'\ntransports=['acp']\n"
				if explicit {
					body += "acp_args=[]\n"
				}
				if err := os.WriteFile(p, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				c, err := Load(p)
				if err != nil {
					t.Fatal(err)
				}
				if (c.Agents[name].ACPArgs != nil) != explicit {
					t.Fatalf("load lost distinction: %#v", c.Agents[name].ACPArgs)
				}
				if err := Save(p, c); err != nil {
					t.Fatal(err)
				}
				c, err = Load(p)
				if err != nil {
					t.Fatal(err)
				}
				if (c.Agents[name].ACPArgs != nil) != explicit {
					t.Fatalf("save lost distinction: %#v", c.Agents[name].ACPArgs)
				}
				c.ResolveAgents(func(string) (string, error) { return "", errors.New("absent") })
				want := []string{}
				if !explicit {
					if known, ok := KnownAgents[name]; ok {
						want = known.ACPArgsOrDefault()
					} else {
						want = []string{"acp"}
					}
				}
				got := c.Agents[name].ACPArgsOrDefault()
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("resolved %#v want %#v", got, want)
				}
			})
		}
	}
}
