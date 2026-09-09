# One runtime, two products

> **Lifecycle:** living — runtimev1 imports the runtime with subtree history; installed-service cutover remains pending.

The proposed answer to the owner's shared-binary question is yes: upgrade and extract the Go relay once, exposing two client contracts over one runtime. Do not fork the process manager into two repositories. Protocol reuse is possible now; safe simultaneous viewing and stronger durability require changes.

## Ownership by phase

1. **Baseline:** Menagerie/relay-go remains canonical. Continuum contains the new specification, conformance harness, and client work. No installed service changes.
2. **Seam:** introduce separable process/session/history/auth packages in the canonical source behind a protocol adapter. First preserve behavior; new guarantees get independent tests. Menagerie's Go internal packages cannot simply be imported from an unrelated module.
3. **Extraction:** transfer runtime history into Continuum through a reviewed history-preserving extraction, preserving AGPL notices and contribution history. Use a release-tagged module and generated protocol artifacts. Menagerie retains the browser, its product docs, fixtures, and client integration tests. Keep only one maintained implementation.
4. **Packaging:** publish a Go executable with a `serve` mode and CLI commands. Retain `menagerie-relay` compatibility via an invocation alias/launcher for the same artifact. Keep the old `serve/service/token/materialise` contract until a versioned deprecation. Exact release names are a packaging decision, not a reason for two daemons.
5. **Cutover:** test the old app against the new binary, adopt configuration through an explicit migration with backup/dry-run, and preserve or map session IDs. Never auto-start a second writer on the old state directory or port. Never silently restart the user's running agents.

During transition, two packaging names may exist; they must select the same runtime implementation and instance. New features are capability-gated. The existing install remains untouched until the migration is validated and an in-scope upgrade is authorized.

## Compatibility matrix to implement

| Client | Current relay 0.6.0 / protocol 1.3 | Shared runtime with both adapters |
|---|---|---|
| Existing Menagerie | Existing behavior, including single-subscriber attach | Legacy operations preserved, same session IDs; trusted legacy control access; declared takeover behavior |
| Updated Menagerie | Disable unsupported features, retain legacy workflow | Observer/control split, replay cursors, host directory picker, durable waits where advertised |
| Continuum CLI/agent | Legacy subset only; never claim new durability/sharing | Full negotiated Continuum contract |

Compatibility means behavior, error semantics, and authority as well as JSON shape. Legacy `attach` rotates a token and replaces the subscriber today. It cannot acquire read-only semantics silently. Keep it behind the operator-trusted adapter; do not expose it to recipients of narrow modern sharing grants. Both adapters share the control arbiter and audit trail.

## Required migration tests

- Pin the baseline commit and all 29 protocol fixtures (23 frame types); cover behavior fixtures cannot establish with a live test relay.
- Run spawn, reconnect/attach, ACP prompt/permission, atomic wait, status/seen, resume-conversation, and subtree kill for both generations.
- Verify two viewers plus one controller, controller fencing, legacy takeover notification, approval races, and slow-consumer independence.
- Import metadata/capture files without overwriting originals. Test disk-full, partial write, schema-version mismatch, and restore.
- Test mixed-version upgrade and rollback. If a schema cannot be read by the old binary, require backup restore or forward migration; never claim blind downgrade safety.
- Verify a single service, port owner, process registry, and store after install/update. Verify no agents terminate during operations advertised as non-disruptive.

## Scope split

Runtime: execution, durable events, auth/control, coordination, directories, workspace/service lifecycle.

Menagerie: fleet grid/tree, structured diff review, attention UX, host connection UI, browser storage integration, capability-aware runtime client.

Continuum: general work sessions, CLI/agent contract, runtime implementation after extraction, transport/recovery experiments, future terminal/workspace clients.

Neither project needs to own a hosted control plane. A static web client can use existing Cloudflare hosting; the host daemon itself needs real OS process facilities and is not a Cloudflare Worker workload.

## Implemented branch state

The local alpha now lives in the root Continuum Go module. The imported subtree merge retains Menagerie revision 837a3ee history and AGPL attribution. `legacy.Main` is the public compatibility launcher; `cmd/menagerie-relay` delegates to it. `cmd/continuum` exposes the modern CLI and legacy invocation. Both modern and legacy HTTP adapters share one `server.Server`. New recording uses its bbolt journal; legacy invocation retains the old file behavior. No installed configuration is automatically adopted. See README.md for the tested local flow and remaining migration limits.
