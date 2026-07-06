import { assertEquals } from "jsr:@std/assert";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { sha256Text, stableStringify } from "../src/stable.ts";

Deno.test("canonical hashing golden vector matches stableStringify sha256", async () => {
  const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "../../..");
  const inputPath = join(repoRoot, "conformance/fixtures/hashing/canonical-input.json");
  const expectedPath = join(repoRoot, "conformance/fixtures/hashing/canonical-input.sha256");

  const input = JSON.parse(await Deno.readTextFile(inputPath));
  const expected = (await Deno.readTextFile(expectedPath)).trim();

  assertEquals(`sha256:${sha256Text(stableStringify(input))}`, expected);
});
