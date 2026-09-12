# Continuum

One Go daemon per machine that owns your terminals and coding agents — durable history, explicit control, a CLI — so work outlives the client that started it. The runtime under [Menagerie](https://github.com/NakliTechie/menagerie).

Public alpha, AGPL-3.0. The `/v1` API is a versioned contract (`continuum/v1`, contract 1.0 — negotiate with `continuum contract`; see [docs/v1-contract.md](docs/v1-contract.md)). Runs always-on via `continuum service install`; a cutover from the installed `menagerie-relay` service is not yet automated.

## What it does

- **Work survives the client.** Close the tab, drop the network, `kill -9` the daemon — the process keeps running under a per-block holder, and the daemon re-adopts it on restart with the same block ID and PID. Any gap is marked, never hidden.
- **Two doors, one core.** The same binary speaks Menagerie's legacy WebSocket (unchanged) and a versioned `/v1` contract on a private Unix socket — `continuum contract` reports the version and capabilities. One process manager, one journal, one lease model.
- **Observe without stealing.** Many viewers can watch a block; control is a 60-second lease, explicit and revocable.
- **Durable, honest history.** Every event is journaled; retention limits and capture failures surface as typed gaps, not silence. Export a block as asciicast v3.

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

`serve` writes private credentials and a `v1.sock` under your config dir and opens a loopback port for Menagerie only. Use `--state /abs/path` to isolate a workspace; `continuum help` lists every command. Never put this alpha on a public listener.

To run it always-on (launchd on macOS, systemd `--user` on Linux) under its own service label, separate from any installed `menagerie-relay`:

```sh
./bin/continuum service install --listen 127.0.0.1:58750   # fixed port so Menagerie can reconnect
./bin/continuum service status
./bin/continuum service uninstall
```

## With Menagerie

Point Menagerie's Add-relay form at the `ws://` address `serve` prints, using `operator.token` from the state directory. The `menagerie-relay` entry point and the `continuum legacy *` subcommands preserve Menagerie's existing configuration and semantics; an installed-service cutover is a separate, validated step.

## Status & docs

Local alpha `0.1.0-alpha.2-dev`. Gate: `python3 scripts/verify.py verify` (race detector, CLI journey, legacy compatibility). Not yet done: automated cutover from the installed `menagerie-relay` service, structured-session (ACP) restart survival, native Linux and real-provider validation.

- [SPEC.md](SPEC.md) — architecture, failure guarantees, milestones
- [docs/v1-contract.md](docs/v1-contract.md) — the `/v1` contract and how it versions
- [docs/local-alpha.md](docs/local-alpha.md) — the alpha contract and limits
- [docs/terminal-screen-api.md](docs/terminal-screen-api.md) — server-owned screens (screen-v1)
- [docs/service.md](docs/service.md) — running the daemon always-on
- [docs/shared-runtime.md](docs/shared-runtime.md) — Menagerie lineage and cutover
- [docs/legacy-conformance.md](docs/legacy-conformance.md) · [DEFERRED.md](DEFERRED.md) — coverage and later work

Licensed AGPL-3.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE) for the Menagerie relay lineage.
