import { emitWorkflowSpec, resolveWorkflowEntrypoint } from "@massive/sdk";

const entry = Deno.args[0];
if (entry === undefined) {
  throw new Error("usage: deno run ... examples/emit.ts <workflow.ts>");
}

// Resolve the entrypoint as the frontend does, including its step export checks.
const resolved = await resolveWorkflowEntrypoint(entry);
const spec = await emitWorkflowSpec(resolved.workflow, {
  source: resolved.source,
  package: resolved.package,
});

console.log(JSON.stringify(spec, null, 2));
