# ACP frames and durable replay

The ACP reader retains its existing limit: each newline-delimited frame must fit below the scanner’s 8 MiB token bound. Accepted session updates are passed through with their original JSON bytes. Permission requests retain the same envelope plus a small relay request identifier.

The journal admits those two structured event kinds up to 8 MiB plus 64 KiB of routing allowance. Ordinary event payloads retain their 256 KiB limit. Host history is bounded to 16 MiB and 4096 events, so one accepted frame fits even with its event metadata. Retention still evicts the oldest events and reports cursor gaps. These are logical data bounds; database allocation can be larger.

Event pages normally contain at most 256 scanned events and 512 KiB of selected data. A larger first selected event is returned whole, alone, so the cursor advances without discarding or splitting its payload. The local CLI allows up to 16 MiB for an event response. JSON transport/storage encoding preserves HTML-sensitive characters instead of multiplying frame sizes through HTML escaping; these are standalone application/json documents, not HTML script content.

An oversized or otherwise faulted ACP stdout stream stops that managed child and marks its block’s history incomplete, with a capture_error record when storage is available. This flag survives restart and applies to reads for that block. Reads across all blocks report any included incomplete history. An actual journal write failure still marks the shared writer degraded and fences durable mutations. A frame/reader fault alone does not disable control of unrelated sessions.

Frame retention is not a full ACP conformance or real-provider claim. Startup buffering, reconnect tails and slow-viewer queues have separate bounds and loss behavior.
