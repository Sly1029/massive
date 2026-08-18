import { createHash } from "node:crypto";

export type JsonValue =
  | null
  | boolean
  | number
  | string
  | JsonValue[]
  | { [key: string]: JsonValue };

export class CanonicalJsonError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "CanonicalJsonError";
  }
}

export function stableStringify(value: unknown): string {
  return JSON.stringify(sortJson(value, new Set<object>()));
}

// Parses a wire payload through the same canonical field-tree boundary used by
// hashing. It rejects lexical forms (for example 1e3 and -0) that JSON.parse
// normalizes into otherwise valid values, so callers can safely compare and
// hash the original bytes.
export function parseCanonicalJsonText(text: string): JsonValue {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch (error) {
    throw new CanonicalJsonError(
      `canonical JSON is not valid JSON: ${
        error instanceof Error ? error.message : String(error)
      }`,
    );
  }
  if (stableStringify(value) !== text) {
    throw new CanonicalJsonError("JSON source is not canonical-json-v0");
  }
  return value as JsonValue;
}

export function sha256Text(value: string): string {
  return createHash("sha256").update(value).digest("hex");
}

export function sha256Bytes(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

export function sha256RefText(value: string): `sha256:${string}` {
  return `sha256:${sha256Text(value)}`;
}

export function sha256RefBytes(value: Uint8Array): `sha256:${string}` {
  return `sha256:${sha256Bytes(value)}`;
}

// Locale-independent UTF-16 code-unit comparison, matching the key order of
// Object.keys().sort() used by stableStringify. Canonical orderings must use
// this, never localeCompare, or specHash diverges across machines.
export function compareCodeUnits(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}

function sortJson(value: unknown, ancestors: Set<object>): JsonValue {
  if (value === null || typeof value === "boolean" || typeof value === "string") {
    if (typeof value === "string") validateUnicode(value);
    return value;
  }

  if (typeof value === "number") {
    if (!Number.isSafeInteger(value)) {
      throw new CanonicalJsonError(
        "v0 canonical JSON numbers must be safe integers",
      );
    }
    return value;
  }

  if (Array.isArray(value)) {
    if (ancestors.has(value)) {
      throw new CanonicalJsonError("canonical JSON cannot contain cycles");
    }
    if (Object.getOwnPropertySymbols(value).length > 0) {
      throw new CanonicalJsonError("canonical JSON arrays cannot have symbol keys");
    }
    for (let index = 0; index < value.length; index += 1) {
      if (!Object.hasOwn(value, index)) {
        throw new CanonicalJsonError("canonical JSON arrays cannot be sparse");
      }
    }
    const keys = Object.keys(value);
    if (keys.length !== value.length) {
      throw new CanonicalJsonError("canonical JSON arrays cannot have extra properties");
    }
    ancestors.add(value);
    try {
      return value.map((item) => sortJson(item, ancestors));
    } finally {
      ancestors.delete(value);
    }
  }

  if (typeof value !== "object") {
    throw new CanonicalJsonError(
      `canonical JSON does not support ${typeof value} values`,
    );
  }
  if (ancestors.has(value)) {
    throw new CanonicalJsonError("canonical JSON cannot contain cycles");
  }
  const prototype = Object.getPrototypeOf(value);
  if (prototype !== Object.prototype && prototype !== null) {
    throw new CanonicalJsonError("canonical JSON objects must be plain objects");
  }
  if (Object.getOwnPropertySymbols(value).length > 0) {
    throw new CanonicalJsonError("canonical JSON objects cannot have symbol keys");
  }

  const sorted: Record<string, unknown> = {};
  ancestors.add(value);
  try {
    for (const key of Object.keys(value).sort(compareCodeUnits)) {
      validateUnicode(key);
      sorted[key] = sortJson((value as Record<string, unknown>)[key], ancestors);
    }
  } finally {
    ancestors.delete(value);
  }
  return sorted as { [key: string]: JsonValue };
}

function validateUnicode(value: string): void {
  for (let index = 0; index < value.length; index += 1) {
    const codeUnit = value.charCodeAt(index);
    if (codeUnit >= 0xd800 && codeUnit <= 0xdbff) {
      const following = value.charCodeAt(index + 1);
      if (!(following >= 0xdc00 && following <= 0xdfff)) {
        throw new CanonicalJsonError("canonical JSON strings cannot contain lone surrogates");
      }
      index += 1;
    } else if (codeUnit >= 0xdc00 && codeUnit <= 0xdfff) {
      throw new CanonicalJsonError("canonical JSON strings cannot contain lone surrogates");
    }
  }
}
