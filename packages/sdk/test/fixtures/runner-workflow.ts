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

export const assertMappedIdentity = {
  run: (({ input, context }) => {
    const expected =
      "massive-invocation-v1/run-runner-fixture-0001/double/scope/maps/map-double/items/3/attempt/1";
    if (context.idempotencyKey !== expected) {
      throw new Error(
        `unexpected idempotency identity: ${context.idempotencyKey}`,
      );
    }
    return { value: input.value * 2 };
  }) satisfies StepRun<ValueInput, ValueOutput>,
};

export const assertNestedMappedIdentity = {
  run: (({ input, context }) => {
    const expected =
      "massive-invocation-v1/run-runner-fixture-0001/double/scope/maps/outer/items/0/maps/inner/items/4/attempt/1";
    if (context.idempotencyKey !== expected) {
      throw new Error(
        `unexpected idempotency identity: ${context.idempotencyKey}`,
      );
    }
    return { value: input.value * 2 };
  }) satisfies StepRun<ValueInput, ValueOutput>,
};
