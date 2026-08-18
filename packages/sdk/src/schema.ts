import { z } from "zod";
import { SchemaPortabilityError } from "./errors.ts";
import { type JsonValue, sha256Text, stableStringify } from "./stable.ts";

export type AnySchema = z.ZodType;

export interface LoweredSchema {
  readonly hash: string;
  readonly jsonSchema: JsonValue;
}

const portableTypes = new Set([
  "array",
  "boolean",
  "enum",
  "literal",
  "nullable",
  "number",
  "object",
  "optional",
  "string",
  "tuple",
  "union",
]);

export function lowerPortableSchema(
  schema: AnySchema,
  role: string,
): LoweredSchema {
  assertPortableSchema(schema, role);

  try {
    const jsonSchema = z.toJSONSchema(schema) as JsonValue;
    assertCanonicalJsonSchema(jsonSchema, role);
    return {
      hash: sha256Text(stableStringify(jsonSchema)),
      jsonSchema,
    };
  } catch (error) {
    if (error instanceof SchemaPortabilityError) throw error;
    const message = error instanceof Error ? error.message : String(error);
    throw new SchemaPortabilityError(
      role,
      `Zod could not lower it to JSON Schema: ${message}`,
    );
  }
}

function assertCanonicalJsonSchema(value: JsonValue, role: string): void {
  if (typeof value === "number") {
    if (!Number.isSafeInteger(value) || Object.is(value, -0)) {
      throw new SchemaPortabilityError(
        role,
        "schema numeric constants and bounds must be canonical safe integers",
      );
    }
    return;
  }
  if (Array.isArray(value)) {
    for (const item of value) assertCanonicalJsonSchema(item, role);
    return;
  }
  if (value === null || typeof value !== "object") return;

  const object = value as Record<string, JsonValue>;
  for (const child of Object.values(object)) {
    assertCanonicalJsonSchema(child, role);
  }
  const type = object.type;
  if (
    type === "number" ||
    (Array.isArray(type) && type.includes("number"))
  ) {
    throw new SchemaPortabilityError(
      role,
      "number schemas are not portable in v0; use z.int() so artifacts contain canonical safe integers",
    );
  }
}

export function assertPortableSchema(schema: AnySchema, role: string): void {
  const visited = new Set<AnySchema>();
  visitSchema(schema, role, visited);
}

function visitSchema(
  schema: AnySchema,
  role: string,
  visited: Set<AnySchema>,
): void {
  if (visited.has(schema)) return;
  visited.add(schema);

  const def = schemaDef(schema);
  const type = String(def.type ?? "unknown");

  if (!portableTypes.has(type)) {
    throw new SchemaPortabilityError(
      role,
      `${type} is not in the v0 portable subset`,
    );
  }

  if (def.coerce === true) {
    throw new SchemaPortabilityError(role, `${type} uses coercion`);
  }

  if (Array.isArray(def.checks) && def.checks.length > 0) {
    throw new SchemaPortabilityError(role, `${type} uses checks/refinements`);
  }

  switch (type) {
    case "array":
      visitSchema(def.element as AnySchema, `${role}[]`, visited);
      return;
    case "object":
      for (const [key, value] of Object.entries(objectShape(def))) {
        visitSchema(value as AnySchema, `${role}.${key}`, visited);
      }
      return;
    case "optional":
    case "nullable":
      visitSchema(def.innerType as AnySchema, role, visited);
      return;
    case "tuple":
      for (const [index, item] of (def.items as AnySchema[]).entries()) {
        visitSchema(item, `${role}[${index}]`, visited);
      }
      if (def.rest) {
        throw new SchemaPortabilityError(
          role,
          "rest tuples are not in the v0 portable subset",
        );
      }
      return;
    case "union":
      for (const [index, option] of (def.options as AnySchema[]).entries()) {
        visitSchema(option, `${role}.union(${index})`, visited);
      }
      return;
    default:
      return;
  }
}

function schemaDef(schema: AnySchema): Record<string, unknown> {
  // Zod exposes no public visitor API; keep the v0 portable-subset coupling isolated to Zod 4's public `.def` shape.
  return ((schema as unknown as { def?: Record<string, unknown> }).def ??
    {}) as Record<string, unknown>;
}

function objectShape(def: Record<string, unknown>): Record<string, unknown> {
  const shape = def.shape;
  if (typeof shape === "function") {
    return shape() as Record<string, unknown>;
  }
  return shape as Record<string, unknown>;
}
