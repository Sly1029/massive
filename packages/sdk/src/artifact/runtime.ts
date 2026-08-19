import { Ajv2020 } from "ajv/dist/2020.js";
import type { AnySchema, ValidateFunction } from "ajv/dist/2020.js";
import { z } from "zod";
import manifestSchema from "./data-artifact-manifest.schema.json" with {
  type: "json",
};
import { blobKeySHA256Hex, Key } from "../datastore/key.ts";
import {
  type DatastoreClient,
  DatastoreConflictError,
  DatastoreNotFoundError,
  type ObjectInfo,
} from "../datastore/types.ts";
import {
  decodeCanonicalUtf8,
  type JsonValue,
  parseCanonicalJsonText,
  sha256RefBytes,
  stableStringify,
} from "../stable.ts";

export const JSON_CONTENT_TYPE = "application/json";
export const MANIFEST_CONTENT_TYPE =
  "application/vnd.massive.data-artifact-manifest+json";

export interface ArtifactDestination {
  readonly manifestKey: Key;
  readonly schema: string;
}

export interface ArtifactProducer {
  readonly projectKey: string;
  readonly planHash: string;
  readonly runId: string;
  readonly nodeId: string;
  readonly attempt: number;
  readonly scope?: ExecutionScope;
}

export interface MapItemScopeFrame {
  readonly kind: "map-item";
  readonly mapId: string;
  readonly index: number;
}

// Frames are ordered outer-to-inner. The optional wrapper leaves the static
// invocation identity unchanged while making nested map identities explicit.
export interface ExecutionScope {
  readonly frames: readonly MapItemScopeFrame[];
}

const hashRefSchema = z
  .string()
  .regex(/^sha256:[0-9a-f]{64}$/, "must be a lowercase SHA-256 reference");

const projectKeySchema = z
  .string()
  .regex(
    /^sha256-[0-9a-f]{64}$/,
    "must be a normalized SHA-256 project namespace key",
  );

const pathSegmentSchema = z
  .string()
  .min(1, "must not be empty")
  .max(128, "must be at most 128 characters")
  .regex(
    /^[A-Za-z0-9_.@:#-]+$/,
    "must contain only safe ASCII path-segment characters",
  )
  .refine((value) => value !== "." && value !== "..", {
    message: "must not be a dot path segment",
  });

const mapItemScopeFrameSchema = z.object({
  kind: z.literal("map-item"),
  mapId: pathSegmentSchema,
  index: z.number().int().min(0).max(Number.MAX_SAFE_INTEGER),
}).strict();

const executionScopeSchema = z.object({
  frames: z.array(mapItemScopeFrameSchema).min(1),
}).strict();

// The producer is the namespace identity for an immutable output slot. Keep
// its parsing in one schema rather than relying on Key.parse to accidentally
// reject only some unsafe values after work has already begun.
const artifactProducerSchema = z
  .object({
    projectKey: projectKeySchema,
    planHash: hashRefSchema,
    runId: pathSegmentSchema,
    nodeId: pathSegmentSchema,
    attempt: z
      .number()
      .int("must be an integer")
      .positive("must be positive")
      .max(Number.MAX_SAFE_INTEGER, "must be a safe integer"),
    scope: executionScopeSchema.optional(),
  })
  .strict();

const artifactDestinationSchema = z
  .object({
    manifestKey: z.custom<Key>((value): value is Key => value instanceof Key, {
      error: "must be a validated datastore key",
    }),
    schema: hashRefSchema,
  })
  .strict();

export interface ArtifactRef {
  readonly key: string;
  readonly hash: string;
  readonly size: number;
  readonly contentType: string;
}

export interface PublishedJson {
  readonly manifest: ArtifactRef;
  readonly body: ArtifactRef;
  readonly schema: string;
}

export interface ResolvedJson {
  readonly published: PublishedJson;
  readonly body: Uint8Array;
}

interface DataArtifactManifest {
  readonly kind: "DataArtifactManifest";
  readonly schemaVersion: 1;
  readonly encoding: "canonical-json-v0";
  readonly producer: ArtifactProducer;
  readonly schema: string;
  readonly body: ArtifactRef;
}

export class ArtifactError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = "ArtifactError";
  }
}

export class ArtifactValidationError extends ArtifactError {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = "ArtifactValidationError";
  }
}

export class ArtifactIntegrityError extends ArtifactError {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = "ArtifactIntegrityError";
  }
}

