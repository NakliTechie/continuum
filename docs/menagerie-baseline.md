# Menagerie implementation baseline

> **Lifecycle:** living — inspected 2026-09-09; evidence is scoped to the revision below.

Repository: https://github.com/NakliTechie/menagerie
Revision: `837a3ee5fcf92c71a84dfefce062e6a9cf9a2457`.
Local source: a sibling Menagerie checkout (`../menagerie`).
Go module: `github.com/NakliTechie/menagerie/relay-go`, Go `1.26.2`.
Relay declaration: `0.6.0`; protocol: `1.3`. New workspace code on main does not establish inclusion in the released 0.6.0 artifact.

## Reuse inventory

| Source path at pinned revision | Observed behavior | Migration implication |
|---|---|---|
| relay-go/internal/server/server.go | Server owns processes independently of WS connection; one `sub *conn`; attach reissues token; tmux adoption; ACP ring tail | Preserve process ownership; separate subscriptions from control; maintain legacy adapter |
| relay-go/internal/pty/session.go | Append-only `.pty` capture; 256 KiB in-memory tail; best-effort capture writes | Recording exists; add failure visibility, retention, indexed retrieval and recovery |
| relay-go/internal/acp/session.go | `.acp.jsonl` traffic capture; agent conversation references and load support | Preserve envelopes and references; distinguish traffic history from conversation/process restoration |
| relay-go/internal/server/wait.go | In-memory, connection-associated waiters; atomic condition registration and timeout | Reuse race semantics; persistence/result retrieval must be added, not assumed |
| index.html | Browser any/all wait broker, agent tiles, diff review, tree UX, underscore test seam | Keep product UX; migrate durable broker outside tab; publish real agent contract |
| relay-go/fleet + workspace + materialise | Schema/validators, worktree/port provisioning, fixed-order materialisation, dry-run; Supervise function | Reuse after known defects fixed; wire supervision cadence and teardown |
| protocol/types.ts + fixtures | Canonical legacy shapes; 29 fixtures, 23 frame types | Pin and test compatibility before extraction |

Code wins over stale prose: plan/pending.md previously said no relay recording; PTY and ACP code both record locally. The missing guarantee is complete retrieval/recovery and policy. Old protocol/security prose about no reconnect also predates `sessions` + `attach`.

## Gaps carried into design

Single-subscriber takeover; browser-owned broker; bounded replay rather than full cursor recovery; no host directory-list operation; no OS-login/user-mapping layer; no measured poor-network guarantee; no merged structured stream API; unfinished workspace launch/teardown/supervision wiring; workspace-scoped host pairing; unpublished browser command manifest; quoted custom argv split incorrectly; small tiles can break TUIs.

Known workspace defects recorded in Menagerie's pending file include unconstrained copy/template source paths, raw-byte rather than decoded-value secret lint, unsanitised persisted probe errors, Go/JS whitespace disagreements, prose-based supervision verdicts, and destination interpolation. These must not be copied into a more widely accessible API. Existing owner decisions and security gates are not overridden by this scaffold.

## Verification scope

In the immediately preceding comparison, `go test ./...` passed after permitting local test sockets; `node protocol/validate-fixtures.mjs` passed 29 fixtures with 23/23 frame types. The real ACP integration suite uses the `acpintegration` build tag and was not run. No new browser end-to-end run or latency benchmark was performed.

The local two-relay report records an earlier EC2 any/all wait exercise. Treat that as prior evidence, not a fresh deployment or proof of restart durability. No external infrastructure was created for this scaffold.

## Machine-readable pin

[compatibility-baseline.json](compatibility-baseline.json) records the pinned revision and SHA-256 hashes of the runtime, protocol definitions, behavioral test sources, and fixture files. Generated from immutable Git objects; JSONL fixture syntax was checked. These hashes detect drift; they do not replace behavioral conformance tests.
