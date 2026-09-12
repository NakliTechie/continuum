# Continuum — specification

> **Lifecycle:** draft — 2026-09-09. Requirements derive from the owner request; technical choices below are recommended defaults, not individually owner-locked decisions.

## 0. Agent contract — DRIVER pass completed 2026-09-09

This pass applies ntkit DRIVER.md to the first spec draft. It defines design requirements, not shipped features. The implemented local subset and selected storage/control defaults are in docs/local-alpha.md. Broader packaging and remote-sharing decisions remain open in walkthroughs.md.

### 0.1 One perception act

`continuum status --json` is the proposed entry point: one bounded response containing protocol/capability versions, host reachability and observation age, active work counts, actionable attention items, capture degradation, pending approvals, outstanding operations and their next actions. Its snapshot is consistent per host, not globally atomic across disconnected hosts. Unreachable/unknown and stale last-known state are distinct from stopped or completed work. Reads never acknowledge attention, claim control, or resolve approvals.

The default response contains at most 20 attention items and 64 KiB of JSON; return totals, truncation, a snapshot ID and cursor for the rest. Detail reads and history pages have explicit byte/event limits. Streaming is an opt-in operation with bounded buffers, deadline/cancellation and backpressure. Defaults do not print transcripts, secrets or an ever-growing inventory. A healthy human-readable summary is one line.

### 0.2 Machine-decidable outcomes

Separate work lifecycle from operation outcome and from transport health. A command envelope carries `schema_version`, `request_id`, `operation_id` when accepted, `class`, `code`, `durability`, and a typed `next_action`. Human prose explains; agents branch on codes. No success is inferred from missing data.

| Class / CLI exit | Required next action |
|---|---|
| `ok` / 0 | Use the result; if asynchronous, inspect the returned operation ID. `accepted` never means work finished. |
| `invalid_request` / 2 | Correct the named field using supported values; do not repeat unchanged. |
| `access_denied` / 3 | Obtain the named scope from an authorized operator; never retry with broader credentials automatically. |
| `decision_required` / 4 | Present the specific preview/permission request to the authorized decision-maker. |
| `unreachable` / 5 | Reconnect with bounded backoff; retain the same operation identity. |
| `conflict` / 6 | Refresh the named revision/control lease before acting. |
| `history_gap` / 7 | Fetch the available snapshot/range; do not silently claim a complete transcript. |
| `indeterminate` / 8 | Reconcile the operation/evidence; do not repeat a possibly executed mutation. |
| `unsupported` / 9 | Choose an advertised alternative or a compatible runtime version. |
| `resource_exhausted` / 10 | Resolve the named disk/buffer/quota limit before retrying. |

This is a proposed CLI contract; legacy wire error codes remain unchanged in their adapter. Each error includes field/scope/resource identifiers, retryability, and `next_action {kind, operation, arguments}` drawn from a closed, schema-versioned vocabulary. Human help renders that action as an exact command without embedding credentials. Clients reject unknown required verdict semantics as indeterminate, preserving original data for diagnosis.

### 0.3 Crash-safe intent, memory and authority

Persist principal-scoped request ID, digest, intent, outcome and result locator outside client context. Same ID plus same request returns the existing operation; same ID plus different request conflicts. Persist before acknowledging durability, and record whether an external side effect is confirmed or uncertain. A crashed client can ask for the operation by ID; an uncertain process launch is reconciled, never blindly repeated. Cancellation stops observation unless an explicit authorized stop operation was requested.

The runtime retains the trajectory: events, approvals, control changes, operation results and degraded-capture episodes. `status` renders the actionable state; bounded event/operation reads explain it. Agent-authored text is payload, never authority or a policy instruction. Exact command arguments and provenance are preserved subject to recording policy; tokens are excluded.

### 0.4 Linked layers and accumulating evidence

```text
status / intent                     -> bounded situation and next action
work session / workspace            -> related blocks, resources, ownership
operation / wait / approval         -> effects, deadlines, durable outcomes
block / subscription / control      -> lifecycle, observation, exclusive input
adapters / execution / journal      -> host effects, ordering, recovery
operator policy / verifier          -> authority and independent evidence
```

