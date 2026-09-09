# Continuum — deferred work

> **Lifecycle:** living — items are retained with explicit triggers, 2026-09-09.

## Privileged login and production administration

**What:** OS-user mapping, PAM/login accounting, per-user limits, production policy and approvals, revocation, and shared operational sessions.
**Why deferred:** personal-host execution is a smaller initial trust boundary. Replacing SSH requires a dedicated security contract and real platform testing.
**Trigger:** scoped access, recovery, audit, and upgrade gates pass and the owner chooses a concrete login/production deployment.

## QUIC and poor-network optimizations

**What:** alternate transport, reconnect/roaming experiments, latency improvements inspired by the remote-session demo.
**Why deferred:** existing browser WS/WSS compatibility is valuable; there is no measured transport bottleneck yet.
**Trigger:** reproducible latency/loss/roaming benchmarks fail agreed interaction targets. Preserve a browser-compatible fallback.

## Native desktop/mobile clients and terminal polish

**What:** native terminal experience, OS selection/scrollback integration, mobile reconnect, accessibility and device-specific workflows.
**Why deferred:** the protocol and CLI vertical slice should establish the runtime first; Menagerie already supplies a browser client.
**Trigger:** the same contract works through CLI and Menagerie and a specific user workflow needs a native surface. Real iOS testing remains a requirement before mobile claims.

## Replicated coordination and peer mesh

**What:** automatic broker failover, multi-coordinator ownership, direct relay-to-relay child orchestration.
**Why deferred:** distributed ownership and credential delegation are substantially more complex than one explicit coordinator.
**Trigger:** a defined availability target or workload cannot tolerate one recoverable coordinator. Host-qualified IDs and explicit ownership must already exist.

## Sandboxes, Windows, and alternative runtimes

**What:** isolation providers, native Windows process/terminal support, or a measured Rust component.
**Why deferred:** worktrees are not isolation; Unix PTY reuse is the initial path; rewriting the Go relay would delay compatibility.
**Trigger:** a workload requires hostile-code isolation, a Windows user journey requires native support, or profiling/platform evidence justifies a bounded alternate implementation. Consult infra documentation before provisioning providers.

## Review queues, merge queues, cost reporting, and external signals

**What:** wider workflow/portfolio capabilities surrounding sessions.
**Why deferred:** Menagerie already routes related work to Sluice/One Job; duplicating those products is not required for a durable runtime.
**Trigger:** define an integration contract with those projects when a concrete workflow needs it. Preserve event interoperability instead of building an unrelated workflow suite.
