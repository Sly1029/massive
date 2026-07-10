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

export class DatastoreKeyError extends MassiveError {
  constructor(key: string, reason: string) {
    super(`Invalid datastore key "${key}": ${reason}`);
    this.name = "DatastoreKeyError";
  }
}
