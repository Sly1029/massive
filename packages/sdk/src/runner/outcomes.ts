export const RUNNER_EXIT_CODES = {
  success: 0,
  descriptorResolutionFailure: 64,
  schemaValidationFailure: 65,
  stepExecutionFailure: 66,
} as const;

export class DescriptorError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "DescriptorError";
  }
}

export class SymbolResolutionError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "SymbolResolutionError";
  }
}

export class StepSchemaValidationError extends Error {
  constructor(
    readonly role: "schema" | "input" | "output",
    message: string,
  ) {
    super(`${role} validation failed: ${message}`);
    this.name = "StepSchemaValidationError";
  }
}

export class StepExecutionError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "StepExecutionError";
  }
}

export interface StepSuccess {
  readonly kind: "success";
  readonly exitCode: typeof RUNNER_EXIT_CODES.success;
  readonly runId: string;
  readonly nodeId: string;
  readonly attempt: number;
  readonly output: {
    readonly key: string;
    readonly hash: string;
    readonly contentType: string;
    readonly schema: string;
  };
}

export interface DescriptorResolutionFailure {
  readonly kind: "descriptor-resolution-failure";
  readonly exitCode: typeof RUNNER_EXIT_CODES.descriptorResolutionFailure;
  readonly error: DescriptorError | SymbolResolutionError;
}

export interface SchemaValidationFailure {
  readonly kind: "schema-validation-failure";
  readonly exitCode: typeof RUNNER_EXIT_CODES.schemaValidationFailure;
  readonly error: StepSchemaValidationError;
}

export interface StepExecutionFailure {
  readonly kind: "step-execution-failure";
  readonly exitCode: typeof RUNNER_EXIT_CODES.stepExecutionFailure;
  readonly error: StepExecutionError;
}

export type StepOutcome =
  | StepSuccess
  | DescriptorResolutionFailure
  | SchemaValidationFailure
  | StepExecutionFailure;

export function descriptorResolutionFailure(
  error: DescriptorError | SymbolResolutionError,
): DescriptorResolutionFailure {
  return {
    kind: "descriptor-resolution-failure",
    exitCode: RUNNER_EXIT_CODES.descriptorResolutionFailure,
    error,
  };
}

export function schemaValidationFailure(
  error: StepSchemaValidationError,
): SchemaValidationFailure {
  return {
    kind: "schema-validation-failure",
    exitCode: RUNNER_EXIT_CODES.schemaValidationFailure,
    error,
  };
}

export function stepExecutionFailure(
  error: StepExecutionError,
): StepExecutionFailure {
  return {
    kind: "step-execution-failure",
    exitCode: RUNNER_EXIT_CODES.stepExecutionFailure,
    error,
  };
}

export function formatOutcomeDiagnostic(
  outcome: Exclude<StepOutcome, StepSuccess>,
): string {
  return `${outcome.kind}: ${outcome.error.name}: ${outcome.error.message}`;
}
