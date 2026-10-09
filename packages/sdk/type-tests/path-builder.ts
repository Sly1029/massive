import { z } from "zod";
import { workflow } from "../src/index.ts";

const g = workflow({
  name: "types",
  input: z.int(),
  output: z.string(),
});

const numberToNumber = g.step("number-to-number", {
  input: z.int(),
  output: z.int(),
  run: ({ input }) => input + 1,
});

const stringToString = g.step("string-to-string", {
  input: z.string(),
  output: z.string(),
  run: ({ input }) => input,
});

const numberToString = g.step("number-to-string", {
  input: z.int(),
  output: z.string(),
  run: ({ input }) => String(input),
});

const mergeNumbers = g.step("merge-numbers", {
  input: z.array(z.int()),
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

workflow({
  name: "no-mutable-state",
  input: z.int(),
  output: z.int(),
  // @ts-expect-error workflow state is not a portable dataflow mechanism
  state: {},
});

g.step("no-channels", {
  input: z.int(),
  output: z.int(),
  // @ts-expect-error channel publication has no execution semantics
  channel: "result",
  run: ({ input }) => input,
});

g.step("no-publish", {
  input: z.int(),
  output: z.int(),
  // @ts-expect-error return a typed output instead of publishing to a channel
  publish: { result: "result" },
  run: (context) => {
    // @ts-expect-error step state is not part of the invocation interface
    context.state;
    return context.input;
  },
});

// Edges are checked from the consumer's side: a step accepts any producer whose
// output is assignable to its input, and nothing narrower is accepted.
const onlyLiteral = g.step("only-literal", {
  input: z.literal("only-this"),
  output: z.string(),
  run: ({ input }) => input,
});

const stringOrNumber = g.step("string-or-number", {
  input: z.union([z.string(), z.int()]),
  output: z.string(),
  run: ({ input }) => String(input),
});

const partialRecord = g.step("partial-record", {
  input: z.int(),
  output: z.object({ a: z.int() }),
  run: ({ input }) => ({ a: input }),
});

const fullRecord = g.step("full-record", {
  input: z.object({ a: z.int(), b: z.int() }),
  output: z.string(),
  run: ({ input }) => String(input.a + input.b),
});

g.from(numberToString).to(stringOrNumber).to(g.end());

// @ts-expect-error a string output does not satisfy a literal input
g.from(numberToString).to(onlyLiteral);

// @ts-expect-error an output missing field b does not satisfy the input record
g.from(partialRecord).to(fullRecord);

const widen = g.step("widen", {
  input: z.string(),
  output: z.union([z.string(), z.int()]),
  run: ({ input }) => input,
});

// @ts-expect-error string | number output does not satisfy the string workflow output
g.from(widen).to(g.end());

// A call is a node typed by its child workflow's input and output.
const child = workflow({ name: "child", input: z.int(), output: z.string() });
child.start()
  .to(child.step("child-label", { input: z.int(), output: z.string(), run: ({ input }) => String(input) }))
  .to(child.end());
const called = g.call("called", child);
g.start().to(called).to(g.end());
g.merge([numberToString, called]).to(mergeStrings);

// @ts-expect-error the child consumes an int, not a string
g.from(numberToString).to(called);

// @ts-expect-error the child produces a string, not the int this step consumes
g.from(called).to(numberToNumber);

// A transform is a named step whose input is the value on its path.
g.start().transform("to-text", { output: z.string(), run: ({ input }) => String(input) }).to(g.end());
g.from(called).transform("shout", { output: z.string(), run: ({ input }) => input.toUpperCase() });

g.from(numberToString).transform("wants-number", {
  output: z.string(),
  // @ts-expect-error a transform's run must accept the string on its path
  run: ({ input }: { readonly input: number }) => String(input),
});

g.start()
  .transform("to-text-again", { output: z.string(), run: ({ input }) => String(input) })
  // @ts-expect-error a transform's string output does not satisfy an int input
  .to(numberToNumber);

// @ts-expect-error a transform's output must match its output schema
g.start().transform("wrong-output", { output: z.string(), run: ({ input }) => input });
