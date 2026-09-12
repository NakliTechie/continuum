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
signals only the `continuum serve` process — the holder subprocesses that keep
PTY blocks alive stay running and are re-adopted when the daemon comes back.

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
