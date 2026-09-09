# Menagerie source adapter — cutover boundary

> Lifecycle: staged design, 2026-09-09. Public source cutover has not been performed.

The GitHub API reports Menagerie public and Continuum private. The browser can already connect to the new locally built binary, but a public Menagerie Go module cannot depend on this private module for anonymous builds. Keep the existing public source and release pipeline until a public runtime release is available. This is a distribution prerequisite, not a second-runtime design.

After a public shared release passes the remaining compatibility gates:

1. Replace Menagerie's `relay-go` runtime implementation with a module requiring that exact Continuum version.
2. Keep `cmd/menagerie-relay/main.go` as `import "github.com/NakliTechie/continuum/legacy"; func main(){ legacy.Main() }`.
3. Keep the fleet checker entry as a wrapper around `github.com/NakliTechie/continuum/fleet/checkcli.Main`.
4. Keep Menagerie's browser protocol fixtures and client tests. Move runtime changes/tests exclusively through Continuum. Update Go-package and mirror-check paths in the release pipeline.
5. Build both product entry points, run the legacy fixture/conformance suites, browser walkthrough and installation/rollback matrix before changing the installed service.

For development, a disposable Go workspace can include both modules without publishing either repository or adding absolute-path replacements to tracked go.mod files. No such replacement belongs in a release. Publishing Continuum or a public runtime module needs an explicit distribution decision; its current privacy is preserved.
