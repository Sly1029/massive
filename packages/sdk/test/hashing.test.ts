import { assertEquals, assertThrows } from "jsr:@std/assert";
import { z } from "zod";
import { lowerPortableSchema } from "../src/schema.ts";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  CanonicalJsonError,
  decodeCanonicalUtf8,
  parseCanonicalJsonText,
  sha256Bytes,
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

Deno.test("canonical hashing accepts finite doubles and rejects unsupported values", () => {
  assertEquals(
    stableStringify({ zero: -0, nested: [0, 1, 9_007_199_254_740_991] }),
    '{"nested":[0,1,9007199254740991],"zero":0}',
  );

  for (
    const value of [
      Number.NaN,
      Number.NEGATIVE_INFINITY,
      Number.POSITIVE_INFINITY,
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

Deno.test("v0 schema lowering accepts number schemas, fractional constants, and integers", () => {
  for (
    const [schema, type] of [[z.number(), "number"], [z.int(), "integer"], [
      z.literal(1.5),
      "number",
    ], [z.literal(2 ** 53), "number"]] as const
  ) {
    assertEquals(
      (lowerPortableSchema(schema, "workflow input").jsonSchema as {
        type: string;
      }).type,
      type,
    );
  }
});

Deno.test("schema lowering explains why fractional multipleOf is not portable", () => {
  for (
    const schema of [
      z.number().multipleOf(0.1),
      z.object({ values: z.array(z.number().multipleOf(0.5)) }),
    ]
  ) {
    const error = assertThrows(
      () => lowerPortableSchema(schema, "workflow input"),
      Error,
    );
    assertEquals(error.message.includes("non-integer multipleOf"), true);
  }
});

Deno.test("all runtimes share IEEE-754 double bytes and hashes", async () => {
  const root = join(
    dirname(fileURLToPath(import.meta.url)),
    "../../../conformance/fixtures/canonical-json-v0",
  );
  const vectors = JSON.parse(
    await Deno.readTextFile(join(root, "numbers.json")),
  ) as {
    cases: { bits: string; canonical: string | null; hash: string | null }[];
    sources: { source: string; canonical: string | null }[];
  };
  for (const vector of vectors.cases) {
    const buffer = new ArrayBuffer(8);
    const view = new DataView(buffer);
    view.setBigUint64(0, BigInt(`0x${vector.bits}`));
    const value = view.getFloat64(0);
    if (vector.canonical === null) {
      assertThrows(() => stableStringify(value), CanonicalJsonError);
      continue;
    }
    const encoded = stableStringify(value);
    assertEquals(encoded, vector.canonical, vector.bits);
    assertEquals(`sha256:${sha256Text(encoded)}`, vector.hash, vector.bits);
    assertEquals(parseCanonicalJsonText(encoded), value === 0 ? 0 : value);
  }
  for (const vector of vectors.sources) {
    if (vector.canonical === null || vector.source !== vector.canonical) {
      assertThrows(
        () => parseCanonicalJsonText(vector.source),
        CanonicalJsonError,
      );
    }
    if (vector.canonical !== null) {
      assertEquals(
        stableStringify(JSON.parse(vector.source)),
        vector.canonical,
      );
    }
  }
  assertEquals(stableStringify(3.0), stableStringify(3));
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
      const payloadBytes = canonicalFixturePayload(
        await Deno.readFile(join(root, validity, entry.name)),
      );
      const payload = decodeCanonicalUtf8(payloadBytes);
      if (validity === "valid") {
        validFixtureNames.push(entry.name);
        assertEquals(
          `sha256:${sha256Bytes(payloadBytes)}`,
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

  const escaping = parseCanonicalJsonText(
    decodeCanonicalUtf8(
      canonicalFixturePayload(
        await Deno.readFile(join(root, "valid", "escaping.json")),
      ),
    ),
  ) as { controls: string; raw: string };
  assertEquals(
    escaping.controls,
    String.fromCodePoint(...Array.from({ length: 32 }, (_, index) => index)),
    "the escaping vector preserves every invisible C0 control",
  );
  assertEquals(
    escaping.raw,
    "<>&\u2028\u2029",
    "the escaping vector keeps non-control source characters unescaped",
  );
});

function canonicalFixturePayload(fixture: Uint8Array): Uint8Array {
  if (
    fixture.byteLength < 1 ||
    fixture.at(-1) !== 0x0a ||
    fixture.at(-2) === 0x0d ||
    fixture.at(-2) === 0x0a
  ) {
    throw new Error(
      "canonical fixture must use exactly one final LF as repository transport",
    );
  }
  return fixture.slice(0, -1);
}
