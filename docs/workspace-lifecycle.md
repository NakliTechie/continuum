# Managed workspace lifecycle — E

The existing `materialise` command remains a one-shot compatibility path. A
managed workspace opts into supervision with a private record containing the
exact source-byte spec hash, repository root and workspace name. Repository-authored
shell (commands, service starts, escape hatch, hooks and teardown) requires an
explicit `--trust` acknowledgement of that exact hash before first execution.
Changing the spec invalidates the acknowledgement; a passive health check
never executes changed shell. No workspace spec grants host access.

`continuum workspace run --trust HASH` persists the record before executing the
graph; the owning daemon supervises it on a ten-second cadence. A daemon restart reopens the record
but does not rerun provisioning shell automatically. Operators explicitly
resume after uncertain attempts. Each service has a persisted consecutive
restart budget and bounded exponential backoff; budget exhaustion is visible as
unhealthy. A recovered service resets its consecutive budget only after a
successful health check. Managed HTTP health requires 2xx/3xx: a listening
socket or 404 alone never gates ready. The legacy one-shot probe retains its
older below-500 behavior for compatibility.

Managed supervised services require an `on_stop` hook. `continuum workspace stop`
runs it even if materialisation failed before `on_start`, then verifies each declared
service port no longer answers before recording stopped. A returned stop command is
not proof of process ownership or cleanup beyond those endpoints; a still-answering
service blocks destroy for explicit reconciliation. `continuum workspace destroy --confirm NAME` runs stop, teardown commands and `on_destroy`
before removing only its verified git worktree. Destruction requires explicit
confirmation and refuses dirty worktrees; branch removal follows the spec's
`keep_branch` selector. Failed hooks or commands leave records and the
worktree intact for operator reconciliation. Hooks are not implicit retries.

The managed path uses the existing workspace provisioner and materialisation
engine; it does not create a second relay or copy process ownership. Real
headless tests use a temporary home/repository/ports and no provider credits.
