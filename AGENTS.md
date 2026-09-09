# Continuum contributor guidance

- Read README.md, SPEC.md, docs/shared-runtime.md, and local plan/ before implementation. The local alpha is implemented; SPEC.md still contains later release requirements. Never imply an unimplemented command or guarantee exists.
- Keep runtime ownership singular. Do not copy Menagerie's relay into a permanent second implementation or cross Go internal-package boundaries. Preserve AGPL notices/history during extraction.
- Protocol compatibility includes behavior, authority, and failure handling. Capability-gate new features; do not silently change legacy attach or approval semantics.
- Keep source observations separate from design proposals. Record observed checks and remaining limits.
- plan/ is local and gitignored. Tracked specs contain durable requirements; pending/workplan items carry source tags.
- Consult ~/Code/infra for relevant existing resources. Never read or reveal infra/secrets values for routine setup.
- Do not install/replace the live Menagerie relay, restart agents, expose listeners, provision paid resources, or migrate existing data as a side effect of design or tests.
- Prefer isolated test homes/ports; use local fake agents unless a real provider run is explicitly in scope. No tests should spend model credits implicitly.

- Run `python3 scripts/verify.py verify [feature]` for the isolated gate. Raw `go test` on inherited packages needs an explicit temporary HOME/TMPDIR; preserve Go cache/module locations separately. Do not let fake captures use the normal Menagerie directory.
