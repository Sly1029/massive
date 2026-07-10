import { workflow } from "@massive/sdk";
import { z } from "zod";

// Import sentinel probe (see cache.functional.test.ts). The CLI's in-process
// emit imports this module with --allow-env/--allow-write, so it records exactly
// one line here. The step runner also imports this module, but the orchestrator
// spawns it without --allow-env and with --allow-write scoped to the datastore,
// so both the env read and the sentinel write throw and are swallowed — its
// import is a silent no-op. That asymmetry lets the cache test prove the emit
// import was skipped on a source-unchanged (cache-hit) run.
try {
  const sentinel = Deno.env.get("MASSIVE_IMPORT_SENTINEL");
  if (sentinel !== undefined && sentinel !== "") {
    Deno.writeTextFileSync(sentinel, "imported\n", { append: true });
  }
} catch {
  // No env/write permission (the step runner): intentionally silent.
}

// The step runner resolves and executes these exported symbols by step id, so
// the workflow's real semantics live here (mirrors internal/orchestrator/
// testdata/linear-chain): input 20 -> double 40 -> increment 41 -> "value:41".
export function double(args: { readonly input: number }): number {
  return args.input * 2;
}

export function increment(args: { readonly input: number }): number {
  return args.input + 1;
}

export function label(args: { readonly input: number }): string {
  return `value:${args.input}`;
}

const flow = workflow({
  name: "linear-chain",
  input: z.number(),
  output: z.string(),
});
const doubleStep = flow.step("double", {
  input: z.number(),
  output: z.number(),
  run: double,
});
const incrementStep = flow.step("increment", {
  input: z.number(),
  output: z.number(),
  run: increment,
});
const labelStep = flow.step("label", {
  input: z.number(),
  output: z.string(),
  run: label,
});
flow.start().to(doubleStep).to(incrementStep).to(labelStep).to(flow.end());

export default flow;
