import { assertEquals, assertThrows } from "jsr:@std/assert";
import { z } from "zod";
import { SchemaPortabilityError } from "../src/errors.ts";
import { lowerPortableSchema } from "../src/schema.ts";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  CanonicalJsonError,
  parseCanonicalJsonText,
  sha256Text,
  stableStringify,
} from "../src/stable.ts";

Deno.test("canonical hashing golden vector matches stableStringify sha256", async () => {
  const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "../../..");
  const inputPath = join(
    repoRoot,
    "conformance/fixtures/hashing/canonical-input.json",
  );
  const expectedPath = join(
    repoRoot,
    "conformance/fixtures/hashing/canonical-input.sha256",
  );

  const input = JSON.parse(await Deno.readTextFile(inputPath));
  const expected = (await Deno.readTextFile(expectedPath)).trim();

  assertEquals(`sha256:${sha256Text(stableStringify(input))}`, expected);
});

Deno.test("canonical hashing accepts only safe integer field trees", () => {
  assertEquals(
    stableStringify({ zero: -0, nested: [0, 1, 9_007_199_254_740_991] }),
    '{"nested":[0,1,9007199254740991],"zero":0}',
  );

  for (
    const value of [
      1.5,
      Number.POSITIVE_INFINITY,
      9_007_199_254_740_992,
      undefined,
      () => undefined,
      Symbol("not-json"),
      1n,
      { value: undefined },
      { value: "\uD800" },
      { "\uD800": "value" },
    ]
  ) {
    assertThrows(() => stableStringify(value));
  }
});

Deno.test("canonical hashing rejects sparse arrays instead of silently serializing holes as null", () => {
  const sparse: unknown[] = [];
  sparse[1] = "value";
  assertThrows(() => stableStringify(sparse));
});

Deno.test("v0 schema lowering rejects fractional number schemas and accepts z.int", () => {
  const error = assertThrows(
    () => lowerPortableSchema(z.number(), "workflow input"),
    SchemaPortabilityError,
  );
  assertEquals(
    error.message,
    "workflow input uses a non-portable Zod schema: number schemas are not portable in v0; use z.int() so artifacts contain canonical safe integers",
  );
  assertEquals(
    (lowerPortableSchema(z.int(), "workflow input").jsonSchema as {
      type: string;
    }).type,
    "integer",
  );
  const literalError = assertThrows(
    () => lowerPortableSchema(z.literal(1.5), "workflow output"),
    SchemaPortabilityError,
  );
  assertEquals(
    literalError.message,
    "workflow output uses a non-portable Zod schema: schema numeric constants and bounds must be canonical safe integers",
  );
  const unsafeLiteralError = assertThrows(
    () =>
      lowerPortableSchema(
        z.literal(9_007_199_254_740_992),
        "workflow output",
      ),
    SchemaPortabilityError,
  );
  assertEquals(
    unsafeLiteralError.message,
    "workflow output uses a non-portable Zod schema: schema numeric constants and bounds must be canonical safe integers",
  );
});

Deno.test("canonical JSON v0 writes own prototype keys and integer-like keys in UTF-16 order", () => {
  assertEquals(
    stableStringify({ "2": "two", "10": "ten", a: "letter" }),
    '{"10":"ten","2":"two","a":"letter"}',
  );
  const prototypeKeys = Object.create(null) as Record<string, unknown>;
  Object.defineProperty(prototypeKeys, "__proto__", {
    enumerable: true,
    value: "own-data",
  });
  Object.defineProperty(prototypeKeys, "constructor", {
    enumerable: true,
    value: "own-data",
  });
  Object.defineProperty(prototypeKeys, "prototype", {
    enumerable: true,
    value: "own-data",
  });
  assertEquals(
    stableStringify(prototypeKeys),
    '{"__proto__":"own-data","constructor":"own-data","prototype":"own-data"}',
  );
});

Deno.test("canonical JSON v0 rejects accessor properties before evaluating them", () => {
  const object = {} as Record<string, unknown>;
  Object.defineProperty(object, "value", {
    enumerable: true,
    get: () => 1,
  });
  const objectError = assertThrows(
    () => stableStringify(object),
    CanonicalJsonError,
  );
  assertEquals(
    objectError.message,
    "canonical JSON objects must contain data properties",
  );

  const array: unknown[] = [];
  Object.defineProperty(array, "0", {
    enumerable: true,
    get: () => 1,
  });
  array.length = 1;
  const arrayError = assertThrows(
    () => stableStringify(array),
    CanonicalJsonError,
  );
  assertEquals(
    arrayError.message,
    "canonical JSON arrays must contain data properties",
  );
});

Deno.test("canonical JSON v0 conformance corpus keeps only canonical wire payloads", async () => {
  const root = join(
    dirname(fileURLToPath(import.meta.url)),
    "../../../conformance/fixtures/canonical-json-v0",
  );
  const expectedHashes = JSON.parse(
    await Deno.readTextFile(join(root, "hashes.json")),
  ) as Record<string, string>;
  const validFixtureNames: string[] = [];
  for (const validity of ["valid", "invalid"] as const) {
    let fixtureCount = 0;
    for await (const entry of Deno.readDir(join(root, validity))) {
      if (!entry.isFile || !entry.name.endsWith(".json")) continue;
      fixtureCount += 1;
      const payload = canonicalFixturePayload(
        await Deno.readTextFile(join(root, validity, entry.name)),
      );
      if (validity === "valid") {
        validFixtureNames.push(entry.name);
        assertEquals(
          `sha256:${sha256Text(payload)}`,
          expectedHashes[entry.name],
          `${entry.name} must match its pinned canonical bytes`,
        );
        assertEquals(
          stableStringify(parseCanonicalJsonText(payload)),
          payload,
        );
      } else {
        assertThrows(
          () => parseCanonicalJsonText(payload),
          CanonicalJsonError,
        );
      }
    }
    assertEquals(
      fixtureCount > 0,
      true,
      `${validity} corpus must not be empty`,
    );
  }
  assertEquals(Object.keys(expectedHashes).sort(), validFixtureNames.sort());
});

function canonicalFixturePayload(fixture: string): string {
  if (!fixture.endsWith("\n") || fixture.endsWith("\r\n")) {
    throw new Error(
      "canonical fixture must use exactly one final LF as repository transport",
    );
  }
  return fixture.slice(0, -1);
}
