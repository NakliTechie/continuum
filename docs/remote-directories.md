# Directory and SSH contract (Batch D1)

> Lifecycle: living. Implementation and evidence are recorded below; this is not
> the cross-host composition/coordinator contract from the rest of Batch D.

The modern API stays on its private Unix socket. A remote client uses existing
key-authenticated SSH (`--host USER@HOST`, mandatory remote `--state ABSOLUTE_DIR`)
to run `continuum rpc` on the owning host. It sends one bounded JSON request on
stdin and receives the normal envelope on stdout. Credentials never cross SSH:
the bridge reads that host's private operator/observer credential. SSH is
non-interactive and requires an already trusted host key. There is no listener,
pairing, token distribution, service installation or automatic retry.

`--remote-binary` selects an installed executable (default `continuum`). All
remote shell arguments are quoted; the payload is never shell text. Remote
mutations retain request IDs and indeterminate outcomes after transport loss.
Remote `open` requires an explicit absolute `--cwd`, interpreted only there.
Saved remote control leases live in a separate private local cache keyed by
host, executable and remote state, not in the remote state's local pathname.
`serve`, `service`, offline `compact`, and nested `rpc` cannot use `--host`.
Each call starts SSH; latency/throughput optimization is deferred until the
multi-source transport work. Interactive attach may be unsuitable on high-latency
links; the same screen/input/control contract still applies.

Directory browsing is disabled unless the operator supplies one or more
`serve --browse-root ABSOLUTE_DIR` flags. Up to 32 roots are opened at startup
as confined directory handles, not a string-prefix sandbox. The returned roots
are canonical absolute paths. The `directories_v1` experimental capability
advertises support; `directories` without a path lists the configured roots,
and `directories --path ABSOLUTE_DIR` lists entries within one of those roots.
Existing observer credentials cannot browse: directory visibility is additional
operator authority, not implicit in observing block output. Browsing does not
grant execution; existing operator `open --cwd` remains separate and unrestricted
by browse roots. This is not a multi-user execution sandbox.

Pages contain host identity, root, path, entry name/path/type and an opaque next
cursor. Defaults: 100 entries, maximum 200; sorted by name. A scan is bounded at
10,000 entries and 2 MiB of names; larger directories report resource exhaustion
without claiming a complete listing. Cursors bind the path, root, directory
identity and complete names/types digest. Changed listings return a conflict and
require restarting pagination. Cursors are observations, not durable snapshots;
they do not freeze or attest file contents. Traversal, symlinks escaping roots,
permission failures, missing paths and non-directories fail explicitly. Rooted
handles prevent symlink-swap escapes; absolute symlinks are refused even if they
point back into a root. Filesystem mount boundaries are not sandbox boundaries.
No file contents or symlink targets are returned. Non-UTF-8 names fail explicitly
instead of returning lossy paths. Service persistence of browse-root flags is
deferred; for now configure them on foreground daemons only.

## Verification scope

The isolated gate must cover confinement, pagination/cursor changes, permissions,
observer denial, opt-in startup, remote argument preservation, remote cwd,
saved-lease separation, capability mismatch, malformed/oversized responses,
cancellation and ambiguous mutations. The CLI journey uses two isolated daemon
states with a test SSH process that invokes the real bridge. That is transport
contract evidence, not real-network evidence. A separate real SSH probe/exercise
must succeed before claiming the two-host network gate.
