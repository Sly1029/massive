# TypeScript adapter

The SDK emits WorkflowSpec and executes isolated invocations; Go owns plan
writing, scheduling, and target lowering. `src/frontend.ts` is a process adapter,
not a second CLI or cache layer.

A step id must name its exported run function in the entrypoint module: the
runner resolves `module[step.id]`, and emission rejects any other binding.

Both frontends write the canonical spec only to the file named by
`emit --output <path>`; Go reads it from there, and frontend stdout and stderr
are author diagnostics. The wrapper grants Deno write access to that path alone.
Keep directory-entry resolution aligned with `src/resolve.ts` and the Go control
plane.

For shared IR changes, follow `../../conformance/AGENTS.md`. Local author code is
trusted execution; step-only Deno permissions cannot isolate frontend imports.
