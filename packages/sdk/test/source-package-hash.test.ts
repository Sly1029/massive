import { assertEquals, assertThrows } from "jsr:@std/assert";
import {
  parseSourcePackageFiles,
  sourcePackageDigest,
} from "../src/source-package.ts";

Deno.test("source package hash consumes the versioned shared recipe vector", async () => {
  const input = JSON.parse(
    await Deno.readTextFile(
      new URL(
        "../../../conformance/fixtures/hashing/source-package-v1.json",
        import.meta.url,
      ),
    ),
  ) as { files: { path: string; hash: string }[] };
  const expected = (await Deno.readTextFile(
    new URL(
      "../../../conformance/fixtures/hashing/source-package-v1.sha256",
      import.meta.url,
    ),
  )).trim();
  assertEquals(
    sourcePackageDigest(parseSourcePackageFiles(input.files)),
    expected,
  );
});

Deno.test("source package identity rejects noncanonical file manifests", () => {
  const hash = `sha256:${"a".repeat(64)}`;
  for (
    const files of [
      [{ path: "b.py", hash }, { path: "a.py", hash }],
      [{ path: "a.py", hash }, { path: "a.py", hash }],
      [{ path: "./a.py", hash }],
      [{ path: "../a.py", hash }],
      [{ path: "src/../a.py", hash }],
      [{ path: "src/", hash }],
      [{ path: "a.py", hash: "sha256:not-a-digest" }],
      [],
    ]
  ) {
    assertThrows(() => parseSourcePackageFiles(files));
  }
});
