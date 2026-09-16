# Stream composition (Batch D2)

> Lifecycle: living. Extends SPEC §6 using the existing authorized `events`
> read API. Client subscriptions are not durable broker waits (Batch D3).

`continuum interleave --source JSON [--source JSON ...] [--type TYPE ...]
[--follow] [--control]` merges up to eight block subscriptions. Each source has
`name`, absolute owning-host `state`, `block_id`, optional `after` cursor,
`host` and `remote_binary` (D1 SSH transport), and `observer` (defaults true).
Source names are unique ASCII identifiers, not executable commands. No inline
tokens, shell commands or inferred authority. An explicit `observer:false`
selects the operator credential without acquiring any control lease.

The CLI negotiates `event_replay` and verifies block existence independently
for each source. Initial status pins host identity. Follow mode revalidates status
approximately once a second between pages while unpaused and output is writable;
host replacement, retirement and withdrawn capability fail the source closed,
and an exited block with no retained exit event can still finish. Each worker fetches one
bounded page at a time; a quiet/unreachable source cannot hold up other sources.
Events retain per-source journal order; there is no global time sort or clock
agreement. Snapshot mode fixes a high-water mark from each source's first page.
`--follow` continues until each block exits, fails or is explicitly cancelled.

Every stdout line is a versioned JSON object with `v:1` and `kind`. Event rows
have `source`, `host_id`, `block_id`, `seq`, `cursor`, original `time`/`type`/
`payload`, plus `event:[interval,code,data]`: an asciicast-v3-shaped triple paired
with block/source identity. UTF-8 PTY output uses `code:o`, `encoding:utf8`.
Non-UTF-8 output uses `encoding:base64` without corrupting original bytes.
Non-output/unknown events use marker code `m` and retain the original payload.
Intervals are per source between emitted events (first is zero, backward clocks
clamp to zero). This is an NDJSON composition format, not a standalone asciicast
file: use `export` for that. No raw terminal controls are written to stdout.
DEL/C1 controls are JSON-escaped too. This is standalone transport JSON, not
HTML-safe markup; consumers must not embed it directly into an HTML script.

Exact-type filters affect only event rows, never source errors, control replies,
checkpoints or the end summary. Checkpoints include recording-policy/purge metadata;
unchanged quiet checkpoints are suppressed. Checkpoints advance over filtered records only
after earlier selected records were written. The last emitted cursor is a resume
position for that host/block; it is not a guarantee the downstream application
processed the row. Cursor replay is at-least-once, not a distributed transaction.
Unknown event types and structured ACP payloads remain intact by default.

With `--control`, stdin accepts bounded NDJSON commands:

```json
{"request_id":"pause-1","operation":"pause","source":"build"}
{"request_id":"continue-1","operation":"continue","source":"build"}
{"request_id":"cancel-1","operation":"cancel","source":"build"}
```

Replies appear in-stream (`kind:control`) with matching request ID and the last
emitted cursor. Pause stops that observer's delivery/polling, not its process or
other observers. At most one in-flight page can finish and remain buffered;
after the pause acknowledgement no event for that source is emitted until its
continue acknowledgement. Commands take effect between output writes, so replies
wait behind an already blocked stdout. Cancel retires just that subscription.
Duplicate IDs within the last 128 replies replay the reply; a different command
with the same retained ID conflicts. EOF closes only the control input; it does
not cancel subscriptions. Malformed/oversized commands produce explicit errors.

Backpressure is bounded: at most eight in-flight/retained pages (256 events and
16 MiB encoded JSON each), one encoded output row (32 MiB maximum), and one
pending control command (16 KiB), plus the last 128 bounded commands/replies for
correlation. Fetching pauses when a source already owns a
page. A blocked stdout stops delivery/fetch scheduling, never PTY/journal writes.
These are retained protocol budgets, not an exact RSS ceiling: JSON encoding and
decoding also create bounded transient allocations.
Interrupt/deadline cancels local/SSH reads and unblocks owned stdout/stdin file
handles; it never sends stop/input to a workload. Retention can overtake a paused
source: emit `history_gap` and stop that source, never silently skip ahead.
Capture degradation is emitted before retained events and causes nonzero exit.

Source failures are isolated and represented in-stream; successful sources can
finish. End summarizes every source's state/class/code/cursor. Exit is the
highest numeric API failure code seen (not an arrival-order-dependent winner),
zero for complete/explicitly cancelled observations, 130 for external interrupt,
5 for output failure. `--timeout DURATION` bounds the whole observation (exit 130).
Host identity/order/payload violations fail that source closed. Missing or retired
blocks are errors, not successful empty output. Polling and SSH are D1 transport;
server-pushed subscriptions/reconnect retries remain deferred until measured
network needs justify them. Resume explicitly using the emitted cursor.

## Nushell and verification

Nushell's [line-delimited JSON parser](https://www.nushell.sh/commands/docs/from_json.html)
supports `from json --objects`. The gate will exercise the precise live pipeline
and filtering, not just parse a saved fixture. `nu --no-config-file` prevents
user startup scripts/configuration from affecting the test. `CONTINUUM_TEST_NU`
may point at a pinned disposable binary; no global installation is required.
Missing Nushell must be reported as an unverified interoperability gate.

In Nushell, substitute an actual state path and block ID:

```nu
continuum interleave --source '{"name":"build","state":"/absolute/state","block_id":"0123456789abcdef"}' --type output
  | from json --objects
  | where kind == event
  | select source block_id seq event
```

Keep source/checkpoint/end rows too when detecting partial results or saving resume
cursors; the display-only example above deliberately selects just event rows.

Use deterministic fake pages for quiet sources, ordering, unknown/binary events,
filter checkpoints, retention/degradation, malformed responses, backpressure,
control correlation and cancellation. Use private real daemons and the D1 SSH
shim for end-to-end merging and pause/continue without stopping work. Real
two-host network faults remain the separate D4 gate.

Focused gate observed on macOS arm64 with Nushell 0.114.1 on 2026-09-16:
`CONTINUUM_TEST_NU=/path/to/nu python3 scripts/verify.py verify streams` passed.
The disposable official release archive's SHA-256 was verified against its release
metadata (`cb0ac9b97b985cd217a952f44438a14d2df2a95504d9bd8bc01ec73e1351b179`).
No global installation, installed-service changes or model calls were needed.

The full seven-feature `python3 scripts/verify.py verify` gate also passed with
that Nushell binary: core race tests, vet, vulnerability scan, CLI/terminal journeys,
legacy, pinned schema upgrade/rollback, SSH bridge and streams. The upgrade journey
also consumed the pinned schema-1 daemon with the new stream client. A Linux amd64
cross-build succeeded; native Linux and real two-host network behavior remain
unverified by these runs.

Direct security review: this CLI only sends authorized `status`/`events` reads;
pause/continue/cancel never acquire leases or send workload mutations. Source and
command sizes, response/page/order validation and bounded reply caching prevent
unbounded queues. Host identity is pinned and periodically revalidated; output is
JSON-escaped and never raw terminal control bytes. Remote bearer credentials remain
on the owning host through D1's SSH bridge. As with D1, choosing observer credentials
does not restrict the underlying SSH account's OS authority. No browser UI changed;
the user-facing walkthrough is the private real-binary CLI/structured-shell journey.
