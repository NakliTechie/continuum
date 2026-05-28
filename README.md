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
internal/shims/        per-agent shims: mini, claude-code, custom (P3+)
examples/              relay.toml.example, systemd + launchd units
```

## Adding a shim (P3+)

Each shim is a Go file in `internal/shims/` implementing the `Shim` interface (spawn + idle / needs-input heuristics). A how-to lands in `../docs/writing-a-shim.md` (P6).

## Status

**P2 skeleton** — `hello` + `register` handshake, origin enforcement, token-auth scaffold. No PTY yet; `spawn` returns an error until P3.
