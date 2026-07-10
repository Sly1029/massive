import { workflow } from "@massive/sdk";
import { z } from "zod";

// See linear-chain/workflow.ts for why this sentinel write is a no-op inside the
// step runner and fires only during the CLI's in-process emit import.
try {
  const sentinel = Deno.env.get("MASSIVE_IMPORT_SENTINEL");
  if (sentinel !== undefined && sentinel !== "") {
    Deno.writeTextFileSync(sentinel, "imported\n", { append: true });
  }
} catch {
  // No env/write permission (the step runner): intentionally silent.
}

// Mirrors internal/orchestrator/testdata/diamond: input 20 -> split 20; left 21,
// right 60; merge([21, 60]) = 81 (fan-in via mergeInputs).
export function split(args: { readonly input: number }): number {
  return args.input;
}

export function left(args: { readonly input: number }): number {
  return args.input + 1;
}

export function right(args: { readonly input: number }): number {
  return args.input * 3;
}

export function merge(args: { readonly input: readonly number[] }): number {
  return (args.input[0] ?? 0) + (args.input[1] ?? 0);
}

const flow = workflow({
  name: "diamond",
  input: z.number(),
  output: z.number(),
});
const splitStep = flow.step("split", {
  input: z.number(),
  output: z.number(),
  run: split,
});
const leftStep = flow.step("left", {
  input: z.number(),
  output: z.number(),
  run: left,
});
const rightStep = flow.step("right", {
  input: z.number(),
  output: z.number(),
  run: right,
});
const mergeStep = flow.step("merge", {
  input: z.array(z.number()),
  output: z.number(),
  run: merge,
});

flow.start().to(splitStep);
flow.from(splitStep).to(leftStep);
flow.from(splitStep).to(rightStep);
flow.merge([leftStep, rightStep]).to(mergeStep).to(flow.end());

export default flow;
