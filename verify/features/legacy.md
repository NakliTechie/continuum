# Legacy

Exists: inherited protocol 1.3, legacy CLI/config, and `continuum legacy ...` or `menagerie-relay` invocation.

Reach: Menagerie's Add relay form points at the alpha daemon's loopback endpoint and operator token. Observer tokens cannot register on this adapter.

Verify: `python3 scripts/verify.py verify legacy` tests authentication, PTY reattach/takeover, ACP prompt/wait/attention, approvals, subtree kill and offline output. `scripts/check_legacy.py --source PATH` independently tests the immutable upstream baseline.

Manual browser journey: use an isolated browser context and disposable daemon with an explicitly allowed local origin; skip folder/tour, add the test relay, choose custom `/bin/cat`, send a marker, and compare its output/session with the CLI. The browser auto-probes localhost:7878; never register with or operate that unrelated service during this test.

Watch: the old browser's attachment is a takeover, not read-only observation. New modern observers do not rotate its control token. Cross-client session discovery may require reconnecting the legacy browser. Real providers, browser folder persistence, terminal resize/selection edge cases and live service installation need separate evidence.
