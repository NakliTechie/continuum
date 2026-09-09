# Continuum

> **Lifecycle:** living — local alpha implemented on the development branch; broader first-release work remains.

A durable runtime for local and remote work. Terminals, coding agents, background jobs, and services share sessions, history, structured events, and controls that people and software can use equally.

Continuum starts from the lessons and working Go relay in Menagerie. Menagerie remains the browser interface for agent fleets. Continuum develops a broader work-session model and CLI, with a proposed shared runtime behind both products. The working name is provisional.

## Direction

- One Go runtime per host, usable by Menagerie, a CLI, and future clients. Keep one implementation of process ownership, session state, authorization, history, and supervision.
- Work survives closing a client. Multiple viewers can observe without taking control from one another. Control is explicit and revocable.
- Local and remote operations have the same API: inspect a machine, browse directories, open a terminal, drive an agent, observe a job, or manage a workspace.
- Structured streams can be merged, filtered, exported as NDJSON, and consumed from Nushell. Cross-machine coordination has a durable owner outside the browser.
- History recovery, limits, retention, and failures are designed in. A recorded byte stream is not a promise of recoverable process state.
- Start with personal machines and explicit trust boundaries. OS-login integration, hostile-network optimization, and production operations are separate later milestones.

## Why Go

Use the language of the existing relay and preserve its tested PTY, ACP, and coordination work. Go's concurrency model fits independent sessions and stream consumers. No measured requirement currently justifies a Rust rewrite. Reconsider a bounded component only if profiling or a platform constraint supplies evidence.

## Can one binary serve both projects?

Yes, as the target architecture. Start by treating the current Menagerie relay as the sole runtime source. Establish compatibility tests, then move runtime ownership once into this repository with history and attribution preserved. The future binary exposes the existing Menagerie protocol through an adapter and a versioned Continuum API over the same session manager. Retain the `menagerie-relay` invocation during migration. Two adapters must never mean two process managers or two writable session stores.

The branch imports the runtime with history and attribution preserved; installed services and Menagerie main have not been cut over. Details: [shared runtime migration](docs/shared-runtime.md).

## Start here

1. [SPEC.md](SPEC.md): proposed architecture, failure guarantees, interfaces, and milestones.
2. [walkthroughs.md](walkthroughs.md): scope decisions with recommended defaults and tradeoffs.
3. [docs/shared-runtime.md](docs/shared-runtime.md): ownership, compatibility matrix, and cutover.
4. [docs/menagerie-baseline.md](docs/menagerie-baseline.md): inspected implementation and existing gaps.
5. [docs/sources.md](docs/sources.md): external references separated from our conclusions.
6. [DEFERRED.md](DEFERRED.md): later work with explicit revisit triggers.

`plan/` contains local working history, pending items, and the ordered workplan; it is deliberately gitignored. Durable requirements and acceptance criteria live in the tracked documents above.

## Current status

Local alpha `0.1.0-alpha.1` is implemented on `runtimev1`. The Go runtime was imported with Menagerie's subtree history preserved. Both adapters use the same session registry. No installed relay or service has been migrated.

The automated gate covers the core with Go's race detector, a real CLI journey and six legacy black-box compatibility cases. A Chrome walk connected the unchanged Menagerie app to the new daemon, spawned `/bin/cat`, and replayed its browser-entered output from the CLI. Linux binaries are cross-build targets; native Linux execution and real model-provider sessions need separate validation.

### Build and try

Requires Go 1.26.2; verification also uses Python 3 and Git.

```sh
go build -o bin/continuum ./cmd/continuum
./bin/continuum serve
```

Leave that terminal running. In another terminal:

```sh
./bin/continuum open -- /bin/cat
./bin/continuum status
./bin/continuum acquire --block BLOCK_ID
printf 'hello\n' | ./bin/continuum input --block BLOCK_ID
./bin/continuum events --block BLOCK_ID --text
./bin/continuum events --block BLOCK_ID --observer --follow
```

Use the block ID returned by `open`. A second `events` command joins as an observer without taking control. Closing these clients leaves the process running. Control expires after 60 seconds; `acquire` obtains unheld control and `takeover` explicitly fences an existing controller. `stop --block BLOCK_ID` uses your saved lease. `status --cursor CURSOR` pages the inventory; `status --block BLOCK_ID` reads one record.

`serve` uses the user's config directory plus `continuum`, a random free loopback port, and private credential files. Use `--state /absolute/private/path` on every command to isolate a workspace. It runs in the foreground and installs no service. Ctrl-C shuts down the daemon and its managed processes. Process groups do not contain commands that deliberately escape into another OS session.

Metadata and retained history survive restart; processes do not in this alpha. Recovered active records become `interrupted`. History from an unclean daemon epoch is conservatively marked incomplete, including exited blocks; the event API returns an `indeterminate` envelope with the retained events. Retention-expired cursors return `history_gap` and the available range. Fresh work after recovery has its own complete/incomplete record. Live capture failure refuses further durable mutations.

Content recording is enabled by default, bounded to 8 MiB/4096 retained events per host; the database's allocated file can be larger. This alpha also caps recorded blocks at 1024 and operation identities at 4096. It has no purge/compaction UI yet. Captured output can contain sensitive text. Input accepts UTF-8 text up to 64 KiB; `--text` emits terminal control bytes and is for trusted output. JSON/NDJSON retains encoded payloads. The CLI is not yet an interactive terminal emulator.

### Use with Menagerie

Point Menagerie's manual Add relay form at `ws://` plus the address printed by `serve`. Use the operator credential in the private state directory's `operator.token`. For a local browser origin, start `serve --origin http://127.0.0.1:PORT`. Hosted Menagerie origins retain their existing allowlist. Never put this alpha on a public listener.

Menagerie's existing browser still uses trusted legacy takeover behavior. A Continuum observer does not steal its token, but an explicit browser reattach fences modern control. A legacy browser may need reconnecting to discover sessions opened elsewhere. Browser folder storage and daemon history are separate; skipping browser storage does not disable daemon capture.

The same executable supports `continuum legacy serve`, `legacy agents`, `legacy token`, `legacy service` and `legacy materialise`; those commands retain Menagerie's existing configuration and semantics. They do not start automatically. The `menagerie-relay` entry point is a thin wrapper over the same module. An installation cutover remains a separately validated operation. See the [public/private source adapter boundary](docs/menagerie-adapter.md).

### Verify

```sh
python3 scripts/verify.py doctor
python3 scripts/verify.py verify
```

The verifier builds fresh binaries and disposable state. It never attaches to the installed relay or invokes real providers. See [feature map](verify/features/README.md), [local alpha contract](docs/local-alpha.md), and [legacy coverage](docs/legacy-conformance.md).

## First release deliverable

An installable Go binary for macOS and Linux, combining host-daemon and CLI modes; a versioned API/agent contract; compatibility with Menagerie's browser; and install, upgrade, recovery and migration documentation. Release acceptance includes persistent session records/history, explicit process-survival limits, multiple observers with controlled input, local/remote directory operations, structured NDJSON streams, durable coordination, and workspace/service lifecycle.

The implemented local milestone is smaller: launch work, disconnect every client, reconnect and recover output, then let a second viewer join without stealing control. Native applications, full SSH replacement and QUIC remain later milestones. See [legacy conformance](docs/legacy-conformance.md) for the first implementation slice and its limits.
