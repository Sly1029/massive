import { Ajv2020 } from "ajv/dist/2020.js";
import type {
  AnySchema,
  ErrorObject,
  ValidateFunction,
} from "ajv/dist/2020.js";
import { DescriptorError } from "./outcomes.ts";

export type HashRef = `sha256:${string}`;
export type StepLanguage = "typescript" | "python";

export interface StepSymbolRef {
  readonly packageId: string;
  readonly language: StepLanguage;
  readonly module: string;
  readonly export: string;
}

export interface ArtifactRef {
  readonly key: string;
  readonly hash: HashRef;
  readonly contentType: string;
}

export interface ArtifactDestination {
  readonly key: string;
  readonly contentType: string;
}

export interface DataArtifactRef {
  readonly artifact: ArtifactRef;
  readonly schema: HashRef;
}

export interface DataArtifactDestination {
  readonly artifact: ArtifactDestination;
  readonly schema: HashRef;
}

export interface ChannelArtifactRef {
  readonly channelName: string;
  readonly artifact: ArtifactRef;
  readonly schema: HashRef;
}

export interface ChannelArtifactDestination {
  readonly channelName: string;
  readonly artifact: ArtifactDestination;
  readonly schema: HashRef;
}

export interface SourcePackageRef {
  readonly packageId: string;
  readonly language: StepLanguage;
  readonly packageHash: HashRef;
  readonly sourceArchive: ArtifactRef;
  readonly manifest?: ArtifactRef;
}

export interface LocalDatastoreDescriptor {
  readonly kind: "local";
  readonly path: string;
}

export interface S3DatastoreDescriptor {
  readonly kind: "s3";
  readonly bucket: string;
  readonly region: string;
  readonly prefix?: string;
  readonly endpoint?: string;
}

export type DatastoreDescriptor =
  | LocalDatastoreDescriptor
  | S3DatastoreDescriptor;

export interface StepInvocationDescriptor {
  readonly kind: "StepInvocationDescriptor";
  readonly schemaVersion: 0;
  readonly encoding: "json-v0";
  readonly planHash: HashRef;
  readonly runId: string;
  readonly nodeId: string;
  readonly attempt: number;
  readonly symbol: StepSymbolRef;
  readonly sourcePackage: SourcePackageRef;
  readonly environmentRef: HashRef;
  readonly input: DataArtifactRef;
  readonly output: DataArtifactDestination;
  readonly channelReads: readonly ChannelArtifactRef[];
  readonly channelWrites: readonly ChannelArtifactDestination[];
  readonly datastore: DatastoreDescriptor;
}

let descriptorValidator: Promise<ValidateFunction> | undefined;

export async function parseStepInvocationDescriptor(
  value: unknown,
): Promise<StepInvocationDescriptor> {
  const validate = await compileStepInvocationDescriptorValidator();
  if (!validate(value)) {
    throw new DescriptorError(
      `StepInvocationDescriptor JSON schema violation ${
        formatAjvError(validate.errors)
      }`,
    );
  }

  const descriptor = value as StepInvocationDescriptor;
  return {
    kind: descriptor.kind,
    schemaVersion: descriptor.schemaVersion,
    encoding: descriptor.encoding,
    planHash: descriptor.planHash,
    runId: descriptor.runId,
    nodeId: descriptor.nodeId,
    attempt: descriptor.attempt,
    symbol: { ...descriptor.symbol },
    sourcePackage: {
      packageId: descriptor.sourcePackage.packageId,
      language: descriptor.sourcePackage.language,
      packageHash: descriptor.sourcePackage.packageHash,
      sourceArchive: { ...descriptor.sourcePackage.sourceArchive },
      ...(descriptor.sourcePackage.manifest === undefined
        ? {}
        : { manifest: { ...descriptor.sourcePackage.manifest } }),
    },
    environmentRef: descriptor.environmentRef,
    input: {
      artifact: { ...descriptor.input.artifact },
      schema: descriptor.input.schema,
    },
    output: {
      artifact: { ...descriptor.output.artifact },
      schema: descriptor.output.schema,
    },
    channelReads: [...(descriptor.channelReads ?? [])].map((channel) => ({
      channelName: channel.channelName,
      artifact: { ...channel.artifact },
      schema: channel.schema,
    })),
    channelWrites: [...(descriptor.channelWrites ?? [])].map((channel) => ({
      channelName: channel.channelName,
      artifact: { ...channel.artifact },
      schema: channel.schema,
    })),
    datastore: decodeDatastore(descriptor.datastore),
  };
}

export async function parseStepInvocationDescriptorText(
  text: string,
): Promise<StepInvocationDescriptor> {
  try {
    return await parseStepInvocationDescriptor(JSON.parse(text));
  } catch (error) {
    if (error instanceof DescriptorError) {
      throw error;
    }

    const message = error instanceof Error ? error.message : String(error);
    throw new DescriptorError(
      `StepInvocationDescriptor JSON parse failed: ${message}`,
    );
  }
}

function decodeDatastore(datastore: DatastoreDescriptor): DatastoreDescriptor {
  if (datastore.kind === "local") {
    return { kind: "local", path: datastore.path };
  }

  return {
    kind: "s3",
    bucket: datastore.bucket,
    region: datastore.region,
    ...(datastore.prefix === undefined ? {} : { prefix: datastore.prefix }),
    ...(datastore.endpoint === undefined
      ? {}
      : { endpoint: datastore.endpoint }),
  };
}

function compileStepInvocationDescriptorValidator(): Promise<ValidateFunction> {
  descriptorValidator ??= compileDescriptorSchema();
  return descriptorValidator;
}

async function compileDescriptorSchema(): Promise<ValidateFunction> {
  const ajv = new Ajv2020({ allErrors: true, strict: true });
  const schema = JSON.parse(
    await readTextFile(
      new URL(
        "../../../../conformance/schema/step-invocation-descriptor.schema.json",
        import.meta.url,
      ),
    ),
  ) as AnySchema;
  return ajv.compile(schema);
}

async function readTextFile(path: URL): Promise<string> {
  const { readFile } = await import("node:fs/promises");
  return readFile(path, "utf8");
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
    return `at ${location}: missing required property "${error.params.missingProperty}"`;
  }

  const message = error.message ?? "is invalid";
  return `at ${location}: ${message}`;
}
