# CLI

Exists: foreground serve, status/detail/paging, open, events/follow/text, acquire/takeover/release, input, resize and stop. First use needs no model credential.

Reach: `continuum help`; start `continuum serve`, then open a command in another terminal.

Verify: `python3 scripts/verify.py verify cli` builds the real binary, uses disposable state and checks two viewers, no-client output, duplicate/conflicting launches, auth, process control and crash recovery.

Watch: readiness must probe the API, not trust a stale endpoint file after a crash. Input is UTF-8, at most 64 KiB. Leases last 60 seconds. `--text` decodes untrusted terminal bytes and is opt-in. Following output never owns the process lifetime. Raw terminal attach/emulation is not yet a CLI feature.
