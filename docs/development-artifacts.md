# Local development binaries

Tagged releases carry the plain version (`continuum version` prints `continuum 0.1.0-alpha.2`) and ship as per-platform archives on the GitHub release, each with a `SHA256SUMS` entry. A local build from a later commit should append its source commit through Go’s linker `-X github.com/NakliTechie/continuum/internal/cli.Version=0.1.0-alpha.2+<sha>` option so it is never mistaken for the release; `go version -m ./continuum` also embeds the VCS revision and whether the tree was modified.

One executable implements both the Continuum CLI/daemon and Menagerie’s legacy commands. Invoke `continuum legacy ...`, or copy that same executable to the exact basename `menagerie-relay` for the compatibility entry point. Use the target platform’s build; Linux cross-builds do not establish native Linux runtime behavior.

To try interactive terminals with a disposable private state directory:

```sh
./continuum serve --state /absolute/private/demo-state
# Leave this foreground daemon running. In another terminal:
./continuum open --state /absolute/private/demo-state --terminal screen-v1 -- /bin/sh
./continuum attach --state /absolute/private/demo-state --block BLOCK_ID
```

Replace BLOCK_ID with the full value returned by open. Ctrl-] detaches the viewer. Attach again to recover the current screen. Add `--observer` from a second terminal to watch without typing or resizing. A normal attach refuses if another controller holds the block; the displayed commands let you watch or explicitly replace that controller.

Ctrl-C in the daemon terminal stops the daemon and its managed processes. Recorded history survives; this alpha’s processes and screen state do not survive daemon restart. The unchanged Menagerie browser can use the same daemon’s legacy endpoint for legacy-profile PTYs and ACP sessions. It cannot render the opt-in screen-v1 profile yet.

Do not replace an installed Menagerie relay as part of this demo. Installation cutover, remote hosts, native Linux execution, full TUI/Unicode conformance and real-provider runs remain separate gates from a release; the release archives are cross-compiled, and only the platforms named in the release notes ran a gate.

The artifact check compares `go version -m ./continuum` against `git rev-parse HEAD`, in addition to the CLI version. This Go toolchain skipped the nested worktree's `.git` file and reported the parent checkout's revision. The local packaging procedure therefore builds an identical committed tree from a clean standalone temporary clone and verifies both tree identity and embedded revision before delivery. This is a manual procedure: no committed script performs it, and `dist/` manifests are produced outside the repository.
