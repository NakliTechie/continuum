# Local alpha contract

> Lifecycle: locked for implementation, 2026-09-09. Engineering choices under the owner’s instruction to build; broader first-release gates remain in SPEC.md.

One canonical runtime source is being transferred with history into Continuum on runtimev1. Menagerie migration is prepared separately; installed services and main branches are not cut over by these changes.

The first slice runs a single loopback server per private state directory, with legacy WebSocket and modern authenticated HTTP adapters over the same process registry. The modern CLI can open a PTY, inspect sessions, page/stream recorded events, claim/release/take over control, send input, resize, and stop. Observers never attach through the legacy takeover operation. Modern leases last 60 seconds; a fresh acquisition/takeover fences prior leases, including legacy token rotation. Authority is personal-host operator access in this alpha, not grants for untrusted users.

Use bbolt transactions with fsync enabled for host identity, operation intent/results, bounded event history and block metadata. This avoids a hand-written crash journal and CGO; SQLite remains a future query-driven option, not a prerequisite. Reference: https://pkg.go.dev/go.etcd.io/bbolt. Start with 8 MiB and 4096 retained events per host, up to 1024 recorded blocks and 4096 operation records; limits return resource_exhausted. Retention gaps are explicit. Events are committed before observers read them. A failed capture marks degradation; mutations requiring durable state fail. Polling observers hold no long-lived transaction and cannot backpressure a process.

New captures use the explicit private state directory, not the installed Menagerie home. Metadata/history survive restart; in this slice PTY processes do not. Previously active blocks are marked interrupted on startup. Legacy compatibility mode retains its existing configuration and tmux behavior. No adoption of live tmux sessions is performed by the modern mode.

Each mutation has a caller-supplied request ID, stored with a request digest before effects. Repeated IDs return the saved result; mismatched payloads conflict; unresolved intents return indeterminate after a crash, never silently repeat a launch. Token-bearing control results are not journaled; an uncertain acquire requires a new explicit takeover. Input completion confirms the write, not execution of the command.

Acceptance: same process observed by two clients without takeover; no-client output replay; explicit takeover fences old input including legacy attach; restart recovers records with interrupted status; duplicate/conflicting intents; bounded replay with gaps; database lock and incompatible schema refusal; auth rejection; real CLI first-run and actionable errors. Remote hosts, native GUI, tmux continuation, real ACP provider validation and full release packaging remain separate gates.
