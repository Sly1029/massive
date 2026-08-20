import { workflow } from "@massive/sdk";
import { z } from "zod";

export function split({ input }: { readonly input: number }): number {
  return input;
}

export function addOne({ input }: { readonly input: number }): number {
  return input + 1;
}

export function triple({ input }: { readonly input: number }): number {
  return input * 3;
}

export function total(
  { input }: { readonly input: readonly number[] },
): number {
  return input.reduce((sum, value) => sum + value, 0);
}

const graph = workflow({
  name: "diamond-example",
  input: z.int(),
  output: z.int(),
});

const root = graph.step("split", {
  input: z.int(),
  output: z.int(),
  run: split,
});
const left = graph.step("addOne", {
  input: z.int(),
  output: z.int(),
  run: addOne,
});
const right = graph.step("triple", {
  input: z.int(),
  output: z.int(),
  run: triple,
});
const merged = graph.step("total", {
  input: z.array(z.int()),
  output: z.int(),
  run: total,
});

graph.start().to(root);
graph.from(root).to(left);
graph.from(root).to(right);
graph.merge([left, right]).to(merged).to(graph.end());

export default graph;
