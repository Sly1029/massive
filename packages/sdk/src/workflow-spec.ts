import { Ajv2020 } from "ajv/dist/2020.js";
import type {
  AnySchema,
  ErrorObject,
  ValidateFunction,
} from "ajv/dist/2020.js";
import { MassiveError } from "./errors.ts";
import { computeSpecHash, type WorkflowSpec } from "./emit.ts";
import { stableStringify } from "./stable.ts";

export class WorkflowSpecError extends MassiveError {
  constructor(message: string) {
    super(message);
    this.name = "WorkflowSpecError";
  }
}

let workflowSpecValidator: Promise<ValidateFunction<WorkflowSpec>> | undefined;

// Validate an already-decoded WorkflowSpec from any language runtime. Callers
// that accept JSON text should use parseWorkflowSpecText so text framing is
// also required to be canonical.
export async function parseWorkflowSpec(
  value: unknown,
): Promise<WorkflowSpec> {
  assertCanonicalFieldTree(value);

  const validate = await compileWorkflowSpecValidator();
  if (!validate(value)) {
    throw new WorkflowSpecError(
      `WorkflowSpec JSON schema violation ${formatAjvError(validate.errors)}`,
    );
  }

  const actualHash = computeSpecHash(value);
  if (actualHash !== value.specHash) {
    throw new WorkflowSpecError(
      `WorkflowSpec specHash mismatch: expected ${value.specHash}, got ${actualHash}`,
    );
  }

  return structuredClone(value);
}

// Parse the persisted WorkflowSpec representation. The exact text must be the
// canonical v0 JSON serialization, preventing whitespace and alternate JSON
// spellings from becoming interchangeable artifact bytes.
export async function parseWorkflowSpecText(
  text: string,
): Promise<WorkflowSpec> {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new WorkflowSpecError(`WorkflowSpec JSON parse failed: ${message}`);
  }

  try {
    assertCanonicalFieldTree(value);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new WorkflowSpecError(`WorkflowSpec is not canonical JSON: ${message}`);
  }
  if (stableStringify(value) !== text) {
    throw new WorkflowSpecError("WorkflowSpec is not canonical JSON");
  }

  return parseWorkflowSpec(value);
}

function compileWorkflowSpecValidator(): Promise<ValidateFunction<WorkflowSpec>> {
  workflowSpecValidator ??= compileSchema();
  return workflowSpecValidator;
}

async function compileSchema(): Promise<ValidateFunction<WorkflowSpec>> {
  const schema = JSON.parse(
    await readTextFile(
      new URL(
        "../../../conformance/schema/workflow-spec.schema.json",
        import.meta.url,
      ),
    ),
  ) as AnySchema;
  return new Ajv2020({ allErrors: true, strict: true }).compile<WorkflowSpec>(
    schema,
  );
}

async function readTextFile(path: URL): Promise<string> {
  const { readFile } = await import("node:fs/promises");
  return readFile(path, "utf8");
}

function assertCanonicalFieldTree(value: unknown): void {
  if (value === null || typeof value === "boolean") {
    return;
  }
  if (typeof value === "number") {
    if (!Number.isSafeInteger(value) || Object.is(value, -0)) {
      throw new Error("numbers must be safe integers");
    }
    return;
  }
  if (typeof value === "string") {
    assertWellFormedUnicode(value);
    return;
  }
  if (Array.isArray(value)) {
    for (const entry of value) {
      assertCanonicalFieldTree(entry);
    }
    return;
  }
  if (typeof value === "object") {
    for (const [key, entry] of Object.entries(value)) {
      assertWellFormedUnicode(key);
      assertCanonicalFieldTree(entry);
    }
    return;
  }
  throw new Error("must be a JSON field tree");
}

function assertWellFormedUnicode(value: string): void {
  for (let index = 0; index < value.length; index += 1) {
    const unit = value.charCodeAt(index);
    if (unit >= 0xd800 && unit <= 0xdbff) {
      const next = value.charCodeAt(index + 1);
      if (next >= 0xdc00 && next <= 0xdfff) {
        index += 1;
        continue;
      }
      throw new Error("strings must not contain lone surrogate code units");
    }
    if (unit >= 0xdc00 && unit <= 0xdfff) {
      throw new Error("strings must not contain lone surrogate code units");
    }
  }
}

function formatAjvError(
  errors: readonly ErrorObject[] | null | undefined,
): string {
  const error = errors?.[0];
  if (error === undefined) {
    return "at <root>: unknown validation error";
  }

  const location = error.instancePath === "" ? "<root>" : error.instancePath;
  if (
    error.keyword === "required" &&
    typeof error.params.missingProperty === "string"
  ) {
    return `${location}: missing required property "${error.params.missingProperty}"`;
  }

  return `${location}: ${error.message ?? "is invalid"}`;
}
