# Local development binaries

The development version is `0.1.0-alpha.2-dev`. Reviewed local artifacts append their source commit to that version through Go’s linker `-X github.com/NakliTechie/continuum/internal/cli.Version=...` option. Run `continuum version` to identify the build. These artifacts are not a tagged or published release.

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

Do not replace an installed Menagerie relay as part of this demo. Installation cutover, release publication, remote hosts, native Linux execution, full TUI/Unicode conformance and real-provider runs remain separate gates.
