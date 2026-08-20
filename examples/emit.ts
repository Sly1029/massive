import { emitWorkflowSpec, type WorkflowBuilder } from "@massive/sdk";
import { basename, dirname, resolve } from "node:path";
import { pathToFileURL } from "node:url";

const entry = Deno.args[0];
if (entry === undefined) {
  throw new Error("usage: deno run ... examples/emit.ts <workflow.ts>");
}

const absoluteEntry = resolve(entry);
const loaded = await import(pathToFileURL(absoluteEntry).href);
const graph = loaded.default as WorkflowBuilder<unknown, unknown> | undefined;
if (graph === undefined) {
  throw new Error(`${entry} must have a default workflow export`);
}

const spec = await emitWorkflowSpec(graph, {
  source: {
    root: dirname(absoluteEntry),
    include: [basename(absoluteEntry)],
    module: basename(absoluteEntry),
  },
});

console.log(JSON.stringify(spec, null, 2));
