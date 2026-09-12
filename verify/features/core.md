# Core

Exists: bbolt metadata/history, bounded replay with gaps, intent deduplication, observer/operator separation, expiring control and legacy takeover fencing, PTY process groups and bounded input. ACP startup cancellation, final-frame drain, and shutdown admission have fake-process regressions. Inherited ACP and workspace commands retain their regression suites.

Reach: HTTP `POST /v1` over the state directory's `v1.sock` (or the same operations through the CLI); the legacy WebSocket uses `/` on the loopback TCP port.

Verify: `python3 scripts/verify.py verify core`.

Watch: fsync commits do not guarantee an external effect executed exactly once. Unclean daemon epochs mark old history incomplete. Workloads share the daemon's OS identity; process groups are lifecycle management, not a sandbox.
