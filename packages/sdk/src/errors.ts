export class MassiveError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "MassiveError";
  }
}

export class SchemaPortabilityError extends MassiveError {
  constructor(role: string, reason: string) {
    super(`${role} uses a non-portable Zod schema: ${reason}`);
    this.name = "SchemaPortabilityError";
  }
}

export class GraphValidationError extends MassiveError {
  constructor(message: string) {
    super(message);
    this.name = "GraphValidationError";
  }
}

export class SourcePackagePathError extends MassiveError {
  constructor(message: string) {
    super(message);
    this.name = "SourcePackagePathError";
  }
}

// Step code throws this (or a subclass) for failures another attempt cannot
// fix. The runner exits 67 and the orchestrator does not retry the step.
export class NonRetryableError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "NonRetryableError";
  }
}

// Stop the run, including retries and collecting maps, with a safe cause.
export class FatalRunError extends NonRetryableError {
  readonly type?: string;

  constructor(message: unknown, type?: unknown) {
    super(
      typeof message === "string"
        ? Array.from(message.slice(0, 2048)).slice(0, 1024).join("")
        : "Fatal run failure",
    );
    this.type = typeof type === "string"
      ? Array.from(type.slice(0, 128)).slice(0, 64).join("")
      : undefined;
    this.name = "FatalRunError";
  }
}

export class DatastoreKeyError extends MassiveError {
  constructor(key: string, reason: string) {
    super(`Invalid datastore key "${key}": ${reason}`);
    this.name = "DatastoreKeyError";
  }
}
