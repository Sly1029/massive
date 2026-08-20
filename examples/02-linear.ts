import { workflow } from "@massive/sdk";
import { z } from "zod";

const Request = z.object({ value: z.int() });
const Result = z.object({ message: z.string() });

export function double({ input }: { readonly input: z.infer<typeof Request> }) {
  return { value: input.value * 2 };
}

export function label(
  { input }: { readonly input: { readonly value: number } },
): z.infer<typeof Result> {
  return { message: `value:${input.value}` };
}

const graph = workflow({
  name: "linear-example",
  input: Request,
  output: Result,
});

const doubled = graph.step("double", {
  input: Request,
  output: Request,
  run: double,
});
const labeled = graph.step("label", {
  input: Request,
  output: Result,
  run: label,
});

graph.start().to(doubled).to(labeled).to(graph.end());

export default graph;