export class ArtifactBodyConflictError extends ArtifactError {
  constructor(readonly key: Key) {
    super(`Artifact body conflict at ${key.toString()}`);
    this.name = "ArtifactBodyConflictError";
  }
}

export class ArtifactManifestConflictError extends ArtifactError {
  constructor(readonly key: Key) {
    super(`Artifact manifest conflict at ${key.toString()}`);
    this.name = "ArtifactManifestConflictError";
  }
}

export class ArtifactNotFoundError extends ArtifactError {
  constructor(readonly key: Key) {
    super(`Artifact object not found: ${key.toString()}`);
    this.name = "ArtifactNotFoundError";
  }
}

// ArtifactRuntime is deliberately the only seam that commits a produced JSON
// value. Callers provide the deterministic attempt destination and canonical
// value; this module owns body-first publication, immutable-manifest recovery,
// and all validation required before a value becomes visible downstream.
export class ArtifactRuntime {
  constructor(private readonly store: DatastoreClient) {}

  validateDestination(
    destination: ArtifactDestination,
    producer: ArtifactProducer,
  ): void {
    validateDestination(destination, validateProducer(producer));
  }

  async publishJson(
    destination: ArtifactDestination,
    producer: ArtifactProducer,
    body: string | Uint8Array,
  ): Promise<PublishedJson> {
    const validatedProducer = validateProducer(producer);
    validateDestination(destination, validatedProducer);
    const bytes = encode(body);
    await this.validateCanonicalJson(destination.schema, bytes);

    const bodyHash = sha256RefBytes(bytes);
    const bodyKey = keyForHash(bodyHash);
    const bodyRef: ArtifactRef = {
      key: bodyKey.toString(),
      hash: bodyHash,
      size: bytes.byteLength,
      contentType: JSON_CONTENT_TYPE,
    };
    const manifest: DataArtifactManifest = {
      kind: "DataArtifactManifest",
      schemaVersion: 1,
      encoding: "canonical-json-v0",
      producer: validatedProducer,
      schema: destination.schema,
      body: bodyRef,
    };
    const manifestBytes = await canonicalManifest(manifest);

    await putImmutable(
      this.store,
      bodyKey,
      bytes,
      JSON_CONTENT_TYPE,
      (key) => new ArtifactBodyConflictError(key),
    );
    await putImmutable(
      this.store,
      destination.manifestKey,
      manifestBytes,
      MANIFEST_CONTENT_TYPE,
      (key) => new ArtifactManifestConflictError(key),
    );

    return publishedJson(
      destination.manifestKey,
      manifestBytes,
      bodyRef,
      destination.schema,
    );
  }

  async resolveJson(
    destination: ArtifactDestination,
    producer: ArtifactProducer,
  ): Promise<ResolvedJson> {
    const validatedProducer = validateProducer(producer);
    validateDestination(destination, validatedProducer);
    const manifestObject = await getArtifactObject(
      this.store,
      destination.manifestKey,
    );
    if (manifestObject.info.contentType !== MANIFEST_CONTENT_TYPE) {
      throw new ArtifactIntegrityError(
        `Manifest ${destination.manifestKey.toString()} has content type ${manifestObject.info.contentType}`,
      );
    }

    const manifest = await decodeManifest(
      manifestObject.body,
      destination.manifestKey,
    );
    if (
      !sameProducer(manifest.producer, validatedProducer) ||
      manifest.schema !== destination.schema
    ) {
      throw new ArtifactIntegrityError(
        `Manifest ${destination.manifestKey.toString()} does not match its expected producer and schema`,
      );
    }

    const bodyKey = keyForHash(manifest.body.hash);
    if (bodyKey.toString() !== manifest.body.key) {
      throw new ArtifactIntegrityError(
        `Manifest ${destination.manifestKey.toString()} body key does not match its digest`,
      );
    }
    const bodyObject = await getArtifactObject(this.store, bodyKey);
    if (
      bodyObject.info.contentType !== JSON_CONTENT_TYPE ||
      bodyObject.body.byteLength !== manifest.body.size ||
      sha256RefBytes(bodyObject.body) !== manifest.body.hash
    ) {
      throw new ArtifactIntegrityError(
        `Body ${bodyKey.toString()} does not match its manifest`,
      );
    }
    try {
      await this.validateCanonicalJson(manifest.schema, bodyObject.body);
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      throw new ArtifactIntegrityError(
        `Body ${bodyKey.toString()} is invalid: ${message}`,
        { cause: error },
      );
    }

    return {
      published: publishedJson(
        destination.manifestKey,
        manifestObject.body,
        manifest.body,
        manifest.schema,
      ),
      body: bodyObject.body,
    };
  }

