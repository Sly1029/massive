import { NonRetryableError } from "@massive/sdk";
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

class PermanentInputError extends NonRetryableError {
  constructor() {
    super("fixture input is permanently invalid");
    this.name = "PermanentInputError";
  }
}

export const rejectPermanently = {
  run: (() => {
    throw new PermanentInputError();
  }) satisfies StepRun<ValueInput, ValueOutput>,
};

export const assertAttemptContext = {
  run: (({ context }) => {
    const expected = "massive-invocation-v2/run-runner-fixture-0001/double";
    if (context.idempotencyKey !== expected) {
      throw new Error(
        `unexpected idempotency identity: ${context.idempotencyKey}`,
      );
    }
    return { value: context.attempt * 10 + context.maxAttempts };
  }) satisfies StepRun<ValueInput, ValueOutput>,
};

export const assertMappedIdentity = {
  run: (({ input, context }) => {
    const expected =
      "massive-invocation-v2/run-runner-fixture-0001/double/scope/maps/map-double/items/3";
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
      "massive-invocation-v2/run-runner-fixture-0001/double/scope/maps/outer/items/0/maps/inner/items/4";
    if (context.idempotencyKey !== expected) {
      throw new Error(
        `unexpected idempotency identity: ${context.idempotencyKey}`,
      );
    }
    return { value: input.value * 2 };
  }) satisfies StepRun<ValueInput, ValueOutput>,
};