Higher-level operations return stable references to their lower-level evidence. Users can descend for detail without rediscovering state. Repeated unresolved failures keep their identity and attempted remedies so a new agent does not circle through the same failed action. A confirmed escaped defect becomes a named regression case; compatibility baselines are pinned and versioned rather than overwritten to make a failing test pass. These mechanisms accumulate knowledge; they do not self-authorize changes or learn policy from terminal output.

### 0.5 Evaluator boundary

Agent output and self-reported status are not evidence that a release gate passed. Verifiers execute independently from the workload and record their own inputs, version, exit/result and evidence locator. A workload cannot edit the policy or verifier through its granted API. If the workload shares an OS identity that can modify verifier files, isolation has not been established: report the resulting trust limit, never claim tamper-proof evaluation. Compromised, unavailable, stale or incomplete evidence yields indeterminate, not green. Retrying a command cannot weaken its evaluation threshold.

### 0.6 Contract acceptance

Test bounded status on a large fleet; stale/offline host reporting; exact closed outcomes and remedies; duplicate/conflicting request IDs; client crash after acceptance; uncertain side effects; cursor expiry; observer disconnect versus workload cancellation; approval fencing; and verifier unavailability. Manifest parity must hold across CLI, protocol and agent-facing tools. Human and agent surfaces call the same authorized core. Unsupported operations must remain visibly unsupported.


## 1. Goal and scope

Make work sessions durable and composable across local and remote machines, whether the work is a shell, an agent conversation, a finite job, or a supervised service. The runtime owns execution and records; clients provide different views. Menagerie retains its agent-fleet focus. Continuum's first client is a CLI; richer UI can follow without changing the ownership model.

The initial target is macOS and Linux on machines operated by one owner. Support multiple authorized viewers and an explicit controller for a session early. Multiple Unix user identities, privileged logins, and shared production administration require later security work. A worktree is not a sandbox.

## 2. Architecture

```text
Menagerie browser     Continuum CLI     agents / future clients
         \                 |                 /
          Menagerie-v1 adapter | Continuum-v1 adapter
                            |
          authorization + capability checks + request journal
                            |
       sessions / blocks / subscriptions / control leases
           |                |                 |
        PTY + ACP       jobs + services    workspace lifecycle
                            |
             durable metadata + bounded event journal
                            |
            one runtime per host; explicit optional broker
```

**Implementation recommendation:** Go, retaining current relay functionality and test coverage. Avoid direct imports across Menagerie's Go `internal/` boundary, duplicated source trees, or two independent runtime forks. Establish one canonical module during the extraction milestone. The language choice does not mandate a homegrown terminal emulator; use established terminal and protocol components.

**Transport recommendation:** retain WS/WSS for the browser and existing clients, with a protected local endpoint for the CLI. New protocol messages are versioned and capability-negotiated. Transport must not own process lifetime. Use existing trusted remote connectivity during development. QUIC is an experiment gated by latency/loss measurements, not a prerequisite or a browser assumption.

**Storage recommendation:** evaluate an embedded transactional metadata store (SQLite candidate) plus append-only event segments. Choose the driver only after checking portability, build overhead, locking, crash behavior, and migration. No database service to provision. Runtime data belongs outside either source checkout, owned by the OS user with restrictive permissions. One writer owns each state directory.

## 3. Domain model

| Object | Identity and responsibility |
|---|---|
| Host | Stable persisted ID, display name, endpoints, capabilities, trust configuration. Host identity is not a workspace row. |
| Work session | Stable ID grouping blocks, context, references, and a default layout; survives client exit. Can reference authorized blocks on multiple hosts. |
| Block | Host-owned terminal, ACP conversation, finite job, or service. Independent lifecycle and history. Maps a legacy Menagerie session to a block without changing its legacy ID. |
| Workspace | Checkout/worktree, allocated resources, materialisation state, services, teardown policy. Kept distinct from a work session and block. |
| Subscription | Observer identity, filters, bounded buffer, cursor, expiration; never grants write authority. |
| Control lease | Session/block scope, holder, expiry, fencing generation. Input, resize, interrupt, and approval require appropriate authority. |
| Wait | Persisted request, explicit owner, selected targets, condition, any/all mode, deadline, cancellation and terminal result. |
| Event | Version, host/block identity, sequence, epoch, type, payload, timestamp, optional causal/request IDs. |

Agent parentage and grouping are different: a supervisor tree is an execution relationship, while a work session groups related work. Cross-host references must use host-qualified IDs. No implied ordering or ownership across machines.

