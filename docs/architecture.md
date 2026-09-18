# Continuum architecture

The technical companion to the README: what the pieces are, which surfaces
exist, and what has actually been checked. Contracts live in the documents
linked from each section; this page is the map.

## One daemon, blocks, holders

`continuum serve` is one process per machine. Every terminal or agent it
starts is a **block**: a durable record with an ID, a lifecycle
(`running`, `idle`, `done`, `needs_input`, `stalled`, `rate_limited`,
`exited`, `unknown`) and a bounded journal of output and events. A PTY block's
process is owned by a small per-session **holder** (a `setsid`'d reader that
streams to the daemon over a private socket), so the daemon can be restarted
or `kill -9`'d and re-adopt the process with the same block ID and PID. Any
output the daemon missed is marked, never hidden. Structured (ACP) sessions do
not survive a daemon restart yet.

State lives in a private directory (`--state`, default: the user config dir):
a bbolt journal, `operator.token` / `observer.token`, `v1.sock`, holder
sockets, `serve.log`. Never put the daemon on a public listener.

## Two doors, one core

- **`/v1`** — a versioned contract (`continuum/v1`, contract 1.0) over a Unix
  socket in the state directory. `continuum contract` reports the stable and
  experimental capability tiers; additive within major 1, a breaking change
  changes the socket name. Read-only **observers** and a 60-second fenced
  **control lease** (`acquire`, `renew`, `takeover`, `release`) are the
  authority model. [docs/v1-contract.md](v1-contract.md),
  [docs/local-alpha.md](local-alpha.md).
- **Menagerie door** — the unchanged Menagerie relay protocol (1.3) on a
  loopback port, plus a read-only `/observer` page that accepts observer
  credentials only. The same binary answers as `menagerie-relay` by basename
  and as `continuum legacy …`. `service cutover --adopt-relay` replaces an
  installed relay in place (port, token, origins, agents).
  [docs/menagerie-adapter.md](menagerie-adapter.md),
  [docs/legacy-conformance.md](legacy-conformance.md),
  [docs/shared-runtime.md](shared-runtime.md).

## Recording and terminals

Per-block recording policy: `none`, a committed visible screen, the last N
lines, or full raw output; lifecycle records are always durable. Retention,
`purge`, `retire`, `compact` and capture failures are explicit. `events`
replays from any cursor (`--after`, `--follow`, `--text` for printable text
and colour only, `--raw` for byte-exact output); `export` writes asciicast v3.

`open --terminal screen-v1` opts into a server-owned terminal engine:
`screen` reads the current or final frame, `attach` is an interactive viewer
(Ctrl-] detaches; `--observer` watches, `--takeover` replaces a controller).
Children get a UTF-8 locale and an IUTF8 PTY.
[docs/terminal-screen-api.md](terminal-screen-api.md),
[docs/terminal-engine.md](terminal-engine.md).

## Always-on service

`continuum service install --listen 127.0.0.1:PORT` writes a launchd agent
(macOS) or a systemd `--user` unit (Linux) under its own label, embedding the
absolute path of the binary that ran the install; re-run it from a new binary
to repoint the unit. Service replacement kills only the daemon so holders and
their PTYs survive. [docs/service.md](service.md).

## Experimental surfaces

All of these are gated as experimental in `continuum contract`.

- **SSH clients and scoped directories.** `--host user@host --state /remote`
  runs one SSH process per request as a one-shot JSON stdio bridge; bearer
  tokens stay on the owning host. `serve --browse-root DIR` opts into
  operator-only directory browsing (`directories`), foreground only.
  [docs/remote-directories.md](remote-directories.md).
- **Composed streams.** `interleave` merges bounded, source-labelled event
  pages from several blocks and hosts into NDJSON with per-source cursors and
  checkpoints; `--control` pauses, continues or cancels the observer, never
  the workload. [docs/streams.md](streams.md).
- **Durable waits.** `wait create --mode any|all` over local blocks and pinned
  SSH peers; admission needs an initial observation from every source,
  outcomes are typed and replay-stable, `history: incomplete` marks a source
  whose recording was degraded. [docs/coordination.md](coordination.md).
- **Managed workspaces.** `workspace plan|run|status|stop|destroy` and
  `materialise` provision worktrees from a hash-pinned fleet spec in a fixed
  order (ports → files → commands → services → health), with supervision and
  explicit teardown; `run` requires `--trust` with the spec's exact hash.
  [docs/workspace-lifecycle.md](workspace-lifecycle.md),
  [docs/fleet-mirror.md](fleet-mirror.md).
- **Scoped access and recovery.** `access` mints show-once per-block
  observer/controller/moderator grants, shares and revokes, and reads the
  bounded audit; `backup` creates or restores a checked offline copy of the
  state. [docs/access-operations.md](access-operations.md).

## Verification

`python3 scripts/verify.py verify` is the gate: race-detected package tests,
`govulncheck`, and CLI / terminal / legacy / upgrade / remote / streams /
managed journeys plus a network-free check of the WAN runner. Opt-in features:
`real-acp` (a local Ollama model, no cloud credentials), `CONTINUUM_TEST_NU`
(Nushell pipeline), the two-host script and the
[three-server WAN drill](wan-multi-server-test.md) over trusted SSH.

What has run against `v0.1.0-alpha.2`: the full gate on macOS/arm64; the
three-server WAN drill from a macOS coordinator to two temporary Linux/amd64
hosts (AWS Mumbai and N. Virginia, ~0.5 s and ~3.5 s per SSH bridge call);
the Menagerie CU1–CU7 client follow-through in a browser against this daemon.
Earlier trees: a Linux/arm64 Docker core gate, a two-macOS-host fault drill,
and OMP 18 with local Ollama over ACP. Not run: a Linux systemd service
install, an arm64 remote host, lossy-WAN benchmarks, cloud ACP providers.

## Where to read next

- [SPEC.md](../SPEC.md) — architecture, failure guarantees, milestones
- [CHANGELOG.md](../CHANGELOG.md) — releases
- [docs/development-artifacts.md](development-artifacts.md) — release vs local builds
- [docs/acp-protocol.md](acp-protocol.md), [docs/acp-turns.md](acp-turns.md), [docs/acp-startup.md](acp-startup.md), [docs/acp-capture.md](acp-capture.md) — structured sessions
- [DEFERRED.md](../DEFERRED.md) — later work
