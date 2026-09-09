# ACP prompt turns and outcomes

One structured session admits one prompt at a time. A second prompt while its predecessor is still active returns `prompt_busy`; a session awaiting a human decision retains the stronger `session_blocked` refusal. Cancel requests cancellation from the agent; a new prompt waits for that turn’s response or the process to stop. A wait timeout ends the wait, not the agent’s work.

An atomic prompt-plus-wait starts a new running generation before dispatch. Its wait cannot match a previous turn’s done/idle state or a later turn’s transitions. A successful response emits done after any usage update. Standalone waits retain immediate matching against current status. Unmatched prompt waits retain their caller timeout and report the status at expiry with `timed_out: true`. Exit drains all waits.

An RPC error, missing response, missing result, or malformed result produces `acp_prompt_failed`, with a session ID, turn ID and RPC code when supplied by the agent. The error is recorded and retained in the structured reconnect tail. Error messages are capped at 4096 bytes plus a truncation notice. This failure does not emit done or idle; the session’s tracked state becomes unknown. An outstanding wait carried by that prompt terminates separately with `prompt_wait_failed`, its original `wait_id`, and the same turn ID. Other waits continue to use their exact requested states. Clients should handle correlated error frames as well as waited frames.

These are transport-level turn outcomes. A successful ACP response is not proof that an agent’s task, code, or recommendation is correct. Provider-specific behavior, full ACP conformance, and real-provider runs remain outside the local fake-agent validation.