  private async validateCanonicalJson(
    schemaRef: string,
    body: Uint8Array,
  ): Promise<void> {
    const value = parseCanonicalJson(body, "value");
    const schemaKey = keyForHash(schemaRef);
    let schemaObject;
    try {
      schemaObject = await this.store.get(schemaKey);
    } catch (error) {
      if (!(error instanceof DatastoreNotFoundError)) {
        throw error;
      }
      throw new ArtifactValidationError(
        `Cannot read schema ${schemaRef}: ${
          error instanceof Error ? error.message : String(error)
        }`,
        { cause: error },
      );
    }
    const schema = parseCanonicalJson(schemaObject.body, "schema");
    if (sha256RefBytes(schemaObject.body) !== schemaRef) {
      throw new ArtifactValidationError(
        `Schema ${schemaRef} is not stored under its digest`,
      );
    }

    let validate: ValidateFunction;
    try {
      validate = new Ajv2020({ allErrors: true, strict: true }).compile(
        schema as AnySchema,
      );
    } catch (error) {
      throw new ArtifactValidationError(
        `Schema ${schemaRef} could not be compiled: ${
          error instanceof Error ? error.message : String(error)
        }`,
        { cause: error },
      );
    }
    if (!validate(value)) {
      throw new ArtifactValidationError(
        `Value does not satisfy schema ${schemaRef}: ${
          formatValidationError(validate)
        }`,
      );
    }
  }
}

function validateDestination(
  destination: ArtifactDestination,
  producer: ArtifactProducer,
): void {
  const destinationResult = artifactDestinationSchema.safeParse(destination);
  if (!destinationResult.success) {
    throw new ArtifactValidationError(
      `Invalid artifact destination: ${
        z.prettifyError(destinationResult.error)
      }`,
      { cause: destinationResult.error },
    );
  }
  let expected: Key;
  try {
    expected = Key.parse(
      `projects/${producer.projectKey}/runs/${producer.runId}/steps/${producer.nodeId}${
        scopeKeySuffix(producer.scope)
      }/${producer.attempt}/output-manifest.json`,
    );
  } catch (error) {
    throw new ArtifactValidationError(
      `Invalid producer identity: ${
        error instanceof Error ? error.message : String(error)
      }`,
      { cause: error },
    );
  }
  if (destination.manifestKey.toString() !== expected.toString()) {
    throw new ArtifactValidationError(
      `Manifest destination ${destination.manifestKey.toString()} does not match producer slot ${expected.toString()}`,
    );
  }
}

function validateProducer(producer: unknown): ArtifactProducer {
  const result = artifactProducerSchema.safeParse(producer);
  if (!result.success) {
    throw new ArtifactValidationError(
      `Invalid producer identity: ${z.prettifyError(result.error)}`,
      { cause: result.error },
    );
  }
  return result.data;
}

async function putImmutable(
  store: DatastoreClient,
  key: Key,
  body: Uint8Array,
  contentType: string,
  conflict: (key: Key) => Error,
): Promise<ObjectInfo> {
  try {
    return await store.put(key, body, { contentType, ifAbsent: true });
  } catch (error) {
    if (!(error instanceof DatastoreConflictError)) {
      throw error;
    }
    const existing = await store.get(key);
    if (
      existing.info.contentType !== contentType ||
      !sameBytes(existing.body, body)
    ) {
      throw conflict(key);
    }
    return existing.info;
  }
}

async function canonicalManifest(
  manifest: DataArtifactManifest,
): Promise<Uint8Array> {
  const body = encode(stableStringify(manifest));
  try {
    await validateManifest(body);
  } catch (error) {
    throw new ArtifactValidationError(
      `Artifact manifest is invalid: ${
        error instanceof Error ? error.message : String(error)
      }`,
      { cause: error },
    );
  }
  return body;
}

