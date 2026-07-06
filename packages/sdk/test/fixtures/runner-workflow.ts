import type { StepRun } from "../../src/workflow.ts";

interface ValueInput {
  readonly value: number;
}

interface ValueOutput {
  readonly value: number;
}

export const double = {
  run: (({ input }) => ({ value: input.value * 2 })) satisfies StepRun<
    ValueInput,
    ValueOutput
  >,
};

export const explode = {
  run: (() => {
    throw new Error("fixture step failed");
  }) satisfies StepRun<ValueInput, ValueOutput>,
};
