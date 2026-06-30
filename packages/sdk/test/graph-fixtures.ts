import { z } from "zod";
import { workflow, type WorkflowBuilder } from "../src/index.ts";

export interface GraphCase {
  readonly name: string;
  readonly input: number;
  readonly expected: number | string;
  readonly expectedTasks: number;
  readonly expectedEdges: number;
  readonly mergeExpectations?: Record<string, readonly string[]>;
  build(): WorkflowBuilder<number, number | string>;
}

export const graphCases = [
  passthroughGraphCase(),
  singleStepGraphCase(),
  linearChainGraphCase(),
  diamondGraphCase(),
  unevenFanInGraphCase(),
  multiStageMergeGraphCase(),
  batchMergeGraphCase(100),
] satisfies readonly GraphCase[];

export function passthroughGraphCase(): GraphCase {
  return {
    name: "passthrough",
    input: 42,
    expected: 42,
    expectedTasks: 0,
    expectedEdges: 1,
    build() {
      const g = workflow({ name: "passthrough", input: z.number(), output: z.number() });
      g.start().to(g.end());
      return g;
    },
  };
}

export function singleStepGraphCase(): GraphCase {
  return {
    name: "single-step",
    input: 41,
    expected: 42,
    expectedTasks: 1,
    expectedEdges: 2,
    build() {
      const g = workflow({ name: "single-step", input: z.number(), output: z.number() });
      const hello = g.step("hello", {
        input: z.number(),
        output: z.number(),
        run: ({ input }) => input + 1,
      });
      g.start().to(hello).to(g.end());
      return g;
    },
  };
}

export function linearChainGraphCase(): GraphCase {
  return {
    name: "linear-chain",
    input: 3,
    expected: "hello:8",
    expectedTasks: 3,
    expectedEdges: 4,
    build() {
      const g = workflow({ name: "linear-chain", input: z.number(), output: z.string() });
      const double = g.step("double", {
        input: z.number(),
        output: z.number(),
        run: ({ input }) => input * 2,
      });
      const increment = g.step("increment", {
        input: z.number(),
        output: z.number(),
        run: ({ input }) => input + 2,
      });
      const label = g.step("label", {
        input: z.number(),
        output: z.string(),
        run: ({ input }) => `hello:${input}`,
      });
      g.start().to(double).to(increment).to(label).to(g.end());
      return g;
    },
  };
}

export function diamondGraphCase(): GraphCase {
  return {
    name: "diamond",
    input: 4,
    expected: 45,
    expectedTasks: 4,
    expectedEdges: 6,
    mergeExpectations: { merge: ["left", "right"] },
    build() {
      const g = workflow({ name: "diamond", input: z.number(), output: z.number() });
      const split = g.step("split", { input: z.number(), output: z.number(), run: ({ input }) => input });
      const left = g.step("left", { input: z.number(), output: z.number(), run: ({ input }) => input + 1 });
      const right = g.step("right", { input: z.number(), output: z.number(), run: ({ input }) => input * 10 });
      const merge = g.step("merge", {
        input: z.array(z.number()),
        output: z.number(),
        run: ({ input }) => input.reduce((sum, value) => sum + value, 0),
      });

      g.start().to(split);
      g.from(split).to(left);
      g.from(split).to(right);
      g.merge([left, right]).to(merge).to(g.end());
      return g;
    },
  };
}

export function unevenFanInGraphCase(): GraphCase {
  return {
    name: "uneven-fan-in",
    input: 2,
    expected: 39,
    expectedTasks: 5,
    expectedEdges: 7,
    mergeExpectations: { merge: ["short", "long-tail"] },
    build() {
      const g = workflow({ name: "uneven-fan-in", input: z.number(), output: z.number() });
      const split = g.step("split", { input: z.number(), output: z.number(), run: ({ input }) => input });
      const short = g.step("short", { input: z.number(), output: z.number(), run: ({ input }) => input + 5 });
      const long = g.step("long", { input: z.number(), output: z.number(), run: ({ input }) => input * 10 });
      const longTail = g.step("long-tail", { input: z.number(), output: z.number(), run: ({ input }) => input + 12 });
      const merge = g.step("merge", {
        input: z.array(z.number()),
        output: z.number(),
        run: ({ input }) => input.reduce((sum, value) => sum + value, 0),
      });

      g.start().to(split);
      g.from(split).to(short);
      g.from(split).to(long).to(longTail);
      g.merge([short, longTail]).to(merge).to(g.end());
      return g;
    },
  };
}

export function multiStageMergeGraphCase(): GraphCase {
  return {
    name: "multi-stage-merge",
    input: 1,
    expected: 104,
    expectedTasks: 8,
    expectedEdges: 12,
    mergeExpectations: {
      "merge-a": ["a1", "a2"],
      "merge-b": ["b1", "b2"],
      "merge-final": ["merge-a", "merge-b"],
    },
    build() {
      const g = workflow({ name: "multi-stage-merge", input: z.number(), output: z.number() });
      const split = g.step("split", { input: z.number(), output: z.number(), run: ({ input }) => input });
      const a1 = g.step("a1", { input: z.number(), output: z.number(), run: ({ input }) => input + 10 });
      const a2 = g.step("a2", { input: z.number(), output: z.number(), run: ({ input }) => input + 20 });
      const b1 = g.step("b1", { input: z.number(), output: z.number(), run: ({ input }) => input + 30 });
      const b2 = g.step("b2", { input: z.number(), output: z.number(), run: ({ input }) => input + 40 });
      const mergeA = g.step("merge-a", {
        input: z.array(z.number()),
        output: z.number(),
        run: ({ input }) => input.reduce((sum, value) => sum + value, 0),
      });
      const mergeB = g.step("merge-b", {
        input: z.array(z.number()),
        output: z.number(),
        run: ({ input }) => input.reduce((sum, value) => sum + value, 0),
      });
      const mergeFinal = g.step("merge-final", {
        input: z.array(z.number()),
        output: z.number(),
        run: ({ input }) => input.reduce((sum, value) => sum + value, 0),
      });

      g.start().to(split);
      g.from(split).to(a1);
      g.from(split).to(a2);
      g.from(split).to(b1);
      g.from(split).to(b2);
      g.merge([a1, a2]).to(mergeA);
      g.merge([b1, b2]).to(mergeB);
      g.merge([mergeA, mergeB]).to(mergeFinal).to(g.end());
      return g;
    },
  };
}

export function batchMergeGraphCase(size: number): GraphCase {
  return {
    name: `batch-merge-${size}`,
    input: 1,
    expected: 5050,
    expectedTasks: size + 2,
    expectedEdges: size * 2 + 2,
    mergeExpectations: {
      merge: Array.from({ length: size }, (_, index) => workerId(index)),
    },
    build() {
      const g = workflow({ name: `batch-merge-${size}`, input: z.number(), output: z.number() });
      const split = g.step("split", { input: z.number(), output: z.number(), run: ({ input }) => input });
      const workers = Array.from({ length: size }, (_, index) =>
        g.step(workerId(index), {
          input: z.number(),
          output: z.number(),
          run: async ({ input }) => input + index,
        })
      );
      const merge = g.step("merge", {
        input: z.array(z.number()),
        output: z.number(),
        run: ({ input }) => input.reduce((sum, value) => sum + value, 0),
      });

      g.start().to(split);
      for (const worker of workers) {
        g.from(split).to(worker);
      }
      g.merge(workers).to(merge).to(g.end());
      return g;
    },
  };
}

function workerId(index: number): string {
  return `worker-${String(index).padStart(3, "0")}`;
}
