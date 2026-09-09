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

Frame waits have one overall deadline and a frame-count cap; unrelated output cannot extend a test forever. Failure messages avoid dumping authentication-bearing frames. A missing configured relay/fake-agent path fails the tagged suite rather than silently skipping.

## Boundaries

This suite covers loopback macOS/Linux execution with tmux off and a fake ACP process. It does not validate browser rendering, real provider behavior, tmux adoption across restart, full history recovery, operating-system login, multiple read-only viewers, WAN resilience, or the future adapter against every legacy frame. Add those cases as the corresponding slice arrives; this is not a complete migration gate.

The future runtime can be supplied through explicit executable paths; the documented baseline runner establishes the current reference. Never point the suite at a live daemon: it always launches its own child from the supplied executable.
