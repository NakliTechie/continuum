# Legacy relay conformance — first implementation slice

> **Lifecycle:** locked for the scope of this slice, 2026-09-09. This captures existing behavior; it does not settle the broader runtime/storage/sharing design.

## Deliverable

A black-box Go integration suite launches the pinned Menagerie relay in a disposable environment, drives its public WebSocket protocol, and asserts behaviors that a future shared runtime must preserve. No imports from Menagerie's internal packages and no persistent copy/fork of its runtime source.

The baseline runner uses the existing local Menagerie Git object database, extracts the exact pinned revision into a temporary directory, verifies the 42 inventory hashes, and builds the relay plus its fake ACP test agent. Test children receive an isolated home, minimal environment, loopback-only listener, tmux disabled, and no real provider agents. It does not read the installed relay config or contact the installed service. Cleanup terminates the disposable process group and removes temporary files.

## Cases in this slice

| Case | Required observation |
|---|---|
| Authentication | Origin rejection, unregistered request rejection, wrong registration-token rejection; correct registration reaches session inventory. |
| PTY detach/attach | Output produced with no attached client is replayed after reconnect; the same process remains controllable. |
| Legacy takeover | A second attach issues a new token; input/control using the old token is rejected. This is compatibility behavior, not a multiple-observer feature. |
| Atomic prompt + wait | A fake ACP turn resolves its same-frame wait to done, with the correct ID and no timeout. |
| Attention | Another inventory read preserves done; explicit seen moves done to idle. |
| Approval guard | While the fake ACP agent asks permission, a task prompt is refused with session_blocked; explicit permission response completes the turn. |
| Subtree kill | Parent and child both exit after an authorized subtree stop. |

Legacy PTY input is raw UTF-8 in `input.data`; output is base64 in `output.data`. Preserve this directional asymmetry.

Frame waits have one overall deadline and a frame-count cap; unrelated output cannot extend a test forever. Failure messages avoid dumping authentication-bearing frames. A missing configured relay/fake-agent path fails the tagged suite rather than silently skipping.

## Boundaries

This suite covers loopback macOS/Linux execution with tmux off and a fake ACP process. It does not validate browser rendering, real provider behavior, tmux adoption across restart, full history recovery, operating-system login, multiple read-only viewers, WAN resilience, or the future adapter against every legacy frame. Add those cases as the corresponding slice arrives; this is not a complete migration gate.

The future runtime can be supplied through explicit executable paths; the documented baseline runner establishes the current reference. Never point the suite at a live daemon: it always launches its own child from the supplied executable.

## Run

On macOS or Linux with Go 1.26.8, Git, Python 3 and tar:

```sh
python3 scripts/check_legacy.py --source ../menagerie
```

The runner verifies the pinned Git objects, builds temporary executables and runs the tagged suite with the Go race detector on the test client. The relay executables use their normal build; this is not a relay race audit. The local Go module cache may need the declared dependencies on its first run. There are no model-provider calls.

For a future candidate runtime, build a compatible fake ACP helper and set absolute `CONTINUUM_TEST_RELAY` and `CONTINUUM_TEST_ACP` executable paths, then run `go test -race -tags=legacyintegration -count=1 -timeout=90s ./compat`. These variables name binaries, not a live endpoint. Tests always start their own isolated relay. The tagged suite is opt-in; a plain `go test ./...` is not conformance evidence.

## Observed validation — 2026-09-09

The baseline runner verified all 42 hashes at Menagerie revision `837a3ee5fcf92c71a84dfefce062e6a9cf9a2457`, built disposable relay/fake-agent executables and passed all six Go integration tests on macOS (test-client race detector enabled). This includes PTY output emitted while every WebSocket client was disconnected and replayed after reconnect to the same PID. Linux, real provider agents, browser compatibility and the future Continuum runtime were not exercised.

The initial run exposed a test-client encoding mistake: legacy input uses raw text, while output uses base64. Correcting the test client produced the passing run; no Menagerie implementation change was needed.

## Candidate runtime validation — 2026-09-09

All six unchanged black-box tests also passed against the Continuum-built candidate relay after the import and runtime corrections. `python3 scripts/verify.py verify legacy` now rebuilds that candidate in temporary storage. The upstream-baseline runner remains independent and pinned. A separate Chrome walk exercised the unchanged Menagerie app against the alpha daemon: add relay, spawn custom `/bin/cat`, send text and recover that output through the CLI. This does not establish every browser or mixed-version migration case.

## Registration rotation

A legacy server loaded from relay.toml reads current registration authority before registration and each subsequent command/output. `legacy token rotate` atomically replaces that private config. Old credentials fail on new connections; existing connections close before their next command or output. Idle connections need no polling timer. Rotation does not kill processes or adopt/restart services. Missing, malformed or empty credential configuration fails closed. Other config fields still require the existing restart workflow. Programmatically configured modern daemons keep their separate private-state credentials.

ACP argument configuration distinguishes omission from an explicit empty array. Omitted `acp_args` inherits a known agent’s suffix or defaults to `["acp"]`; `acp_args = []` starts the configured executable directly. The five dedicated ACP adapters use this empty suffix. Client-supplied spawn arguments are appended without modifying the configured slice.
