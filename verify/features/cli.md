# CLI

Exists: foreground serve, status/detail/paging, open, events/follow/text, acquire/takeover/renew/release, input, resize and stop. First use needs no model credential.

Reach: `continuum help`; start `continuum serve`, then open a command in another terminal.

Verify: `python3 scripts/verify.py verify cli` builds the real binary, uses disposable state and checks two viewers, no-client output, duplicate/conflicting launches, observer refusal of `open` (auth beyond that lives in `internal/server` tests), `acquire`/`resize`/`stop` accepted with a saved lease (their effect on the process is asserted in `internal/server` and the terminal journey, not here), sanitized versus raw replay, and crash recovery. `renew` and `release` are exercised by the terminal journey; `takeover` by `internal/server` tests and the `internal/cli` attach `--takeover` test, not by any journey script.

Watch: the API is a Unix socket in the state directory; a socket left by a crash is removed by the next `serve`, and a client dialing it gets `daemon_unreachable`, never a foreign listener. Input is UTF-8, at most 64 KiB. Leases last 60 seconds. `--text` decodes untrusted terminal bytes to printable text and colour only; `--raw` is the byte-exact, trusted-output opt-in. Following output never owns the process lifetime. Interactive server-frame attachment is covered separately in [Terminal](terminal.md).
