# Go core

The control plane owns frontend dispatch, compilation, target binding, and run
inspection. Language adapters exchange canonical WorkflowSpec and invocation
artifacts; keep scheduling and target lowering here in Go.

For graph contract changes, read `../docs/spec/ir-and-datastore.md` and
`../conformance/AGENTS.md`. Migrate emitters, schemas, compiled plans, and callers
in the same change. `irversion.Current` is the only accepted graph IR version.
Target compilers must reject requirements they cannot execute faithfully.

Use shared contract validators at artifact boundaries. A recomputed content hash
proves identity, not valid graph semantics. Preserve semantic validation before
execution even when a plan has been rehashed.
