# Running Continuum as a service

`continuum serve` runs in the foreground. To keep the daemon running across
logins and restarts, install it as a per-user service:

```sh
continuum service install --state /abs/state --listen 127.0.0.1:58750
```

- **macOS** writes a launchd agent labelled `com.naklitechie.continuum` under
  `~/Library/LaunchAgents` and bootstraps it (`RunAtLoad`, `KeepAlive`).
- **Linux** writes a systemd `--user` unit `continuum.service` under
  `~/.config/systemd/user` and enables it (`Restart=always`). On a headless box,
  `loginctl enable-linger "$USER"` keeps it running after logout.

The `--state` directory is resolved to an absolute path and baked into the unit,
along with the credentials it needs (created at install time, so the first boot
has nothing to generate). A **fixed** `--listen 127.0.0.1:PORT` is required: a
background daemon on a random port could never be reached by Menagerie after a
restart. `--origin URL` is carried through for a hosted Menagerie origin.

Preview without installing anything by setting `CONTINUUM_SERVICE_DRYRUN=1`: the
unit file is written and its path printed, but nothing is loaded.

On Linux the unit sets `KillMode=process`, so stopping or restarting the service
signals only the `continuum serve` process. It also sets `KillSignal=SIGKILL`:
the foreground daemon's SIGTERM path intentionally stops its work, so a service
replacement must terminate only the daemon before its holders can be re-adopted.
On macOS, reinstall/uninstall performs that same daemon-only kill before
`bootout`, and the agent sets `AbandonProcessGroup` so launchd does not reap the
holder sessions during unload. PTY blocks stay running with the same PID; their
next daemon reports the unclean handoff conservatively as incomplete history.
Structured ACP blocks still do not survive a daemon replacement.

Running `service install` again updates the unit and reloads the service. Linux
uses `daemon-reload`, `enable`, then an unconditional `restart`; this matters
because `enable --now` alone does not replace an already-running binary. The
macOS path unloads the old job only after its daemon-only stop. Failure to make
that safe handoff aborts instead of falling back to a SIGTERM that would stop
the blocks.

This service is **separate from** the legacy `menagerie-relay` service: it uses
its own label/unit, so installing, checking, or removing one never touches the
other, and the two can run side by side. Replacing an installed `menagerie-relay`
with the Continuum daemon (a cutover) is a deliberate, separate operation, not
part of `service install`.

```sh
continuum service status      # is it loaded / active?
continuum service uninstall   # remove the unit; state and running processes stay
```

Paths containing newlines are refused (they could inject unit directives), and
every argument is XML-escaped (launchd) or systemd-quoted (`%`, `$`, quotes,
backslash) before it reaches the unit file. Never expose this alpha on a public
listener.

## Cutover from menagerie-relay

To make the always-on service the modern Continuum daemon while keeping a client
that already trusts your `menagerie-relay` connected with no change:

```sh
continuum service cutover --state /abs/state --adopt-relay ~/.menagerie/relay.toml
```

`--adopt-relay` makes the daemon answer as the relay it replaces: the daemon
listens on the relay's port, seeds its operator credential with the relay's
registration token (so a client's stored token keeps working), and adopts the
relay's allowed origins and agent commands. `cutover` also stops and backs up
an installed `menagerie-relay` service (launchd agent or systemd unit) before
loading the Continuum one, so the switch is reversible: the backup is
`<unit>.cutover-bak-<date>`. If no relay service is installed, it simply installs
the Continuum service adopting the config. Preview it all with
`CONTINUUM_SERVICE_DRYRUN=1`.

`serve --adopt-relay PATH` does the same adoption for a foreground run. After a
cutover, browser-spawned PTY blocks gain holder-based restart survival, and the
`/v1` door is available locally alongside the unchanged legacy WebSocket.

## Upgrade and rollback boundary

On-disk schema 2 is intentionally not readable by schema-1 binaries. Keep the
exact old binary/unit and make a copy of `state.db` while no daemon owns it
before the first schema-2 start. A rollback restores that matching database
copy before loading the old binary; starting the old binary directly on schema
2 fails with `unsupported state schema`. This alpha does not yet bundle a
general backup/restore command, so do not treat a binary-only downgrade as
safe.

That database copy is a point-in-time rollback boundary. For a pre-existing PTY
the holder can replay its bounded post-backup byte tail, as the matrix proves,
but journal/control changes after the copy are not generally recoverable and a
block first opened by the new version is absent from the old database. Settle
new blocks before rollback or stay on the newer binary; rollback is not a
lossless time machine.

`python3 scripts/verify.py verify upgrade` builds the pinned schema-1 revision
and exercises private dry-run install/update/rollback units plus real foreground
daemon replacements. It proves old-client/new-daemon and new-client/old-daemon
stable operations, same-PID PTY adoption, schema migration, downgrade refusal,
and matching-backup rollback. It never registers a service or reads the user's
installed state. Native systemd execution and structured-agent survival remain
untested.
