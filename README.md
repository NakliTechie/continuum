# Continuum

One Go daemon per machine that owns your terminals and coding agents — durable history, explicit control, a CLI — so work outlives the client that started it. The runtime under [Menagerie](https://github.com/NakliTechie/menagerie).

Public alpha, AGPL-3.0. The `/v1` API is a versioned contract (`continuum/v1`, contract 1.0 — negotiate with `continuum contract`; see [docs/v1-contract.md](docs/v1-contract.md)). Runs always-on via `continuum service install`, or `continuum service cutover` to replace an installed `menagerie-relay` in place (adopting its port, token, origins and agents).

## What it does

- **Work survives the client.** Close the tab, drop the network, `kill -9` the daemon — the process keeps running under a per-block holder, and the daemon re-adopts it on restart with the same block ID and PID. Any gap is marked, never hidden.
- **Two doors, one core.** The same binary speaks Menagerie's legacy WebSocket (unchanged) and a versioned `/v1` contract on a private Unix socket — `continuum contract` reports the version and capabilities. One process manager, one journal, one lease model.
- **Observe without stealing.** Many viewers can watch a block; control is a 60-second lease, explicit and revocable.
- **Durable, honest history.** Choose `none`, a committed visible screen, the last N lines, or full raw output per block; lifecycle records remain durable. Retention limits, purge, and capture failures are explicit. Export retained output as asciicast v3.

## Quick start

Requires Go 1.26.8.

```sh
go build -o bin/continuum ./cmd/continuum
./bin/continuum serve              # leave this running
```

In another terminal:

```sh
./bin/continuum open -- /bin/sh
./bin/continuum status
./bin/continuum events --block BLOCK_ID --follow --text
```

Choose retention when opening a block, then manage it explicitly:

```sh
./bin/continuum open --recording lines:500 -- /bin/sh
./bin/continuum purge --block BLOCK_ID      # exited blocks; keep lifecycle metadata
./bin/continuum retire --block BLOCK_ID     # exited blocks only
./bin/continuum compact                     # stop the daemon first
```

`serve` writes private credentials and a `v1.sock` under your config dir and opens a loopback port for Menagerie only. Use `--state /abs/path` to isolate a workspace; `continuum help` lists every command. Never put this alpha on a public listener.

To run it always-on (launchd on macOS, systemd `--user` on Linux) under its own service label, separate from any installed `menagerie-relay`:

```sh
./bin/continuum service install --listen 127.0.0.1:58750   # fixed port so Menagerie can reconnect
./bin/continuum service status
./bin/continuum service uninstall
```

## With Menagerie

Point Menagerie's Add-relay form at the `ws://` address `serve` prints, using `operator.token` from the state directory. The `menagerie-relay` entry point and the `continuum legacy *` subcommands preserve Menagerie's existing configuration and semantics; an installed-service cutover is a separate, validated step.

## SSH clients and scoped directory browsing (experimental)

On the owning host, opt into browsing one or more roots when starting a foreground daemon:

```sh
continuum serve --state /absolute/state --browse-root /absolute/workspaces
continuum directories --state /absolute/state --path /absolute/workspaces --limit 50
```

With an existing trusted SSH host/key and Continuum on that host:

```sh
continuum directories --host user@host --state /remote/state
continuum open --host user@host --state /remote/state --cwd /remote/workspace -- /bin/sh
```

`--remote-binary /absolute/path/to/continuum` selects a non-PATH installation.
Daemon bearer tokens remain on the owning host. Browsing is operator-only, defaults off,
and does not change execution authority. Browse roots currently require `serve`
(service-unit persistence is not implemented). Remote calls use one SSH process
per request; real-network performance and fault testing remain outstanding.
See [the directory/SSH contract](docs/remote-directories.md).

## Compose block streams (experimental)

```sh
continuum interleave \
  --source '{"name":"build","state":"/absolute/state","block_id":"LOCAL_BLOCK_ID"}' \
  --source '{"name":"tests","host":"user@host","state":"/remote/state","block_id":"REMOTE_BLOCK_ID"}' \
  --type output
```

Replace the block IDs with full IDs from `open` or `status`. Output is source-labelled
NDJSON with original payloads, per-source cursors and asciicast-shaped event triples.
Add `--follow` for ongoing observation and `--control` for stdin pause/continue/cancel
commands. These controls affect the observer only, never the workload. Sources use
observer credentials by default. See [stream limits, control replies and Nushell
usage](docs/streams.md).

## Status & docs

Local alpha `0.1.0-alpha.2-dev`. Gate: `python3 scripts/verify.py verify` (race detector, CLI/terminal journeys, legacy, schema-upgrade, isolated SSH-bridge compatibility and stream composition). Set `CONTINUUM_TEST_NU` to a Nushell binary to include its live pipeline check. Not yet done: structured-session (ACP) restart survival, native Linux, real-network remote and broader real-provider validation.

- [SPEC.md](SPEC.md) — architecture, failure guarantees, milestones
- [docs/v1-contract.md](docs/v1-contract.md) — the `/v1` contract and how it versions
- [docs/local-alpha.md](docs/local-alpha.md) — the alpha contract and limits
- [docs/terminal-screen-api.md](docs/terminal-screen-api.md) — server-owned screens (screen-v1)
- [docs/service.md](docs/service.md) — running the daemon always-on
- [docs/shared-runtime.md](docs/shared-runtime.md) — Menagerie lineage and cutover
- [docs/legacy-conformance.md](docs/legacy-conformance.md) · [DEFERRED.md](DEFERRED.md) — coverage and later work

Licensed AGPL-3.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE) for the Menagerie relay lineage.
