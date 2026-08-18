import { Ajv2020 } from "ajv/dist/2020.js";
import type { AnySchema, ErrorObject } from "ajv/dist/2020.js";
import { ArtifactError, ArtifactRuntime } from "../artifact/runtime.ts";
import { Key } from "../datastore/key.ts";
import { LocalDatastoreClient } from "../datastore/local.ts";
import {
  type DatastoreClient,
  DatastoreNotFoundError,
} from "../datastore/types.ts";
import {
  CanonicalJsonError,
  decodeCanonicalUtf8,
  type JsonValue,
  parseCanonicalJsonText,
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
    const store = await datastoreForDescriptor(descriptor);
    const inputSchema = await readSchema(store, descriptor.input.schema);
    const outputSchema = await readSchema(store, descriptor.output.schema);
    const input = await readCanonicalJsonArtifact(
      store,
      descriptor.input.artifact.key,
      descriptor.input.artifact.hash,
    );

    validateJson(inputSchema, input, "input");

    const resolved = await resolveStepSymbol(descriptor, store);
    let output: unknown;
    try {
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

      const published = await new ArtifactRuntime(store).publishJson(
        {
          manifestKey: Key.parse(descriptor.output.manifestKey),
          schema: descriptor.output.schema,
        },
        {
          projectKey: descriptor.projectKey,
          planHash: descriptor.planHash,
          runId: descriptor.runId,
          nodeId: descriptor.nodeId,
          attempt: descriptor.attempt,
        },
        stableStringify(output),
      );

      return {
        kind: "success",
        exitCode: RUNNER_EXIT_CODES.success,
        runId: descriptor.runId,
        nodeId: descriptor.nodeId,
        attempt: descriptor.attempt,
        output: {
          manifest: published.manifest,
          body: published.body,
          schema: published.schema,
        },
      };
    } finally {
      await resolved.cleanup();
    }
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
    if (error instanceof ArtifactError || error instanceof CanonicalJsonError) {
      return schemaValidationFailure(
        new StepSchemaValidationError("output", error.message),
      );
    }

    // Datastore and runtime failures are infrastructure failures, not user
    // code failures. Let the process boundary surface them with its generic
    // non-user error path rather than misreporting exit 66.
    throw error;
  }
}

async function datastoreForDescriptor(
  descriptor: StepInvocationDescriptor,
): Promise<DatastoreClient> {
  if (descriptor.datastore.kind === "local") {
    return new LocalDatastoreClient({ path: descriptor.datastore.path });
  }

  try {
    const { S3DatastoreClient } = await import("../datastore/s3.ts");
    return new S3DatastoreClient({
      bucket: descriptor.datastore.bucket,
      region: descriptor.datastore.region,
      ...(descriptor.datastore.prefix === undefined
        ? {}
        : { prefix: descriptor.datastore.prefix }),
      ...(descriptor.datastore.endpoint === undefined
        ? {}
        : { endpoint: descriptor.datastore.endpoint }),
      ...(descriptor.datastore.forcePathStyle === undefined
        ? {}
        : { forcePathStyle: descriptor.datastore.forcePathStyle }),
    });
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new DescriptorError(`create S3 datastore: ${message}`);
  }
}

async function readSchema(
  store: DatastoreClient,
  schemaRef: string,
): Promise<JsonValue> {
  const key = schemaKey(schemaRef);
  let bytes: Uint8Array;
  try {
    bytes = (await store.get(Key.parse(key))).body;
  } catch (error) {
    if (!(error instanceof DatastoreNotFoundError)) throw error;
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
  let actualHash: string;
  try {
    actualHash = sha256RefText(stableStringify(schema));
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new StepSchemaValidationError(
      "schema",
      `schema ${schemaRef} is not canonical JSON: ${message}`,
    );
  }
  if (actualHash !== schemaRef) {
    throw new StepSchemaValidationError(
      "schema",
      `schema ${schemaRef} hash mismatch: got ${actualHash}`,
    );
  }

  return schema;
}

async function readCanonicalJsonArtifact(
  store: DatastoreClient,
  key: string,
  expectedHash: string,
): Promise<JsonValue> {
  let bytes: Uint8Array;
  try {
    bytes = (await store.get(Key.parse(key))).body;
  } catch (error) {
    if (!(error instanceof DatastoreNotFoundError)) throw error;
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

  const value = parseJsonBytes(
    bytes,
    "input",
    `input artifact ${key}`,
  ) as JsonValue;
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
  try {
    return parseJsonText(decodeCanonicalUtf8(bytes), boundary, role);
  } catch (error) {
    if (error instanceof StepSchemaValidationError) throw error;
    const message = error instanceof Error ? error.message : String(error);
    throw new StepSchemaValidationError(
      boundary,
      `${role} is not valid JSON: ${message}`,
    );
  }
}

function parseJsonText(
  text: string,
  boundary: "schema" | "input" | "output",
  role: string,
): unknown {
  try {
    return parseCanonicalJsonText(text);
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
