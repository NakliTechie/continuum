# Changelog

All notable changes to Continuum are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/). Pre-1.0, the `/v1` API contract
(`continuum contract`) is the compatibility promise, not the binary version.

## [0.1.0-alpha.2] — 2026-09-18

First tagged release. Public alpha, AGPL-3.0. macOS and Linux, single static
binary; the same executable answers as `continuum` (the modern CLI and daemon)
and as `menagerie-relay` (the unchanged Menagerie relay entry point) by
basename.

### Added

- **Durable local daemon** (`continuum serve`, `continuum service install`):
  PTY and structured (ACP) agent sessions as recorded *blocks* with a bounded
  journal of output and lifecycle events; replay from any cursor
  (`events --after`, `--follow`, `--text`, `--raw`); asciicast v3 export.
- **Process survival across daemon restart** for PTY blocks via a
  per-session holder; records survive `kill -9` with an honest
  `history_incomplete` marker. Structured (ACP) sessions do not survive yet.
- **Versioned `/v1` contract** (`continuum/v1`, contract 1.0) on a private
  Unix socket inside the state directory; `continuum contract` negotiates
  stable vs experimental capabilities. Read-only observers and a 60-second
  fenced control lease (`acquire` / `renew` / `takeover` / `release`).
- **Server-owned terminal screens** (`--terminal screen-v1`), interactive
  `attach` with Ctrl-] detach, DEC 2026 synchronized-output holds, UTF-8
  locale and IUTF8 for children.
- **Menagerie compatibility door**: protocol 1.3 relay semantics preserved,
  `service cutover --adopt-relay` replaces an installed `menagerie-relay` in
  place (same port, token, origins, agents), pinned-baseline compat matrix.
- **Durable cross-host coordination** (`wait create/attach/output/cancel`,
  `peer-add`): any/all waits over existing SSH trust with typed outcomes,
  deadlines, idempotent request IDs and replay-stable completion order;
  a one-shot JSON stdio SSH bridge (`--host`, `--remote-binary`); scoped
  directory browsing (`serve --browse-root`).
- **Composed observer streams** (`streams`): bounded, source-labelled
  interleave over several blocks and hosts with checkpoints and pause/continue.
- **Managed workspaces** (`workspace`, `materialise`): hash-pinned fleet
  specs, worktree provisioning with bind-tested port allocation, fixed
  materialisation order, service supervision with health, bounded restart and
  explicit stop/destroy; `--dry-run` golden plans.
- **Scoped access** (`grant`, `share`, `revoke`, `audit`, `backup`): hashed
  show-once grants, per-block sharing, a read-only browser doorway that never
  accepts the operator token, pinned machine pairing and checked offline
  backup/restore.
- **Verification harness** (`scripts/verify.py`): race-detected package
  tests, `govulncheck`, CLI/terminal/legacy/upgrade/remote/streams/managed
  journeys, a network-free WAN-runner check and the opt-in two-host and
  three-server SSH drills.

### Changed

- A wait member whose recording is incomplete (unclean daemon epoch or
  capture loss) is marked `history: incomplete` and kept under watch instead
  of failing the wait; `history_gap` and other indeterminate reads still fail
  the source.
- The legacy door tells a displaced client `error{session_taken}` when
  another client attaches, and re-attach replay skips permission requests
  that were already answered.

### Known limits

- Structured (ACP) sessions end when the daemon restarts.
- Remote-host evidence is Linux/amd64 and macOS only; no Linux systemd
  service-install run, no arm64 remote host, no lossy-WAN benchmark, no cloud
  ACP provider validation. Untrusted multi-user hosting is out of scope.

[0.1.0-alpha.2]: https://github.com/NakliTechie/continuum/releases/tag/v0.1.0-alpha.2
