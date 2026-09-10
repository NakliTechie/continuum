# ACP protocol provenance and permission outcomes

Continuum retains ACP protocol version 1 and the inherited Menagerie schema pin: [`b7f0005493b98de32fabee3e9540e2b64da68535`, schema/v1/schema.json](https://github.com/agentclientprotocol/agent-client-protocol/blob/b7f0005493b98de32fabee3e9540e2b64da68535/schema/v1/schema.json). Menagerie owns its generated browser types and fixtures. The runtime extraction does not contain that browser generator. This document replaces the imported references to its `protocol/acp-pin.md`.

The client answers `session/request_permission` with `result: {"outcome":{"outcome":"selected","optionId":"offered-id"}}`. Cancelling the turn answers pending requests with `result: {"outcome":{"outcome":"cancelled"}}`, without selecting an option. These are the inherited schema's response union and the [ACP v1 permission contract](https://agentclientprotocol.com/protocol/v1/tool-calls#requesting-permission).

The runtime serializes selection and cancellation, preserves request-ID precision and offered option IDs, and retains invalid or failed selections as pending. Successful cancellation discharges current requests; requests arriving before the next prompt receive cancelled replies. A new prompt resets that cancellation state. Delayed startup delivery skips already discharged permissions. Wire failures remain errors; cancellation does not auto-approve or retry a tool action.

Local tests use strict fake peers and the inherited schema shape. They do not establish real-provider or full ACP conformance. Menagerie's live permission-card cleanup after cancellation remains a browser change; the runtime's pending-permission guard no longer blocks the next prompt after a successful cancellation.
