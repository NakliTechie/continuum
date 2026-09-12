# Experimental server-owned terminal state

Status: overnight implementation experiment; opt-in integration and interactive attachment are separate gates. This document describes the engine contract, not a completed migration of Menagerie.

## Decision

Keep the shared runtime in Go and put a pinned `github.com/charmbracelet/x/vt` engine behind `internal/terminal.Engine`. Candidate revision: [3986e9119cf9](https://github.com/charmbracelet/x/tree/3986e9119cf9/vt), module `v0.0.0-20260906004030-3986e9119cf9`. The module is MIT licensed; its notice is in `third_party/charm-vt-LICENSE`.

This candidate avoids a second executable and a Rust/C/Zig build chain while establishing a replaceable terminal boundary. It does not settle the long-term engine choice. The [source crib sheet](research/terminal-session-crib-sheet-2026-09-09.md) retains wezterm-term, rio-vt and libghostty-vt as alternatives. Their benchmark results were reported by upstream authors; this run has not executed those implementations or established comparative performance.

The engine consumes PTY output and returns terminal-generated replies independently of attached viewers. Process ownership, reply delivery, input control and event recording remain the runtime's responsibility. A synchronous reply drain avoids the dependency's blocking input pipe when no viewer exists. Closing that pipe and joining its reader before closing the engine avoids racing the dependency's unsynchronized closed flag.

## Snapshot contract

`Snapshot()` copies the visible grid, generated styled rows, cursor, selected input modes, alternate-screen flag, retained scrollback count and a monotonic revision while holding one engine lock. Repeated observation does not resize the engine, consume history or produce terminal replies. Rows contain generated SGR and text; application OSC hyperlinks, clipboard requests, titles and arbitrary raw terminal controls are not replayed into a viewer.

A revision identifies a complete visible frame. It is independent of the durable journal cursor. A snapshot is not serialized parser state, a complete terminal-state dump, or a valid point from which to replay arbitrary raw bytes. A client using this contract must continue displaying complete server frames; any later diff protocol must identify and check its exact base. Engine state is memory-only and cannot promise process or screen survival across daemon restart.

While the application has synchronized output set (DEC private mode 2026), `Snapshot()` keeps returning the frame from before the update began, at its old revision, until the application resets the mode or 150 ms pass — whichever is first. Viewers therefore never poll a half-drawn screen; an application that sets the mode and stalls cannot freeze them. Repeated snapshots at one revision reuse the last rendered frame; rows are copied so callers own what they receive.

## Bounds and failure behavior

- At most 240 columns, 100 rows and 19,200 visible cells. Both primary and alternate grids are bounded.
- At most 200 retained primary-screen scrollback lines. Scrollback is counted but not exported in the initial snapshot contract.
- At most 32 KiB per feed, 64 KiB of generated replies per feed/resize, 4 KiB per escape sequence and numeric sequence parameters up to 4096. A grapheme in one feed is limited to 256 bytes.
- The upstream parser allocates a 4 MiB data buffer. These are component limits, not a measured whole-process memory budget or a hostile-code sandbox.
- Invalid requested dimensions fail without changing the screen. An over-limit output stream becomes explicitly faulted, refuses further parsing and marks every later snapshot stale through its `fault` field. Callers must surface that fault instead of continuing to present the screen as current.

Legacy raw output recording remains a separate data source. These limits must not silently truncate it or silently convert legacy sessions to the experimental engine.

## Executed checks and known gaps

`go test -race ./internal/terminal` checks detached cursor/device/color/mode queries, ANSI operating status, split CSI/OSC sequences and UTF-8 bytes, primary/alternate display switching, bounded scrollback, resize, immutable observation, generated-row escape isolation, fault behavior, concurrent readers/writers and shutdown. A discovered upstream DSR 5 response (`CSI ? 0 n`) is corrected in the adapter to standard `CSI 0 n`; the original assertion remains intact.

A separate direct-library characterization on this machine found that an emoji ZWJ sequence written whole retained its joiner, while three writes for its component emoji/joiner/emoji lost that joiner. Combining-accent text remained in the string representation, but that probe did not establish cursor/cell parity. Complex Unicode across chunk boundaries is therefore an explicit compatibility gap. No full vttest, real provider TUI, graphics, keyboard-protocol, terminal-reset, palette or accessibility conformance is claimed. The library's reported VT220 identity is not certification of full VT220/xterm compatibility.

Promotion gate: run a sustained corpus and real full-screen applications, confirm Unicode cell/cursor behavior, terminal reset and alternate-screen cursor restoration, query ownership with multiple clients, and renderer fidelity. Replace the engine if those gaps cannot be bounded cleanly; do not weaken the contract to call promotion complete.

