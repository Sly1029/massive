import { assertEquals } from "jsr:@std/assert";
import { sha256RefText } from "../src/stable.ts";

Deno.test("source package hash consumes the versioned shared recipe vector", async () => {
  const input = (await Deno.readTextFile(
    new URL(
      "../../../conformance/fixtures/hashing/source-package-v1.json",
      import.meta.url,
    ),
  )).trimEnd();
  const expected = (await Deno.readTextFile(
    new URL(
      "../../../conformance/fixtures/hashing/source-package-v1.sha256",
      import.meta.url,
    ),
  )).trim();
  assertEquals(
    sha256RefText(input),
    expected,
  );
});
