// Package config loads and writes the relay's TOML configuration.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
)

// DefaultPath is ~/.menagerie/relay.toml.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".menagerie", "relay.toml"), nil
}

// Agent is a configured agent the relay can spawn.
type Agent struct {
	// Command is the executable to run (PATH lookup). Empty for the "custom"
	// shim, which takes its command from spawn.args.
	Command string `toml:"command"`
}

// Config mirrors relay.toml.
type Config struct {
	Name              string           `toml:"name"`
	Listen            string           `toml:"listen"`
	TLSCert           string           `toml:"tls_cert"`
	TLSKey            string           `toml:"tls_key"`
	RegistrationToken string           `toml:"registration_token"`
	AllowedOrigins    []string         `toml:"allowed_origins"`
	Agents            map[string]Agent `toml:"agents"`
	// Tmux controls whether agents run inside a tmux session so they survive a
	// relay restart: "auto" (use tmux if installed — default), "on", or "off".
	Tmux string `toml:"tmux"`
}

// Default returns a fresh config with a generated registration token and the
// v1.0 shim set.
func Default() (*Config, error) {
	tok, err := GenerateToken()
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "relay"
	}
	return &Config{
		Name:              host,
		Listen:            "127.0.0.1:7878",
		Tmux:              "auto",
		RegistrationToken: tok,
		// Origin is the gate. We deliberately do NOT default-allow "null":
		// browsers send Origin: null not only for file:// pages but for any
		// sandboxed iframe, so allowing it would let any website pass the gate.
		// Add "null" (or a "http://localhost:PORT" dev origin) by hand only when
		// doing local file:// development.
		AllowedOrigins: []string{"https://menagerie.naklitechie.com"},
		Agents: map[string]Agent{
			"mini":        {Command: "mini"},
			"claude-code": {Command: "claude"},
			"custom":      {},
		},
	}, nil
}

// GenerateToken returns a 32-byte base64url (unpadded) random token.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// AgentNames returns the configured agent ids, sorted alphabetically (clients
// display them in this order; no agent is featured).
func (c *Config) AgentNames() []string {
	names := make([]string, 0, len(c.Agents))
	for k := range c.Agents {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// OriginAllowed reports whether origin exactly matches the allowlist. "null"
// (a file:// page) is an allowed entry; an empty origin (non-browser client,
// e.g. a supervisor agent) is handled by the caller.
func (c *Config) OriginAllowed(origin string) bool {
	for _, o := range c.AllowedOrigins {
		if o == origin {
			return true
		}
	}
	return false
}

// Load reads a config from path.
func Load(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save writes a config to path, creating the parent dir with 0700 and the file
// with 0600 (the registration token lives here).
func Save(path string, c *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(c)
}

// Exists reports whether a config file is present at path.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
