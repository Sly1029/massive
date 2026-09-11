# TypeScript adapter

The SDK emits WorkflowSpec and executes isolated invocations; Go owns plan
writing, scheduling, and target lowering. `src/frontend.ts` is a process adapter,
not a second CLI or cache layer.

Frontend stdout is exclusively the complete canonical spec. Route author logs
to stderr and use a writer that completes large writes. Keep directory-entry
resolution aligned with `src/resolve.ts` and the Go control plane.

For shared IR changes, follow `../../conformance/AGENTS.md`. Local author code is
trusted execution; step-only Deno permissions cannot isolate frontend imports.
