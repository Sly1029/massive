import process from "node:process";
import {
  GetObjectCommand,
  HeadObjectCommand,
  ListObjectsV2Command,
  PutObjectCommand,
  S3Client,
} from "@aws-sdk/client-s3";
import { Key, validateObjectKey } from "./key.ts";
import {
  type DatastoreClient,
  DatastoreConflictError,
  DatastoreNotFoundError,
  type DatastoreObject,
  defaultContentType,
  encodeBody,
  type ObjectInfo,
  type PutOptions,
} from "./types.ts";

export interface S3Config {
  readonly endpoint?: string;
  readonly bucket: string;
  readonly region?: string;
  readonly prefix?: string;
  readonly forcePathStyle?: boolean;
  readonly credentials?: S3EnvironmentCredentials;
}

// The runner descriptor intentionally carries no credential bindings. This
// explicit configuration is for direct SDK callers such as MinIO fixtures;
// when omitted, the AWS SDK uses its default provider chain (for example an
// EKS workload-identity provider).
export interface S3EnvironmentCredentials {
  readonly kind: "environment";
  readonly accessKeyEnv: string;
  readonly secretAccessKeyEnv: string;
  readonly sessionTokenEnv?: string;
}

export class S3DatastoreClient implements DatastoreClient {
  private readonly client: S3Client;
  private readonly bucket: string;
  private readonly prefix: string;

  constructor(config: S3Config) {
    this.bucket = config.bucket;
    this.prefix = normalizePrefix(config.prefix ?? "");
    const credentials = credentialsFromEnvironment(config.credentials);
    this.client = new S3Client({
      ...(config.endpoint === undefined ? {} : { endpoint: config.endpoint }),
      region: config.region ?? "us-east-1",
      forcePathStyle: config.forcePathStyle ?? config.endpoint !== undefined,
      ...(credentials === undefined ? {} : { credentials }),
    });
  }

  async put(
    key: Key,
    body: string | Uint8Array,
    options: PutOptions = {},
  ): Promise<ObjectInfo> {
    const bytes = encodeBody(body);
    try {
      await this.client.send(
        new PutObjectCommand({
          Bucket: this.bucket,
          Key: this.objectName(key),
          Body: bytes,
          ContentType: defaultContentType(options.contentType),
          IfNoneMatch: options.ifAbsent === true ? "*" : undefined,
        }),
      );
    } catch (error) {
      if (isConflict(error)) {
        throw new DatastoreConflictError(key);
      }
      throw error;
    }

    return {
      key,
      size: bytes.byteLength,
      contentType: defaultContentType(options.contentType),
    };
  }

  async get(key: Key): Promise<DatastoreObject> {
    try {
      const output = await this.client.send(
        new GetObjectCommand({
          Bucket: this.bucket,
          Key: this.objectName(key),
        }),
      );
      const body = output.Body
        ? await output.Body.transformToByteArray()
        : new Uint8Array();
      return {
        info: {
          key,
          size: body.byteLength,
          contentType: defaultContentType(output.ContentType),
        },
        body,
      };
    } catch (error) {
      if (isNotFound(error)) {
        throw new DatastoreNotFoundError(key);
      }
      throw error;
    }
  }

  async exists(key: Key): Promise<boolean> {
    try {
      await this.client.send(
        new HeadObjectCommand({
          Bucket: this.bucket,
          Key: this.objectName(key),
        }),
      );
      return true;
    } catch (error) {
      if (isNotFound(error)) {
        return false;
      }
      throw error;
    }
  }

  async list(prefix: Key): Promise<ObjectInfo[]> {
    const objects: ObjectInfo[] = [];
    let continuationToken: string | undefined;
    do {
      const output = await this.client.send(
        new ListObjectsV2Command({
          Bucket: this.bucket,
          Prefix: `${this.objectName(prefix)}/`,
          ContinuationToken: continuationToken,
        }),
      );

      for (const object of output.Contents ?? []) {
        if (object.Key === undefined) {
          continue;
        }
        const key = Key.parse(object.Key.slice(this.prefix.length));
        const head = await this.client.send(
          new HeadObjectCommand({ Bucket: this.bucket, Key: object.Key }),
        );
        objects.push({
          key,
          size: object.Size ?? 0,
          contentType: defaultContentType(head.ContentType),
        });
      }

      continuationToken = output.NextContinuationToken;
    } while (continuationToken !== undefined);

    return objects.sort((left, right) =>
      left.key.toString().localeCompare(right.key.toString())
    );
  }

  private objectName(key: Key): string {
    return `${this.prefix}${key.toString()}`;
  }
}

function credentialsFromEnvironment(
  config: S3EnvironmentCredentials | undefined,
): {
  readonly accessKeyId: string;
  readonly secretAccessKey: string;
  readonly sessionToken?: string;
} | undefined {
  if (config === undefined) {
    return undefined;
  }

  const accessKeyId = process.env[config.accessKeyEnv];
  const secretAccessKey = process.env[config.secretAccessKeyEnv];
  if (
    accessKeyId === undefined || accessKeyId === "" ||
    secretAccessKey === undefined || secretAccessKey === ""
  ) {
    throw new Error(
      `S3 datastore environment credentials require ${config.accessKeyEnv} and ${config.secretAccessKeyEnv}`,
    );
  }

  const sessionToken = config.sessionTokenEnv === undefined
    ? undefined
    : process.env[config.sessionTokenEnv];
  return {
    accessKeyId,
    secretAccessKey,
    ...(sessionToken === undefined || sessionToken === ""
      ? {}
      : { sessionToken }),
  };
}

function normalizePrefix(prefix: string): string {
  if (prefix === "") {
    return "";
  }
  const trimmed = prefix.endsWith("/") ? prefix.slice(0, -1) : prefix;
  validateObjectKey(trimmed);
  return `${trimmed}/`;
}

function isNotFound(error: unknown): boolean {
  return isAwsError(error, 404) || isAwsNamedError(error, "NoSuchKey") ||
    isAwsNamedError(error, "NotFound");
}

function isConflict(error: unknown): boolean {
  return isAwsError(error, 412) || isAwsNamedError(error, "PreconditionFailed");
}

function isAwsError(error: unknown, statusCode: number): boolean {
  return typeof error === "object" && error !== null && "$metadata" in error &&
    typeof error.$metadata === "object" && error.$metadata !== null &&
    "httpStatusCode" in error.$metadata &&
    error.$metadata.httpStatusCode === statusCode;
}

function isAwsNamedError(error: unknown, name: string): boolean {
  return typeof error === "object" && error !== null && "name" in error &&
    error.name === name;
}
