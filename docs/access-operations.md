# Scoped access and operations — F

Root operator and observer credentials retain their current local-alpha
semantics. New grants are opt-in, bounded metadata in the same private journal.
Only a root operator may create or revoke grants and change a block's sharing
policy. Raw grant tokens are generated server-side, returned once, stored only
as hashes and never recoverable. A revoked grant fails immediately. Grant
classes are observer, controller and moderator; all are restricted to explicit
block IDs. Moderator may acquire/take over control for scoped blocks but may
not create credentials, change sharing, retire or purge. Observer never
mutates. Controller may request a control lease only on an allowed block.

Sharing is per block: private, observers, or controllers. Private admits only
root authority; observers admits scoped read grants, and controllers also
admits scoped controller/moderator grants. A block is private by default.
Denied access does not disclose whether a private block exists. Auth decisions
and administrative changes are journaled in a bounded audit trail containing
token fingerprint and operation, never raw secrets or prompts. The new
`/observer` loopback page accepts only a read-only credential; it cannot use
the root operator token. The legacy Menagerie socket stays its separately
gated operator adapter until the client CU track moves, so legacy attach and
control semantics are not silently changed.

Pairing uses explicit SSH trust plus a pinned daemon host identity and existing
per-host observer credentials. It creates no listener and copies no token.
Offline backup includes a checksum manifest and private state snapshot;
restore verifies both while the daemon is stopped, refuses an existing state
unless explicitly replaced, and never performs an implicit schema migration.
