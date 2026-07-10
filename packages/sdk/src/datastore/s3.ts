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
  readonly endpoint: string;
  readonly bucket: string;
  readonly region?: string;
  readonly prefix?: string;
  readonly forcePathStyle?: boolean;
  readonly accessKeyEnv?: string;
  readonly secretAccessKeyEnv?: string;
  readonly sessionTokenEnv?: string;
}

export class S3DatastoreClient implements DatastoreClient {
  private readonly client: S3Client;
  private readonly bucket: string;
  private readonly prefix: string;

  constructor(config: S3Config) {
    const accessKeyEnv = config.accessKeyEnv ?? "AWS_ACCESS_KEY_ID";
    const secretAccessKeyEnv = config.secretAccessKeyEnv ??
      "AWS_SECRET_ACCESS_KEY";
    const sessionTokenEnv = config.sessionTokenEnv ?? "AWS_SESSION_TOKEN";
    const accessKeyId = process.env[accessKeyEnv];
    const secretAccessKey = process.env[secretAccessKeyEnv];

    if (accessKeyId === undefined || secretAccessKey === undefined) {
      throw new Error(
        `S3 datastore credentials require ${accessKeyEnv} and ${secretAccessKeyEnv}`,
      );
    }

    this.bucket = config.bucket;
    this.prefix = normalizePrefix(config.prefix ?? "");
    this.client = new S3Client({
      endpoint: config.endpoint,
      region: config.region ?? "us-east-1",
      forcePathStyle: config.forcePathStyle ?? true,
      credentials: {
        accessKeyId,
        secretAccessKey,
        ...(process.env[sessionTokenEnv] === undefined
          ? {}
          : { sessionToken: process.env[sessionTokenEnv] }),
      },
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
