# TypeScript adapter

The SDK emits WorkflowSpec and executes isolated invocations; Go owns plan
writing, scheduling, and target lowering. `src/frontend.ts` is a process adapter,
not a second CLI or cache layer.

A step's `export` (default: its id) must name its exported run function in the
entrypoint module: the runner resolves `module[symbol.export]`, and resolution
rejects any other binding, including for steps of called workflows.

`call()` is expanded at emission by `src/compose.ts`, which must stay in step
with the Python SDK's call expansion: calls expand in code-unit order of their
ids, child nodes become `<call>--<child node>` (validated against the IR id
pattern and 128-character limit), a child needs one start successor and one end
predecessor, edges and `mergeInputs` naming a call are rewired to its entry or
exit, and a merge into a call moves onto the child's entry step. Expanded steps
keep their export and their own workflow's contract defaults.
`conformance/fixtures/composition` and `internal/controlplane`'s composition
test pin both SDKs to the same graph and result.

Both frontends write the canonical spec only to the file named by
`emit --output <path>`; Go reads it from there, and frontend stdout and stderr
are author diagnostics. The wrapper grants Deno write access to that path alone.
Keep directory-entry resolution aligned with `src/resolve.ts` and the Go control
plane.

For shared IR changes, follow `../../conformance/AGENTS.md`. Local author code is
trusted execution; step-only Deno permissions cannot isolate frontend imports.

The TypeScript runner buffers source archives in memory, so it keeps a
1,024-file, 50 MiB cap below the shared Go/Python limits. Its rejection must
name the Python runner; `conformance/fixtures/source-limits` checks it.
