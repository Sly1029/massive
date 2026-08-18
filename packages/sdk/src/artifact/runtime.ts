import { Ajv2020 } from "ajv/dist/2020.js";
import type { AnySchema, ValidateFunction } from "ajv/dist/2020.js";
import { blobKeySHA256Hex, Key } from "../datastore/key.ts";
import {
  type DatastoreClient,
  DatastoreConflictError,
  type ObjectInfo,
} from "../datastore/types.ts";
import { type JsonValue, sha256RefBytes, stableStringify } from "../stable.ts";

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
}

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
  readonly schemaVersion: 0;
  readonly encoding: "canonical-json-v0";
  readonly producer: ArtifactProducer;
  readonly schema: string;
  readonly body: ArtifactRef;
}

export class ArtifactValidationError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = "ArtifactValidationError";
  }
}

export class ArtifactIntegrityError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = "ArtifactIntegrityError";
  }
}

export class ArtifactBodyConflictError extends Error {
  constructor(readonly key: Key) {
    super(`Artifact body conflict at ${key.toString()}`);
    this.name = "ArtifactBodyConflictError";
  }
}

export class ArtifactManifestConflictError extends Error {
  constructor(readonly key: Key) {
    super(`Artifact manifest conflict at ${key.toString()}`);
    this.name = "ArtifactManifestConflictError";
  }
}

// ArtifactRuntime is deliberately the only seam that commits a produced JSON
// value. Callers provide the deterministic attempt destination and canonical
// value; this module owns body-first publication, immutable-manifest recovery,
// and all validation required before a value becomes visible downstream.
export class ArtifactRuntime {
  constructor(private readonly store: DatastoreClient) {}

  async publishJson(
    destination: ArtifactDestination,
    producer: ArtifactProducer,
    body: string | Uint8Array,
  ): Promise<PublishedJson> {
    validateDestination(destination, producer);
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
      schemaVersion: 0,
      encoding: "canonical-json-v0",
      producer,
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
    validateDestination(destination, producer);
    const manifestObject = await this.store.get(destination.manifestKey);
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
      !sameProducer(manifest.producer, producer) ||
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
    const bodyObject = await this.store.get(bodyKey);
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
  let expected: Key;
  try {
    expected = Key.parse(
      `projects/${producer.projectKey}/runs/${producer.runId}/steps/${producer.nodeId}/${producer.attempt}/output-manifest.json`,
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
    let existing;
    try {
      existing = await store.get(key);
    } catch (readError) {
      throw conflict(key);
    }
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
  manifestValidator ??= compileManifestValidator();
  return manifestValidator.then((validate) => {
    const value = JSON.parse(new TextDecoder().decode(body));
    if (!validate(value)) {
      throw new Error(formatValidationError(validate));
    }
  });
}

async function compileManifestValidator(): Promise<ValidateFunction> {
  const { readFile } = await import("node:fs/promises");
  const schema = JSON.parse(
    await readFile(
      new URL(
        "../../../../conformance/schema/data-artifact-manifest.schema.json",
        import.meta.url,
      ),
      "utf8",
    ),
  ) as AnySchema;
  return new Ajv2020({ allErrors: true, strict: true }).compile(schema);
}

function parseCanonicalJson(body: Uint8Array, role: string): JsonValue {
  const text = new TextDecoder().decode(body);
  let value: JsonValue;
  try {
    value = JSON.parse(text) as JsonValue;
  } catch (error) {
    throw new ArtifactValidationError(
      `${role} is not valid JSON: ${
        error instanceof Error ? error.message : String(error)
      }`,
      { cause: error },
    );
  }
  if (stableStringify(value) !== text) {
    throw new ArtifactValidationError(`${role} is not canonical JSON`);
  }
  return value;
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
    left.attempt === right.attempt;
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
