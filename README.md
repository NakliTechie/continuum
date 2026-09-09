# Continuum

> **Lifecycle:** draft — scaffolded 2026-09-09; design and migration baseline, no runtime shipped.

A durable runtime for local and remote work. Terminals, coding agents, background jobs, and services share sessions, history, structured events, and controls that people and software can use equally.

Continuum starts from the lessons and working Go relay in Menagerie. Menagerie remains the browser interface for agent fleets. Continuum develops a broader work-session model and CLI, with a proposed shared runtime behind both products. The working name is provisional.

## Direction

- One Go runtime per host, usable by Menagerie, a CLI, and future clients. Keep one implementation of process ownership, session state, authorization, history, and supervision.
- Work survives closing a client. Multiple viewers can observe without taking control from one another. Control is explicit and revocable.
- Local and remote operations have the same API: inspect a machine, browse directories, open a terminal, drive an agent, observe a job, or manage a workspace.
- Structured streams can be merged, filtered, exported as NDJSON, and consumed from Nushell. Cross-machine coordination has a durable owner outside the browser.
- History recovery, limits, retention, and failures are designed in. A recorded byte stream is not a promise of recoverable process state.
- Start with personal machines and explicit trust boundaries. OS-login integration, hostile-network optimization, and production operations are separate later milestones.

## Why Go

Use the language of the existing relay and preserve its tested PTY, ACP, and coordination work. Go's concurrency model fits independent sessions and stream consumers. No measured requirement currently justifies a Rust rewrite. Reconsider a bounded component only if profiling or a platform constraint supplies evidence.

## Can one binary serve both projects?

Yes, as the target architecture. Start by treating the current Menagerie relay as the sole runtime source. Establish compatibility tests, then move runtime ownership once into this repository with history and attribution preserved. The future binary exposes the existing Menagerie protocol through an adapter and a versioned Continuum API over the same session manager. Retain the `menagerie-relay` invocation during migration. Two adapters must never mean two process managers or two writable session stores.

This scaffold does not replace the installed relay, copy its implementation, or claim the migration is complete. Details: [shared runtime migration](docs/shared-runtime.md).

## Start here

1. [SPEC.md](SPEC.md): proposed architecture, failure guarantees, interfaces, and milestones.
2. [walkthroughs.md](walkthroughs.md): scope decisions with recommended defaults and tradeoffs.
3. [docs/shared-runtime.md](docs/shared-runtime.md): ownership, compatibility matrix, and cutover.
4. [docs/menagerie-baseline.md](docs/menagerie-baseline.md): inspected implementation and existing gaps.
5. [docs/sources.md](docs/sources.md): external references separated from our conclusions.
6. [DEFERRED.md](DEFERRED.md): later work with explicit revisit triggers.

`plan/` contains local working history, pending items, and the ordered workplan; it is deliberately gitignored. Durable requirements and acceptance criteria live in the tracked documents above.

## Current status

The first spec draft and source inventory are present. No daemon, CLI, remote listener, migration, or service has been installed or implemented. The first implementation milestone is a compatibility harness around the existing relay, followed by a bounded vertical slice with two viewers and recoverable events.

Inherited code must retain Menagerie's AGPL-3.0 notices and attribution. Its license text is retained in this scaffold; no Menagerie runtime code has been copied.
