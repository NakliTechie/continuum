# ACP startup and viewer buffering

Structured startup installs update and permission sinks together. Events accepted before sink installation are delivered exactly once in their original mixed order. Permission request identifiers and offered options remain unchanged; invalid answers retain the request so the client can correct them. Responses are deliberate; startup never auto-approves a permission. After the final pending answer, an active turn returns to running and a session without a turn returns to idle, allowing its first prompt.

The startup queue is bounded to 2048 frames and 32 MiB of encoded payload. Outstanding permissions are bounded separately to 64 requests and 32 MiB of encoded request data. Exceeding either bound faults and stops that managed agent, with an explicit startup error or a per-block incomplete-history marker if startup already returned. These limits bound retained payloads, not peak RSS or all parser allocations.

The legacy viewer outbox keeps at most 384 frames and 16 MiB. Its reconnect tail keeps at most 256 frames and 16 MiB. A slow viewer receives the existing frames_dropped marker; durable capture proceeds independently. Retention can evict older content; the tail is a recent replay window, not the whole conversation. One in-flight socket write and its temporary encodings are additional to the queued-byte bound.

The startup handshake must return session/new or session/load before the browser can answer a permission. A permission emitted before that reply is buffered and shown when startup completes. An agent that waits for a human permission answer before returning its handshake result remains unsupported by the current legacy spawn contract: startup ends on its 30-second handshake deadline or the caller’s earlier cancellation. It is not silently approved or retried. Supporting interactive permission during the handshake needs a separate browser protocol/UI design.

Local fake-agent tests cover startup replay and permission delivery. They do not establish real-provider interoperability, peak-memory benchmarks, or a full ACP conformance guarantee.
