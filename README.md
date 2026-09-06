# menagerie-relay

The reference Menagerie relay: a single Go binary that exposes a WebSocket the browser (or a supervisor agent) connects to, and — from P3 — manages PTYs for spawned coding agents. It implements the protocol in [`../protocol/`](../protocol/).

## Build

```sh
go build ./cmd/menagerie-relay   # produces ./menagerie-relay
go test ./...
go vet ./...
```

No runtime deps beyond the Go module set: `coder/websocket`, `BurntSushi/toml` (and `creack/pty` once PTY support lands in P3).

## Run

```sh
./menagerie-relay init     # writes ~/.menagerie/relay.toml, prints a registration token (once)
./menagerie-relay serve    # starts the relay (default 127.0.0.1:7878)
```

- `menagerie-relay agents` — list the agents detected on this machine's PATH, and the known ones that aren't installed
- `menagerie-relay token print` — re-print the registration token
- `menagerie-relay token rotate` — issue a new token (invalidates the old; clients must re-register)

Paste the registration token into Menagerie → Settings → Add relay. See [`examples/`](./examples/) for `relay.toml.example` and systemd / launchd units.

## Layout

```
cmd/menagerie-relay/   CLI entrypoint (init / serve / token)
internal/protocol/     Go port of ../protocol/types.ts (canonical shapes live there)
internal/config/       relay.toml load/save + token generation
internal/server/       WebSocket server: hello + register  (+ PTY from P3)
internal/pty/          PTY management                       (P3)
internal/shims/        spawn shims: generic (any configured agent) + custom
examples/              relay.toml.example, systemd + launchd units
```

## Agents

The relay detects agents rather than hardcoding a menu. `internal/config/agents.go` holds `KnownAgents` — a registry of coding-agent CLIs (id → executable) — and at startup `ResolveAgents` probes each command on PATH and advertises only what it finds. So the browser's dropdown lists what this machine can actually spawn, and never an agent whose spawn would fail.

- Anything written in `relay.toml` is always offered, detected or not, and its `command` overrides the registry — that's how you point an id at a wrapper script or an off-PATH binary.
- `custom` is always available; it takes its command from `spawn.args`.
- The registry ships in the binary and is refreshed at release time. It is never fetched at runtime: the relay would then be taking the names of programs it executes from the network, and a periodic check would be a phone-home.

Adding an agent is a registry row, not code — every agent except `custom` runs through the one `Generic` shim. Write a new shim only for an agent that needs different spawn mechanics; see `../docs/writing-a-shim.md`.

## Status

**P2 skeleton** — `hello` + `register` handshake, origin enforcement, token-auth scaffold. No PTY yet; `spawn` returns an error until P3.
