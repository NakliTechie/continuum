# Durable coordination — D3

> Lifecycle: living. This is the implementation contract for D3, distinct from
> client-owned `interleave` subscriptions. Experimental capability `waits_v1`.

One owning daemon coordinates each durable any/all wait in its private state.
Creating a wait commits its identity, caller, digest, targets and deadline before
acknowledgement. Clients may disconnect, use nonblocking `wait output`, or poll
with `wait attach`; cancelling an attach never cancels the durable wait. Explicit
`wait cancel` only cancels coordination, never sends input/stop to a target.

Targets name local blocks or explicitly configured peers. Peer configuration pins
the remote host ID, existing trusted SSH account, absolute state and executable.
Requests cannot supply arbitrary SSH destinations or executable paths. The D1
bridge keeps remote daemon tokens on their owning host. No automatic host-key
acceptance, listener, credential copying or pairing trust is inferred from output.

Conditions use exact lifecycle names (running, idle, done, needs_input, stalled,
rate_limited, exited, unknown). A committed lifecycle snapshot plus journal cursor
avoids the check/register gap. Later journal pages preserve transient transitions;
history loss fails explicitly instead of guessing from today's state. First
observed matching completions receive coordinator commit order; this is not a
global physical clock. Replays return the same winner and order after restart.
An unexpected exit is distinct from satisfaction. Unreachable members remain
pending with explicit transport state until reconnect/deadline, not fabricated
completion. Admission requires an initial atomic observation/cursor from every
source; a source unreachable before that boundary makes creation return
`unreachable` without committing a wait, because replay from cursor zero could
mistake an older transient event for new completion. Revoked access, changed
host identity and unsupported capability fail
closed. Interrupted targets are not successful results. Capture degradation
(an unclean daemon epoch or a recording loss) marks the member
`history: incomplete` and the wait keeps watching; only a later journaled,
contiguous transition can satisfy it, never today's state. A cursor outside
retained history (`history_gap`) still fails the source.

Waits are bounded: 64 pending, 1024 retained, eight members each, deadline at most
24 hours, one bounded event page per poll and at most eight concurrent peer reads.
Finished records remain until an explicit later retention operation; at the cap
creation fails rather than forgetting an idempotency key and repeating intent.
Same caller/request ID and digest returns the existing record; changed arguments
conflict. A failed commit never returns a durable success.

Modern ACP prompt/approval calls retain the existing arbiter and approval guard.
Atomic prompt+wait commits observation intent before dispatch, binds it to the
new turn, and never blindly redispatches an uncertain prompt after restart.
Permission replies name the actual pending request and selected option under a
current control lease; a wait or ordinary prompt is never an approval answer.
Existing legacy prompt/wait/permission behavior remains unchanged.

Verification includes restart between acceptance and observation, deterministic
any/all replay, quiet/disconnected members, deadlines/cancellation, duplicate and
conflicting IDs, gaps, wrong-host/access failures, transient lifecycle changes,
prompt-generation fencing, approval races and unchanged legacy gates. D4 adds real
two-host evidence; fake SSH tests do not substitute for that gate.
