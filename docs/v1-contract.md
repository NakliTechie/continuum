# The `/v1` contract

Continuum's modern API is a request/response protocol over a private Unix
socket (`<state>/v1.sock`). This document is the stability contract: what a
client may depend on, and how the contract changes. It is versioned — a client
negotiates with the `version` operation before depending on anything else.

## Versioning

Two independent version numbers:

- **`contract_version`** (currently `1.0`) — the operation and capability
  contract described here. Within major `1`, the stable surface below keeps its
  meaning and grows only additively (new operations, new optional request
  fields, new response fields). A change that removes or repurposes any stable
  element bumps the major **and** the socket name (`v1.sock` → `v2.sock`), so an
  older client connecting to `v1.sock` meets no socket rather than a contract it
  cannot speak — never a silent behaviour change.
- **`schema_version`** (currently `1`) — the response envelope shape
  (`class`, `code`, `durability`, `next_action`, `result`). Bumped only if that
  envelope changes incompatibly.

Discover both, plus the live capability set, with the `version` operation
(`continuum contract` from the CLI). It requires only a valid bearer token and
never mutates state.

```json
{
  "contract": "continuum/v1",
  "contract_version": "1.0",
  "schema_version": 1,
  "protocol": "continuum.local-alpha.1",
  "server": "0.1.0-alpha.2-dev",
  "operations": ["version","status","events","screen","open","acquire","renew","release","takeover","input","resize","stop","purge","retire","directories"],
  "capabilities": {
    "stable": ["pty","observers","control_lease","event_replay","control_renewal","legacy_1.3"],
    "experimental": ["terminal_screen_v1","terminal_input_base64","recording_policy_v1","directories_v1"]
  },
  "process_restart_survival": true
}
```

## Compatibility rules

For a client:

- **Ignore unknown response fields.** New fields are additive; a client that
  rejects them breaks itself on the next additive release.
- **Gate on capabilities, not on `server`.** Use a feature only when its
  capability is advertised. `stable` capabilities keep their meaning within
  contract major 1; `experimental` capabilities may change or be withdrawn — pin
  the `server` build if you depend on one.
- **Send only known request fields.** The server rejects unknown request fields
  (`invalid_request`), so a newer client's new field is refused cleanly by an
  older daemon rather than silently ignored.
- **Treat an absent socket as "no compatible daemon".** A client built for a
  future major finds no `v1.sock` and reports the daemon as not running, rather
  than mis-driving it.

For the server, within contract major 1: operations are never removed or given
new required request fields; response fields are never removed or repurposed;
error `class`/`code` values keep their meaning; `next_action.kind`/`operation`
stay drawn from the existing closed vocabulary. Any of those requires a major
bump.

## Stable surface (contract 1.0)

Operations — reads (`version`, `status`, `events`, `screen`) need only a token;
mutations (`open`, `acquire`, `renew`, `release`, `takeover`, `input`,
`resize`, `stop`, `purge`, `retire`) require the operator token and a stable `request_id` that the
daemon's idempotency ledger reconciles. Every response carries
`class`/`code`/`durability`/`next_action`; `Exit`-code mapping for each class is
in `api/types.go`.

Stable capabilities: `pty` (raw PTY blocks), `observers` (read-only followers),
`control_lease` + `control_renewal` (the 60-second explicit, revocable control
lease), `event_replay` (bounded journal reads with typed gaps), `legacy_1.3`
(the unchanged Menagerie WebSocket adapter on its loopback port).

## Experimental surface

`terminal_screen_v1` (server-owned screens), `terminal_input_base64`
(byte-exact input for them), and `recording_policy_v1` are experimental: shape
and limits may change. Recording policy adds optional `recording` and
`recording_lines` fields to `open`, the corresponding block status fields, and
the `purge`/`retire` mutations. `visible` requires `screen-v1`; `lines` requires
`recording_lines` in `1..10000`. An omitted policy means `full`, preserving old
clients. The offline CLI-only `compact` command is not a `/v1` operation. See
[terminal-screen-api.md](terminal-screen-api.md) and
[local-alpha.md](local-alpha.md).

`directories_v1` adds an operator-only read, `directories`, with optional `path`,
`limit` and `cursor`. No path lists configured roots; a path lists entries with
bounded, change-detecting pagination. Directory observations have `volatile`
durability. The capability advertises support even when browsing is disabled;
operators opt in through `serve --browse-root`. The SSH stdio bridge carries
these same envelopes without exposing a new API listener or sending daemon bearer tokens
to the client. See [remote-directories.md](remote-directories.md) for authority,
confinement, limits, error classes and transport behavior.

## Not covered by this contract

The legacy Menagerie WebSocket wire protocol (its own `protocol/protocol.md`
in Menagerie), the on-disk journal format, and CLI human-readable output. The
`protocol` string (`continuum.local-alpha.1`) names the runtime protocol
generation and is informational; negotiate on `contract_version` and
capabilities, not on it.
