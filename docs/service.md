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
