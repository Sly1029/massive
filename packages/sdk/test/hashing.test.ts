import { assertEquals, assertThrows } from "jsr:@std/assert";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  parseCanonicalJsonText,
  sha256Text,
  stableStringify,
} from "../src/stable.ts";

Deno.test("canonical hashing golden vector matches stableStringify sha256", async () => {
  const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "../../..");
  const inputPath = join(repoRoot, "conformance/fixtures/hashing/canonical-input.json");
  const expectedPath = join(repoRoot, "conformance/fixtures/hashing/canonical-input.sha256");

  const input = JSON.parse(await Deno.readTextFile(inputPath));
  const expected = (await Deno.readTextFile(expectedPath)).trim();

  assertEquals(`sha256:${sha256Text(stableStringify(input))}`, expected);
});

Deno.test("canonical hashing accepts only safe integer field trees", () => {
  assertEquals(
    stableStringify({ zero: -0, nested: [0, 1, 9_007_199_254_740_991] }),
    '{"nested":[0,1,9007199254740991],"zero":0}',
  );

  for (const value of [
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
  ]) {
    assertThrows(() => stableStringify(value));
  }
});

Deno.test("canonical hashing rejects sparse arrays instead of silently serializing holes as null", () => {
  const sparse: unknown[] = [];
  sparse[1] = "value";
  assertThrows(() => stableStringify(sparse));
});

Deno.test("canonical JSON v0 conformance corpus keeps only canonical wire payloads", async () => {
  const root = join(
    dirname(fileURLToPath(import.meta.url)),
    "../../../conformance/fixtures/canonical-json-v0",
  );
  for (const validity of ["valid", "invalid"] as const) {
    for await (const entry of Deno.readDir(join(root, validity))) {
      if (!entry.isFile || !entry.name.endsWith(".json")) continue;
      const text = await Deno.readTextFile(join(root, validity, entry.name));
      if (validity === "valid") {
        assertEquals(stableStringify(parseCanonicalJsonText(text.trimEnd())), text.trimEnd());
      } else {
        assertThrows(() => parseCanonicalJsonText(text.trimEnd()));
      }
    }
  }
});
