# Python adapter and artifacts

Read `../../../../docs/spec/file-artifacts.md` for Blob/Tree transport changes.
The frontend emits canonical WorkflowSpec; graph scheduling remains in Go.
`GraphBuilder.add()` and `map()` register ordinary top-level functions; execution
contracts belong to registration, not function wrappers. Keep signature inspection
in `_step.py`, shared by emission and runner loading. `StepContext[Input]` has no
dependency generic: live application clients are constructed within tasks.
Keep authoring graph composition separate from worker imports. The runner loads
the function by its archived module/export without importing an SDK wrapper type.
`NodeHandle[Input, Output]` is invariant in input (edges need equal schemas)
and covariant in output; only steps and calls accept edges, and value-only
handles (start, map, select, case) are `NodeHandle[Never, Output]`. Keep
`EdgePath.to`/`to_end` non-overloaded so checkers report one precise error;
`add`/`map`/`transform` overload Awaitable first because ty cannot solve
`Output | Awaitable[Output]`. `tests/typecheck/rejected_wiring.py` pins
rejected wiring for pyright and ty through their unused-ignore rules.
`GraphBuilder.call()` is statically expanded at emission into scoped node IDs.
Require one child entry and one child exit before rewiring the parent's edges.
The IR gives every graph exactly one start successor and end predecessor, and a
called child must remain a valid standalone workflow, so child start fan-out is
not supported; a child may end in a fan-in step. A call that consumes a fan-in
moves its `mergeInputs` onto the child's entry, which must be a step. `merge`
and `gather` validate the consumer type at `.to()`; a heterogeneous gather
decodes through the same tagged-union rule as decisions (`_tagged_union_cases`).
Keep reference-bearing fields (`decisionRef`, `selectInputs`, and `mergeInputs`)
and edges scoped together; the emitted Graph IR has no `call` nodes. Reject
recursive composition, scoped ID collisions, and over-long scoped IDs before
producing a spec. Expansion must stay spec-transparent: a composed graph emits
the same canonical spec as its hand-inlined twin using `<call>--<child>` IDs.
The Go compiler owns environment identity; `Container` describes requirements.
The dependency preflight probe is the separate top-level `massive_environment`
package. It must not import `massive` or workflow modules.
The frontend and runner also run under `-I`, with an explicit `sys.path`
entry for the workflow directory or verified snapshot. The `massive` launcher
always sets `MASSIVE_PYTHON` to its own interpreter; Go has no runner fallback.

Blob/Tree values carry immutable references. Publish bodies before committing
an output manifest; hydrate into invocation-local scratch. A changed working
copy needs an explicit snapshot. Preserve ownership checks when moving values
between ArtifactFiles contexts, and reject paths escaping an artifact tree.

Keep domain repository fetching, revision metadata, service clients, and report
models in application packages. Typed artifacts are the composition boundary;
implicit mutable instance state and pickle codecs are not runtime contracts.
Test real publication and hydration with the submission archive removed, and
cover the installed wheel as well as editable SDK imports.

The runner streams source archives to scratch and verifies the descriptor's
archive digest before opening the tar; do not buffer whole archives or read
entries from unverified bytes. Bound the download by the largest valid archive
before hashing, since pods hold store write credentials. Source limits match
the Go verifier and `conformance/fixtures/source-limits`. Missing, oversized,
mismatched, and access-denied archives are descriptor failures (exit 64), never
retried: without s3:ListBucket, S3 reports a missing key as AccessDenied.

Keep the extracted source directory importable throughout invocation and output
serialization; ordinary functions and validators may lazily import sibling modules.
