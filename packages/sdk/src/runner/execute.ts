import { Ajv2020 } from "ajv/dist/2020.js";
import type { AnySchema, ErrorObject } from "ajv/dist/2020.js";
import { type Datastore, datastore } from "../datastore/facade.ts";
import {
  type JsonValue,
  sha256RefBytes,
  sha256RefText,
  stableStringify,
} from "../stable.ts";
import type { StepInvocationDescriptor } from "./descriptor.ts";
import {
  DescriptorError,
  descriptorResolutionFailure,
  RUNNER_EXIT_CODES,
  schemaValidationFailure,
  StepExecutionError,
  stepExecutionFailure,
  type StepOutcome,
  StepSchemaValidationError,
  SymbolResolutionError,
} from "./outcomes.ts";
import { resolveStepSymbol } from "./source.ts";

export async function executeStep(
  descriptor: StepInvocationDescriptor,
): Promise<StepOutcome> {
  try {
    const store = datastoreForDescriptor(descriptor);
    const inputSchema = await readSchema(store, descriptor.input.schema);
    const outputSchema = await readSchema(store, descriptor.output.schema);
    const input = await readCanonicalJsonArtifact(
      store,
      descriptor.input.artifact.key,
      descriptor.input.artifact.hash,
    );

    validateJson(inputSchema, input, "input");

    const resolved = await resolveStepSymbol(descriptor);
    let output: unknown;
    try {
      output = await resolved.run({
        input,
        state: {},
        context: {
          runId: descriptor.runId,
          stepId: descriptor.nodeId,
        },
      });
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      throw new StepExecutionError(message);
    }

    validateJson(outputSchema, output, "output");

    const serializedOutput = stableStringify(output);
    const outputHash = sha256RefText(serializedOutput);
    await store.put(descriptor.output.artifact.key, serializedOutput, {
      contentType: descriptor.output.artifact.contentType,
    });

    return {
      kind: "success",
      exitCode: RUNNER_EXIT_CODES.success,
      runId: descriptor.runId,
      nodeId: descriptor.nodeId,
      attempt: descriptor.attempt,
      output: {
        key: descriptor.output.artifact.key,
        hash: outputHash,
        contentType: descriptor.output.artifact.contentType,
        schema: descriptor.output.schema,
      },
    };
  } catch (error) {
    if (
      error instanceof DescriptorError || error instanceof SymbolResolutionError
    ) {
      return descriptorResolutionFailure(error);
    }
    if (error instanceof StepSchemaValidationError) {
      return schemaValidationFailure(error);
    }
    if (error instanceof StepExecutionError) {
      return stepExecutionFailure(error);
    }

    const message = error instanceof Error ? error.message : String(error);
    return stepExecutionFailure(new StepExecutionError(message));
  }
}

function datastoreForDescriptor(
  descriptor: StepInvocationDescriptor,
): Datastore {
  if (descriptor.datastore.kind !== "local") {
    throw new SymbolResolutionError(
      "only local datastores are supported by the v0 TypeScript runner",
    );
  }

  return datastore.local({ path: descriptor.datastore.path });
}

async function readSchema(
  store: Datastore,
  schemaRef: string,
): Promise<JsonValue> {
  const key = schemaKey(schemaRef);
  let bytes: Uint8Array;
  try {
    bytes = await store.get(key);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new StepSchemaValidationError(
      "schema",
      `schema ${schemaRef} could not be read from ${key}: ${message}`,
    );
  }

  const schema = parseJsonBytes(
    bytes,
    "schema",
    `schema ${schemaRef}`,
  ) as JsonValue;
  const actualHash = sha256RefText(stableStringify(schema));
  if (actualHash !== schemaRef) {
    throw new StepSchemaValidationError(
      "schema",
      `schema ${schemaRef} hash mismatch: got ${actualHash}`,
    );
  }

  return schema;
}

async function readCanonicalJsonArtifact(
  store: Datastore,
  key: string,
  expectedHash: string,
): Promise<JsonValue> {
  let bytes: Uint8Array;
  try {
    bytes = await store.get(key);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new StepSchemaValidationError(
      "input",
      `input artifact ${key} could not be read: ${message}`,
    );
  }

  const actualHash = sha256RefBytes(bytes);
  if (actualHash !== expectedHash) {
    throw new StepSchemaValidationError(
      "input",
      `input artifact ${key} hash mismatch: expected ${expectedHash}, got ${actualHash}`,
    );
  }

  const text = new TextDecoder().decode(bytes);
  const value = parseJsonText(
    text,
    "input",
    `input artifact ${key}`,
  ) as JsonValue;
  if (stableStringify(value) !== text) {
    throw new StepSchemaValidationError(
      "input",
      `input artifact ${key} is not canonical JSON`,
    );
  }

  return value;
}

function validateJson(
  schema: JsonValue,
  value: unknown,
  role: "input" | "output",
): void {
  let validate;
  try {
    validate = new Ajv2020({ allErrors: true, strict: true }).compile(
      schema as AnySchema,
    );
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new StepSchemaValidationError(
      "schema",
      `${role} schema could not be compiled: ${message}`,
    );
  }

  if (!validate(value)) {
    throw new StepSchemaValidationError(role, formatAjvError(validate.errors));
  }
}

function schemaKey(schemaRef: string): string {
  return `blobs/sha256/${schemaRef.slice("sha256:".length)}`;
}

function parseJsonBytes(
  bytes: Uint8Array,
  boundary: "schema" | "input" | "output",
  role: string,
): unknown {
  return parseJsonText(new TextDecoder().decode(bytes), boundary, role);
}

function parseJsonText(
  text: string,
  boundary: "schema" | "input" | "output",
  role: string,
): unknown {
  try {
    return JSON.parse(text);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new StepSchemaValidationError(
      boundary,
      `${role} is not valid JSON: ${message}`,
    );
  }
}

function formatAjvError(
  errors: readonly ErrorObject[] | null | undefined,
): string {
  const error = errors?.[0];
  if (error === undefined) {
    return "unknown JSON Schema violation";
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
