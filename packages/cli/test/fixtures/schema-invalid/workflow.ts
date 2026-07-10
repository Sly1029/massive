import { workflow } from "@massive/sdk";
import { z } from "zod";

// The runner executes the exported `double` symbol, which returns a string while
// its step declares a numeric output. The runner's output-schema validation
// fails at the step boundary -> runner exit 65 (schema-validation-failure),
// propagated by the CLI. Mirrors internal/orchestrator/testdata/invalid-output.
export function double(_args: { readonly input: number }): string {
  return "not-a-number";
}

export function increment(args: { readonly input: number }): number {
  return args.input + 1;
}

export function label(args: { readonly input: number }): string {
  return `value:${args.input}`;
}

const flow = workflow({
  name: "schema-invalid",
  input: z.number(),
  output: z.string(),
});
// The builder's `run` for `double` is a well-typed placeholder (number -> number)
// so emission type-checks; execution uses the exported `double` symbol above,
// which is the one that violates the output schema.
const doubleStep = flow.step("double", {
  input: z.number(),
  output: z.number(),
  run: ({ input }) => input * 2,
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
