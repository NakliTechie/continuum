# Verification map

`python3 scripts/verify.py doctor` checks prerequisites and identifies this checkout.
`python3 scripts/verify.py verify` builds fresh executables and runs the complete automated alpha gate.

1. [Core](core.md) — journal, fencing, process lifecycle, inherited packages.
2. [CLI](cli.md) — a new user's real local session journey.
3. [Terminal](terminal.md) — real PTY attachment, detached query ownership and renewal.
4. [Legacy](legacy.md) — Menagerie protocol compatibility.

Every daemon uses a temporary private home/state and allocated loopback port. No installed relay is reused. No model providers are invoked. This automated gate does not replace the manual browser walk, native Linux execution, restart-surviving processes or remote-host release gates.

The verifier sets disposable HOME and TMPDIR for every build, test, and check command and preserves only the existing Go cache/module locations. For raw `go test` runs, supply an isolated HOME/TMPDIR explicitly; inherited capture defaults otherwise target the normal Menagerie capture directory.
