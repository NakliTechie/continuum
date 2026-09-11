# Fleet validator parity

Continuum retains the runtime’s Go validator; Menagerie retains its browser validator. Run the shared fixture comparison with an explicit client source path:

```sh
node /absolute/path/to/continuum/fleet/mirror-check.mjs --client-html /absolute/path/to/menagerie/index.html
```

The command resolves the Go module and fixtures from its own location, so it works from another current directory and from a worktree. It reads the browser’s marked fleet validator from the specified HTML file and compares every valid, invalid, secret and normalization fixture. Comparisons retain issue paths, codes and duplicate counts. A missing Go result is a failure. Missing arguments or an unreadable client file return exit2; a comparison failure returns exit1.

This check needs Node with `Array.prototype.toSorted` support, Go, and a trusted Menagerie checkout. It executes the marked validator source from that HTML file. It does not install, edit, or start Menagerie. The core gate remains usable without an external client checkout; shared releases should run this parity check against their exact proposed Menagerie revision as well.

`fleet/testdata/invalid-pending-mirror/` holds rules the Go validator enforces that the browser validator has not adopted yet (required `workspace.isolation`, numeric minimums, `branch_prefix` shape). The parity check skips that directory on purpose; the Go suite still pins each fixture to its error path. Adopting those rules in Menagerie is client follow-through, after which the fixtures move into `invalid/`.
