import { workflow } from "@massive/sdk";
import { z } from "zod";

// input 20 -> double 40 -> increment 41 -> "value:41" (mirrors linear-chain).
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
  name: "settings-import",
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
