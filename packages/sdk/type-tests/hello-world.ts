import { z } from "zod";
import { type InvocationContext, type MapItemIdentity, type MapItemScopeFrame, workflow } from "../src/index.ts";

const helloWorld = workflow({
  name: "hello-world",
  input: z.string(),
  output: z.string(),
});

const hello = helloWorld.step("hello", {
  input: z.string(),
  output: z.string(),
  run: ({ input }) => `hello ${input}`,
});

const exclaim = helloWorld.step("exclaim", {
  input: z.string(),
  output: z.string(),
  run: ({ input }) => `${input}!`,
});

helloWorld.start().to(hello).to(exclaim).to(helloWorld.end());

const numberInput = helloWorld.step("number-input", {
  input: z.int(),
  output: z.string(),
  run: ({ input }) => String(input),
});

// @ts-expect-error hello-world starts with string input
helloWorld.start().to(numberInput);

const numeric = workflow({
  name: "numeric",
  input: z.int(),
  output: z.int(),
});

const double = numeric.step("double", {
  input: z.int(),
  output: z.int(),
  run: ({ input }) => input * 2,
});

const stringify = numeric.step("stringify", {
  input: z.int(),
  output: z.string(),
  run: ({ input }) => String(input),
});

numeric.start().to(double);

// @ts-expect-error numeric workflow output is number, not string
numeric.from(double).to(stringify).to(numeric.end());


export function itemIdentity(context: InvocationContext): number | null {
  const item: MapItemIdentity | null = context.item;
  if (item === null) return null;
  const frame: MapItemScopeFrame = item.path[item.path.length - 1]!;
  // @ts-expect-error item paths are immutable
  item.path.push(frame);
  // @ts-expect-error scope frames are immutable
  frame.index = 3;
  return item.index;
}
