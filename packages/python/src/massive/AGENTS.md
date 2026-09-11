# Python adapter and artifacts

Read `../../../../docs/spec/file-artifacts.md` for Blob/Tree transport changes.
The frontend emits canonical WorkflowSpec; graph scheduling remains in Go.
Top-level typed functions can be registered directly without decorator syntax.

Blob/Tree values carry immutable references. Publish bodies before committing
an output manifest; hydrate into invocation-local scratch. A changed working
copy needs an explicit snapshot. Preserve ownership checks when moving values
between ArtifactFiles contexts, and reject paths escaping an artifact tree.

Keep domain repository fetching, revision metadata, service clients, and report
models in application packages. Typed artifacts are the composition boundary;
implicit mutable instance state and pickle codecs are not runtime contracts.
Test real publication and hydration with the submission archive removed, and
cover the installed wheel as well as editable SDK imports.
