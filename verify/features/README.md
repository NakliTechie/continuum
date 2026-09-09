# Verification map

`python3 scripts/verify.py doctor` checks prerequisites and identifies this checkout.
`python3 scripts/verify.py verify` builds fresh executables and runs the complete automated alpha gate.

1. [Core](core.md) — journal, fencing, process lifecycle, inherited packages.
2. [CLI](cli.md) — a new user's real local session journey.
3. [Legacy](legacy.md) — Menagerie protocol compatibility.

Every daemon uses a temporary private home/state and allocated loopback port. No installed relay is reused. No model providers are invoked. This automated gate does not replace the manual browser walk, native Linux execution, restart-surviving processes or remote-host release gates.
