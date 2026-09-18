# Continuum

One Go daemon per machine that owns your terminals and coding agents — durable history, explicit control, a CLI — so work outlives the client that started it. The runtime under [Menagerie](https://github.com/NakliTechie/menagerie).

Public alpha `0.1.0-alpha.2`, AGPL-3.0. macOS and Linux; one static binary.

## Install

```sh
brew install naklitechie/tap/continuum
```

or the checked installer (downloads the release archive for your OS/arch, verifies `SHA256SUMS`, installs to `~/.local/bin`; read it first if you like — it is 40 lines):

```sh
curl -fsSL https://raw.githubusercontent.com/NakliTechie/continuum/main/scripts/install.sh | sh
```

or from source with Go 1.26.8: `go build -o bin/continuum ./cmd/continuum`.

## First run

```sh
continuum serve                 # leave this running; prints the Menagerie endpoint
continuum open -- /bin/sh       # in another terminal
continuum status
continuum events --block BLOCK_ID --follow --text
```

Always-on (launchd on macOS, systemd `--user` on Linux):

```sh
continuum service install --listen 127.0.0.1:7878
continuum service status
```

Already running `menagerie-relay`? `continuum service cutover --adopt-relay ~/.menagerie/relay.toml` replaces it in place — same port, token, origins and agents. The unit embeds the absolute path of the binary that installed it, so after `brew upgrade continuum` run `service install` again.

## With Menagerie

Add the `ws://` address `serve` prints as a relay in Menagerie, with `operator.token` from the state directory as the registration token. The same binary answers as `menagerie-relay` and as `continuum legacy …`, so an existing Menagerie setup keeps working.

## What it does

- **Work survives the client.** Close the tab, drop the network, `kill -9` the daemon: the process keeps running under a per-block holder and is re-adopted on restart. Gaps are marked, never hidden.
- **Two doors, one core.** Menagerie's protocol and a versioned `/v1` contract (`continuum contract`) over one journal and one lease model.
- **Observe without stealing.** Many viewers per block; control is a 60-second explicit lease.
- **Honest history.** Per-block recording policy, explicit retention, replay from any cursor, asciicast export.
- **Experimental:** SSH clients, composed streams, durable cross-host waits, managed workspaces, scoped grants — see the architecture doc.

## Status

Alpha. Structured (ACP) sessions do not survive a daemon restart yet; the gate has run on macOS/arm64, the WAN drill on two Linux/amd64 hosts, nothing yet on a Linux systemd install or an arm64 remote host. Details and what each check covers: [docs/architecture.md](docs/architecture.md#verification).

## Docs

[docs/architecture.md](docs/architecture.md) · [CHANGELOG.md](CHANGELOG.md) · [SPEC.md](SPEC.md) · [docs/v1-contract.md](docs/v1-contract.md) · [docs/service.md](docs/service.md) · [DEFERRED.md](DEFERRED.md)

Licensed AGPL-3.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE) for the Menagerie relay lineage.
