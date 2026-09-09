# Continuum — scope walkthroughs

> **Lifecycle:** draft — recommended defaults are written into SPEC.md; scope choices remain open for refinement, not blockers to this scaffold.

## Question 1 — Where does the shared runtime live?

> **Status:** 🟡 OPEN — recommend staged extraction into Continuum after compatibility tests.

Options: leave all shared runtime code in Menagerie permanently; extract once into Continuum; or create a third neutral repository. Recommend extraction into Continuum: clear broader ownership with two products consuming one version. Give up permanent independence of release planning. Revisit a neutral repo only when another independent consumer requires it. Preserve source history, notices, module boundaries, and one canonical implementation.

## Question 2 — What survives a restart?

> **Status:** 🟡 OPEN — recommend durable records/events/waits with explicitly separate process guarantees.

Options: promise only client-disconnect survival; persist work records and use tmux/agent-load where available; build a separate process-host service immediately. Recommend the middle option initially. It adds useful recovery without pretending an ACP process or a rebooted machine can continue unchanged. Revisit a separate process host when transparent runtime upgrades are required and measured tests show tmux/agent-load insufficient.

## Question 3 — How do several people share control?

> **Status:** 🟡 OPEN — recommend multiple observers and one fenced controller per block.

Options: current takeover-only attachment; observers plus leased control; simultaneous input. Recommend observers plus leased control to avoid competing keystrokes, resize fights, and approval races. Give up simultaneous typing initially. Revisit collaborative editing/input only with a concrete workflow requiring it. Decide lease expiry and takeover UX before implementing remote sharing.

## Question 4 — How much history is retained?

> **Status:** 🟡 OPEN — recommend explicit capture policy and bounded retention with typed gaps.

Options: unlimited automatic capture; opt-in content capture; bounded default capture with per-session exclusion. Select the default and exact disk/time limits before the first persistent runtime release. Preserve metadata without assuming content capture. Terminal output can contain credentials; do not describe redaction as complete protection. Choose storage driver and fsync/ack contract with crash tests.

## Question 5 — Who owns cross-host work?

> **Status:** 🟡 OPEN — recommend one explicit coordinator host running headless broker mode.

Options: browser broker; one persisted coordinator; replicated consensus. Recommend one coordinator to remove the browser lifetime dependency. Give up automatic failover initially; report outage and recover records on restart. Revisit replication only when a concrete availability objective requires it. Cross-host child references do not grant remote execution authority.

## Question 6 — Is this an SSH replacement in v1?

> **Status:** 🟡 OPEN — recommend no; retain authenticated WS/WSS and existing trusted connectivity first.

Options: full privileged OS-login implementation now; personal-host daemon now with later login adapter; remain solely a browser-agent tool. Recommend the middle option for broader sessions without a premature login-security claim. Revisit login integration after recovery, scoped authorization, auditing, and revocation are proven. QUIC and native clients are separately gated by measurements and user workflows.
