# Core

Exists: bbolt metadata/history, per-block PTY recording policy (`none`, visible screen, N lines, full), purge/exited-block retirement/offline atomic compaction, bounded replay with gaps, intent deduplication, observer/operator separation, expiring control and legacy takeover fencing, PTY process groups and bounded input. ACP startup cancellation, final-frame drain, and shutdown admission have fake-process regressions. Inherited ACP and workspace commands retain their regression suites.

Reach: HTTP `POST /v1` over the state directory's `v1.sock` (or the same operations through the CLI); the legacy WebSocket uses `/` on the loopback TCP port.

Verify: `python3 scripts/verify.py verify core`.

Watch: fsync commits do not guarantee an external effect executed exactly once. Deliberately discarded PTY payload still advances the durable holder-resume offset. Unclean daemon epochs mark old history incomplete. Compaction is offline and lock-fenced. Workloads share the daemon's OS identity; process groups are lifecycle management, not a sandbox.
