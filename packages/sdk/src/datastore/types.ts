import type { Key } from "./key.ts";

export interface PutOptions {
  readonly contentType?: string;
  readonly ifAbsent?: boolean;
}

export interface ObjectInfo {
  readonly key: Key;
  readonly size: number;
  readonly contentType: string;
}

export interface DatastoreObject {
  readonly info: ObjectInfo;
  readonly body: Uint8Array;
}

export interface DatastoreClient {
  put(
    key: Key,
    body: string | Uint8Array,
    options?: PutOptions,
  ): Promise<ObjectInfo>;
  get(key: Key): Promise<DatastoreObject>;
  exists(key: Key): Promise<boolean>;
  list(prefix: Key): Promise<ObjectInfo[]>;
}

export class DatastoreConflictError extends Error {
  constructor(key: Key) {
    super(`Datastore object already exists: ${key.toString()}`);
    this.name = "DatastoreConflictError";
  }
}

export class DatastoreNotFoundError extends Error {
  constructor(key: Key) {
    super(`Datastore object not found: ${key.toString()}`);
    this.name = "DatastoreNotFoundError";
  }
}

export function encodeBody(body: string | Uint8Array): Uint8Array {
  if (typeof body === "string") {
    return new TextEncoder().encode(body);
  }
  return body;
}

export function defaultContentType(contentType: string | undefined): string {
  return contentType ?? "application/octet-stream";
}
