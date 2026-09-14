# Python adapter and artifacts

Read `../../../../docs/spec/file-artifacts.md` for Blob/Tree transport changes.
The frontend emits canonical WorkflowSpec; graph scheduling remains in Go.
`GraphBuilder.add()` and `map()` register ordinary top-level functions; execution
contracts belong to registration, not function wrappers. Keep signature inspection
in `_step.py`, shared by emission and runner loading. `StepContext[Input]` has no
dependency generic: live application clients are constructed within tasks.
Keep authoring graph composition separate from worker imports. The runner loads
the function by its archived module/export without importing an SDK wrapper type.
The Go compiler owns environment identity; `Container` describes requirements.

Blob/Tree values carry immutable references. Publish bodies before committing
an output manifest; hydrate into invocation-local scratch. A changed working
copy needs an explicit snapshot. Preserve ownership checks when moving values
between ArtifactFiles contexts, and reject paths escaping an artifact tree.

Keep domain repository fetching, revision metadata, service clients, and report
models in application packages. Typed artifacts are the composition boundary;
implicit mutable instance state and pickle codecs are not runtime contracts.
Test real publication and hydration with the submission archive removed, and
cover the installed wheel as well as editable SDK imports.

Keep the extracted source directory importable throughout invocation and output
serialization; ordinary functions and validators may lazily import sibling modules.
