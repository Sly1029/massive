import { createHash } from "node:crypto";

export type JsonValue =
  | null
  | boolean
  | number
  | string
  | JsonValue[]
  | { [key: string]: JsonValue };

export class CanonicalJsonError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = "CanonicalJsonError";
  }
}

export function stableStringify(value: unknown): string {
  const out: string[] = [];
  writeCanonicalJson(out, value, new Set<object>());
  return out.join("");
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

// TextDecoder normally replaces malformed sequences and silently consumes a
// leading UTF-8 BOM. Neither transformation is permitted at a canonical-byte
// boundary: the bytes themselves are what get hashed and published.
export function decodeCanonicalUtf8(bytes: Uint8Array): string {
  if (
    bytes.byteLength >= 3 &&
    bytes[0] === 0xef &&
    bytes[1] === 0xbb &&
    bytes[2] === 0xbf
  ) {
    throw new CanonicalJsonError(
      "canonical JSON must not begin with a UTF-8 byte-order mark",
    );
  }
  try {
    return new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(
      bytes,
    );
  } catch (error) {
    throw new CanonicalJsonError(
      `canonical JSON must be valid UTF-8: ${
        error instanceof Error ? error.message : String(error)
      }`,
      { cause: error },
    );
  }
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

// Locale-independent UTF-16 code-unit comparison. Canonical orderings must use
// this, never localeCompare, or specHash diverges across machines.
export function compareCodeUnits(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}

// Do not build an intermediate object then call JSON.stringify: JavaScript
// gives array-index keys their own enumeration order and treats __proto__ as a
// setter on ordinary objects. Both behaviours corrupt a digestable field tree.
function writeCanonicalJson(
  out: string[],
  value: unknown,
  ancestors: Set<object>,
): void {
  if (
    value === null || typeof value === "boolean" || typeof value === "string"
  ) {
    if (typeof value === "string") {
      validateUnicode(value);
      out.push(JSON.stringify(value));
      return;
    }
    out.push(value === null ? "null" : value ? "true" : "false");
    return;
  }

  if (typeof value === "number") {
    if (!Number.isSafeInteger(value)) {
      throw new CanonicalJsonError(
        "v0 canonical JSON numbers must be safe integers",
      );
    }
    out.push(String(value));
    return;
  }

  if (Array.isArray(value)) {
    if (ancestors.has(value)) {
      throw new CanonicalJsonError("canonical JSON cannot contain cycles");
    }
    if (Object.getOwnPropertySymbols(value).length > 0) {
      throw new CanonicalJsonError(
        "canonical JSON arrays cannot have symbol keys",
      );
    }
    for (let index = 0; index < value.length; index += 1) {
      if (!Object.hasOwn(value, index)) {
        throw new CanonicalJsonError("canonical JSON arrays cannot be sparse");
      }
    }
    const keys = Object.keys(value);
    if (keys.length !== value.length) {
      throw new CanonicalJsonError(
        "canonical JSON arrays cannot have extra properties",
      );
    }
    ancestors.add(value);
    try {
      out.push("[");
      for (let index = 0; index < value.length; index += 1) {
        if (index > 0) out.push(",");
        const property = Object.getOwnPropertyDescriptor(value, index);
        if (
          property === undefined || property.get !== undefined ||
          property.set !== undefined
        ) {
          throw new CanonicalJsonError(
            "canonical JSON arrays must contain data properties",
          );
        }
        writeCanonicalJson(out, property.value, ancestors);
      }
      out.push("]");
      return;
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
    throw new CanonicalJsonError(
      "canonical JSON objects must be plain objects",
    );
  }
  if (Object.getOwnPropertySymbols(value).length > 0) {
    throw new CanonicalJsonError(
      "canonical JSON objects cannot have symbol keys",
    );
  }

  ancestors.add(value);
  try {
    out.push("{");
    for (
      const [index, key] of Object.keys(value).sort(compareCodeUnits).entries()
    ) {
      if (index > 0) out.push(",");
      validateUnicode(key);
      const property = Object.getOwnPropertyDescriptor(value, key);
      if (
        property === undefined || property.get !== undefined ||
        property.set !== undefined
      ) {
        throw new CanonicalJsonError(
          "canonical JSON objects must contain data properties",
        );
      }
      out.push(JSON.stringify(key), ":");
      writeCanonicalJson(out, property.value, ancestors);
    }
    out.push("}");
  } finally {
    ancestors.delete(value);
  }
}

function validateUnicode(value: string): void {
  for (let index = 0; index < value.length; index += 1) {
    const codeUnit = value.charCodeAt(index);
    if (codeUnit >= 0xd800 && codeUnit <= 0xdbff) {
      const following = value.charCodeAt(index + 1);
      if (!(following >= 0xdc00 && following <= 0xdfff)) {
        throw new CanonicalJsonError(
          "canonical JSON strings cannot contain lone surrogates",
        );
      }
      index += 1;
    } else if (codeUnit >= 0xdc00 && codeUnit <= 0xdfff) {
      throw new CanonicalJsonError(
        "canonical JSON strings cannot contain lone surrogates",
      );
    }
  }
}
