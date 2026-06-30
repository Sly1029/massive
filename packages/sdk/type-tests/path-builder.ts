import { z } from "zod";
import { workflow } from "../src/index.ts";

const g = workflow({
  name: "types",
  input: z.number(),
  output: z.string(),
});

const numberToNumber = g.step("number-to-number", {
  input: z.number(),
  output: z.number(),
  run: ({ input }) => input + 1,
});

const stringToString = g.step("string-to-string", {
  input: z.string(),
  output: z.string(),
  run: ({ input }) => input,
});

const numberToString = g.step("number-to-string", {
  input: z.number(),
  output: z.string(),
  run: ({ input }) => String(input),
});

const mergeNumbers = g.step("merge-numbers", {
  input: z.array(z.number()),
  output: z.string(),
  run: ({ input }) => input.join(","),
});

const mergeStrings = g.step("merge-strings", {
  input: z.array(z.string()),
  output: z.string(),
  run: ({ input }) => input.join(","),
});

g.start().to(numberToNumber).to(numberToString).to(g.end());
g.merge([numberToNumber]).to(mergeNumbers).to(g.end());

// @ts-expect-error number output cannot flow into string input
g.start().to(numberToNumber).to(stringToString);

// @ts-expect-error workflow output is string, not number
g.start().to(numberToNumber).to(g.end());

// @ts-expect-error merged number outputs cannot flow into string array input
g.merge([numberToNumber]).to(mergeStrings);
