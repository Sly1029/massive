import { z } from "zod";
import { SchemaPortabilityError } from "./errors.ts";
import { sha256Text, stableStringify, type JsonValue } from "./stable.ts";

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

export function lowerPortableSchema(schema: AnySchema, role: string): LoweredSchema {
  assertPortableSchema(schema, role);

  try {
    const jsonSchema = z.toJSONSchema(schema) as JsonValue;
    return {
      hash: sha256Text(stableStringify(jsonSchema)),
      jsonSchema,
    };
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new SchemaPortabilityError(role, `Zod could not lower it to JSON Schema: ${message}`);
  }
}

export function assertPortableSchema(schema: AnySchema, role: string): void {
  const visited = new Set<AnySchema>();
  visitSchema(schema, role, visited);
}

function visitSchema(schema: AnySchema, role: string, visited: Set<AnySchema>): void {
  if (visited.has(schema)) return;
  visited.add(schema);

  const def = schemaDef(schema);
  const type = String(def.type ?? "unknown");

  if (!portableTypes.has(type)) {
    throw new SchemaPortabilityError(role, `${type} is not in the v0 portable subset`);
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
        throw new SchemaPortabilityError(role, "rest tuples are not in the v0 portable subset");
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
  return ((schema as unknown as { _def?: Record<string, unknown>; def?: Record<string, unknown> })._def ??
    (schema as unknown as { def?: Record<string, unknown> }).def ??
    {}) as Record<string, unknown>;
}

function objectShape(def: Record<string, unknown>): Record<string, unknown> {
  const shape = def.shape;
  if (typeof shape === "function") {
    return shape() as Record<string, unknown>;
  }
  return shape as Record<string, unknown>;
}
