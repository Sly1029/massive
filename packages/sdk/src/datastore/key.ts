import { createHash } from "node:crypto";
import { DatastoreKeyError } from "../errors.ts";

const DIGEST_HEX_PATTERN = /^[0-9a-f]{64}$/;

export class Key {
  readonly value: string;

  private constructor(value: string) {
    this.value = value;
  }

  static parse(value: string): Key {
    validateObjectKey(value);
    return new Key(value);
  }

  toString(): string {
    return this.value;
  }
}

export function blobKeySHA256Hex(digestHex: string): Key {
  if (!DIGEST_HEX_PATTERN.test(digestHex)) {
    throw new DatastoreKeyError(
      digestHex,
      "blob digest must be 64 lowercase hex characters",
    );
  }
  return Key.parse(`blobs/sha256/${digestHex}`);
}

export async function blobKeyForBytes(body: Uint8Array): Promise<Key> {
  return blobKeySHA256Hex(createHash("sha256").update(body).digest("hex"));
}

export function validateObjectKey(key: string): void {
  if (key.length === 0) {
    throw new DatastoreKeyError(key, "key cannot be empty");
  }
  if (key.startsWith("/") || isWindowsAbsolute(key)) {
    throw new DatastoreKeyError(key, "key cannot be absolute");
  }
  if (key.includes("\\")) {
    throw new DatastoreKeyError(key, "key must use forward slashes");
  }

  for (const segment of key.split("/")) {
    if (segment === "" || segment === "." || segment === "..") {
      throw new DatastoreKeyError(key, `invalid path segment "${segment}"`);
    }
  }
}

function isWindowsAbsolute(value: string): boolean {
  return /^[A-Za-z]:[\\/]/.test(value);
}