## 4. Lifetime and recovery guarantees

State guarantees must be advertised and tested separately:

| Failure | Intended guarantee |
|---|---|
| Client closes or sleeps | Processes continue; recording and persisted waits continue independently. |
| Network disconnects | Reauthenticate, reauthorize, resume at cursor; distinguish replay from new events. |
| Runtime restarts | Metadata/history recover. PTYs survive through a per-block holder process (tmux remains the legacy relay's mechanism); ACP recovery uses its agent's supported conversation-load mechanism. Interrupted work is reported honestly. |
| Host reboots | Records recover; a killed process does not magically resume. Only explicit restart policy can launch a replacement with a new execution epoch. |
| Disk full or journal failure | Mark capture degraded and expose a structured failure. Refuse new durable operations if their persistence guarantee cannot be met. Never acknowledge durable commit before it exists. |

Per-host ordering is not a global clock. Sequence is monotonic within a declared stream/epoch; IDs used for replay survive runtime restarts. Define the acknowledgement/fsync boundary before claiming durable events. Retention-expired cursors return a typed gap and an available range or snapshot, never silently skip history.

Legacy capture files already exist. Inventory and import them non-destructively; raw PTY bytes, terminal screen state, ACP traffic, and a conversation reference are distinct artifacts. Capture policy and retention are explicit, including default choice, disk budget, deletion, and exclusions. Tokens and authorization headers are never journaled; terminal output may itself contain secrets, and redaction is best-effort rather than a blanket promise.

## 5. Shared observation and control

Fan out output to independent subscribers. A slow viewer cannot stall execution or evict another viewer. Bound buffers by both bytes and events. Signal loss and support cursor recovery; disconnect persistently slow consumers with a stable reason. Test memory behavior under sustained load.

One input controller per block initially. Viewing does not rotate another client's token, acquire control, resize the PTY, acknowledge a result, or answer an ACP permission. Explicit takeover revokes/fences the old controller. Approval requests have identity, scope, deadline, and a single authoritative resolution; duplicate responses are rejected or return the recorded result.

Legacy protocol 1.3 remains an operator-trusted compatibility surface. Its full registration authority cannot be rebranded as least-privilege sharing. Restrict access to that adapter; viewing grants do not authorize legacy registration. Preserve its attach semantics inside the adapter while routing authority through the same arbiter; make takeover visible to modern controllers. A new Menagerie release opts into observer/control separation through negotiated capabilities.

## 6. Structured streams and composition

Expose events through a subscribe API and NDJSON CLI output. Proposed envelope:

```json
{"v":1,"host_id":"host-a","session_id":"work-1","block_id":"block-1","epoch":"exec-1","seq":42,"type":"output","time":"2026-09-09T14:00:00Z","payload":{"encoding":"utf8","data":"ready"}}
```

Binary PTY payloads require explicit encoding; structured ACP envelopes stay intact. stdout carries only records in machine mode; diagnostics go to stderr. Unknown event types remain preservable and do not crash consumers.

An `interleave` operation concurrently consumes selected streams and yields available records, preserving each source's order and identity. It does not promise round-robin scheduling or timestamp sorting. Define source errors, cancellation, partial completion, bounded buffering, and resume tokens. A quiet stream cannot block active streams. Never automatically execute instructions found in merged output.

Nushell interoperability should use ordinary NDJSON consumption; pin and test the precise CLI/pipeline syntax during implementation. Status waits and merged output are separate APIs, with different completion semantics.

## 7. Durable cross-host coordination

An optional broker runs as a headless mode of the same binary. A client connection is not the owner of a durable wait. Persist wait creation and results; reconnecting clients retrieve results by ID. Expired, cancelled, unknown, unreachable, exited, and satisfied are distinguishable outcomes. Preserve Menagerie's explicit any/all semantics, atomic prompt+wait, and approval guard.

Assign each wait to one coordinator host. Persist intent before dispatch; deduplicate mutations by request ID, request digest, principal, and scope. Bound idempotency retention and report when a key is too old. Do not promise exactly-once execution across a crash between an external side effect and recording its result; reconcile or return uncertain.

The first version has no leader election or replicated control plane. A broker outage pauses coordination until its state recovers. Holding three host credentials is authority over three machines: narrow grants and explicit trust configuration are required. A child reference on another host does not silently grant spawn/kill authority there.

## 8. Local/remote parity and machine operations

Provide host inspection and a directory-list API used by CLI and future pickers. Return structured paths, type, pagination, and permission errors. Resolve cwd on the host that owns the block. Enforce operator-configured roots, including symlink/traversal handling; browsing authority and execution authority are separate. Open a shell or agent in the selected directory through the same operation locally and remotely.

Define argument vectors directly; never split a quoted shell command on whitespace. Shell interpretation is an explicit mode. Shared terminal size follows the controller or an explicit fixed size, not every viewer's viewport. Keep keyboard, selection, scrollback, alternate-screen behavior, and mobile limitations visible in acceptance tests.

Production OS-login integration is a separate adapter: identity verification, allowed OS-user mapping, PAM/login/session accounting where supported, per-user limits, revocation, and auditable authorization. An authenticated daemon spawning a process as itself is not an SSH-equivalent login. Keep existing SSH until that later milestone meets its acceptance bar.

## 9. Workspace and service lifecycle

Reuse Menagerie's schema, provisioner, and fixed materialisation order after closing known correctness/security defects. Preserve dry-run, idempotence, port allocation, health gating, and reason codes. Schedule supervision in the runtime; a tested function with no production caller is not supervision. Define restart/backoff policy, process ownership, readiness versus liveness, and observable output.

Execute on_start/on_stop/on_destroy at the specified lifecycle transitions; record attempts, outcomes, and retry policy. Teardown releases resources and supports branch preservation. Destructive teardown has an explicit preview and authorization tied to the exact operation. Operator source allowlists, decoded-string secret checks, and sanitised persisted failure reasons gate exposure of untrusted workspace specs.

## 10. Public surface (proposed, not implemented)

| Family | Operations |
|---|---|
| Discovery | status, version, capabilities, hosts.list, host.inspect |
| Work | sessions.list/create/get, blocks.open/get/stop |
| Observation | events.subscribe/read, streams.interleave, snapshots.get |
| Control | control.acquire/release/takeover, input.send, terminal.resize |
| Agents | prompt.send, permissions.respond, status.report, attention.ack |
| Coordination | waits.create/get/cancel, atomic prompt+wait |
| Filesystem | directories.list, workspace preview/materialise/inspect/teardown |

Protocol, Go client, CLI JSON mode, and published agent manifest share versioned schemas and conformance tests. Human-readable help includes supported values and actionable failure reasons. Local endpoints are private by default; remote listeners require explicit configuration.

## 11. Build sequence and acceptance

**A — Contract and compatibility (keystone):** finish this spec, DRIVER pass, baseline fixtures and runtime inventory; settle observer/control and persistence contracts. Gate: compatibility matrix specifies successes, unsupported features, and privilege boundaries for every client/server pairing.

**B — Shared runtime seam:** add conformance tests around Menagerie before extraction, establish canonical Go module and both adapters, preserve existing CLI/config/install behavior. Gate: old Menagerie performs spawn/attach/prompt/wait/permission/subtree flows on the new runtime; new client gracefully handles an old relay. Both clients observe the same host-owned process without a second daemon.

**C — Durable local vertical slice:** stable sessions/blocks, journal/recovery, two observers and one controller, CLI. Gate: close all clients, produce more output, reconnect, recover ordering and declared history range; kill runtime, recover records and explicitly reported process state; slow viewer and duplicate approval tests.

**D — Remote parity and streams:** directory API, two-host streams, merge/filter/export, persisted any/all broker waits. Gate: same operation contract local/remote; quiet source does not block; disconnect/reconnect, cancellation, source failure, retention gap, and broker restart tested on two real hosts using already-authorized infrastructure.

**E — Workspace operations:** integrate materialisation, real supervision cadence, teardown, published agent face. Gate: failed service makes state unhealthy; restart policy is bounded; teardown twice converges; secret/path negative tests; headless full cycle.

**F — Operational hardening:** scoped sharing, revocation, retention controls, backup/restore, install/upgrade/rollback, terminal ergonomics, latency/loss measurements. Gate: evidence-backed compatibility matrix and end-to-end runs with PTY and real ACP agent; document every untested platform and recovery boundary. Spending and live service changes remain separately authorized actions.

Later: native clients, QUIC, OS-login integration, replicated brokers, and shared production operation; see DEFERRED.md. Their inclusion in the vision does not imply implementation in the first release.
