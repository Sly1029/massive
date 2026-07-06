import { createHash } from "node:crypto";

export type JsonValue =
  | null
  | boolean
  | number
  | string
  | JsonValue[]
  | { [key: string]: JsonValue };

export function stableStringify(value: unknown): string {
  return JSON.stringify(sortJson(value));
}

export function sha256Text(value: string): string {
  return createHash("sha256").update(value).digest("hex");
}

export function sha256Bytes(value: Uint8Array): string {
  return createHash("sha256").update(value).digest("hex");
}

export function sha256RefText(value: string): string {
  return `sha256:${sha256Text(value)}`;
}

export function sha256RefBytes(value: Uint8Array): string {
  return `sha256:${sha256Bytes(value)}`;
}

// Locale-independent UTF-16 code-unit comparison, matching the key order of
// Object.keys().sort() used by stableStringify. Canonical orderings must use
// this, never localeCompare, or specHash diverges across machines.
export function compareCodeUnits(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}

function sortJson(value: unknown): unknown {
  if (Array.isArray(value)) {
    return value.map(sortJson);
  }

  if (value === null || typeof value !== "object") {
    return value;
  }

  const sorted: Record<string, unknown> = {};
  for (const key of Object.keys(value).sort()) {
    sorted[key] = sortJson((value as Record<string, unknown>)[key]);
  }
  return sorted;
}