async function decodeManifest(
  body: Uint8Array,
  key: Key,
): Promise<DataArtifactManifest> {
  let value: JsonValue;
  try {
    value = parseCanonicalJson(body, `manifest ${key.toString()}`);
  } catch (error) {
    throw new ArtifactIntegrityError(
      `Manifest ${key.toString()} is not canonical JSON: ${
        error instanceof Error ? error.message : String(error)
      }`,
      { cause: error },
    );
  }
  try {
    await validateManifest(body);
  } catch (error) {
    throw new ArtifactIntegrityError(
      `Manifest ${key.toString()} is invalid: ${
        error instanceof Error ? error.message : String(error)
      }`,
      { cause: error },
    );
  }
  return value as unknown as DataArtifactManifest;
}

let manifestValidator: Promise<ValidateFunction> | undefined;

function validateManifest(body: Uint8Array): Promise<void> {
  // Manifest JSON is also an immutable, hashed wire artifact. Parsing through
  // the canonical boundary before AJV prevents a second permissive JSON.parse
  // path from accepting whitespace or normalized numeric spellings.
  const value = parseCanonicalJson(body, "manifest");
  manifestValidator ??= compileManifestValidator();
  return manifestValidator.then((validate) => {
    if (!validate(value)) {
      throw new Error(formatValidationError(validate));
    }
  });
}

async function compileManifestValidator(): Promise<ValidateFunction> {
  return new Ajv2020({ allErrors: true, strict: true }).compile(
    manifestSchema as AnySchema,
  );
}

async function getArtifactObject(
  store: DatastoreClient,
  key: Key,
) {
  try {
    return await store.get(key);
  } catch (error) {
    if (error instanceof DatastoreNotFoundError) {
      throw new ArtifactNotFoundError(key);
    }
    throw error;
  }
}

function parseCanonicalJson(body: Uint8Array, role: string): JsonValue {
  try {
    return parseCanonicalJsonText(decodeCanonicalUtf8(body));
  } catch (error) {
    throw new ArtifactValidationError(
      `${role} is not canonical JSON: ${
        error instanceof Error ? error.message : String(error)
      }`,
      { cause: error },
    );
  }
}

function publishedJson(
  manifestKey: Key,
  manifestBody: Uint8Array,
  body: ArtifactRef,
  schema: string,
): PublishedJson {
  return {
    manifest: {
      key: manifestKey.toString(),
      hash: sha256RefBytes(manifestBody),
      size: manifestBody.byteLength,
      contentType: MANIFEST_CONTENT_TYPE,
    },
    body,
    schema,
  };
}

function keyForHash(hash: string): Key {
  if (!hash.startsWith("sha256:")) {
    throw new ArtifactValidationError(
      `Invalid SHA-256 reference ${JSON.stringify(hash)}`,
    );
  }
  try {
    return blobKeySHA256Hex(hash.slice("sha256:".length));
  } catch (error) {
    throw new ArtifactValidationError(
      `Invalid SHA-256 reference ${JSON.stringify(hash)}`,
      { cause: error },
    );
  }
}

function sameProducer(
  left: ArtifactProducer,
  right: ArtifactProducer,
): boolean {
  return left.projectKey === right.projectKey &&
    left.planHash === right.planHash &&
    left.runId === right.runId &&
    left.nodeId === right.nodeId &&
    left.attempt === right.attempt &&
    sameScope(left.scope, right.scope);
}

function sameScope(
  left: ExecutionScope | undefined,
  right: ExecutionScope | undefined,
): boolean {
  if (left === undefined || right === undefined) return left === right;
  return left.frames.length === right.frames.length &&
    left.frames.every((frame, index) => {
      const other = right.frames[index];
      return other !== undefined && frame.kind === other.kind &&
        frame.mapId === other.mapId && frame.index === other.index;
    });
}

function scopeKeySuffix(scope: ExecutionScope | undefined): string {
  if (scope === undefined) return "";
  return "/scopes" +
    scope.frames.map((frame) => `/maps/${frame.mapId}/items/${frame.index}`)
      .join("");
}

function sameBytes(left: Uint8Array, right: Uint8Array): boolean {
  if (left.byteLength !== right.byteLength) {
    return false;
  }
  for (let index = 0; index < left.byteLength; index += 1) {
    if (left[index] !== right[index]) {
      return false;
    }
  }
  return true;
}

function encode(body: string | Uint8Array): Uint8Array {
  return typeof body === "string" ? new TextEncoder().encode(body) : body;
}

function formatValidationError(validate: ValidateFunction): string {
  const error = validate.errors?.[0];
  if (error === undefined) {
    return "unknown JSON Schema violation";
  }
  const location = error.instancePath === "" ? "<root>" : error.instancePath;
  return `${location}: ${error.message ?? "is invalid"}`;
}
