import { assertEquals, assertThrows } from "jsr:@std/assert";
import { join } from "node:path";
import {
  hashSourcePackage,
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

Deno.test("source include wildcards never select dot paths unless named", async () => {
  const root = await Deno.makeTempDir({ prefix: "massive-source-hidden-" });
  try {
    for (
      const path of [
        "workflow.ts",
        "src/module.ts",
        "src/.npmrc",
        ".env",
        ".env.local",
        ".git/HEAD",
        ".aws/credentials",
        ".github/workflows/ci.yml",
      ]
    ) {
      await Deno.mkdir(join(root, path, ".."), { recursive: true });
      await Deno.writeTextFile(join(root, path), "fixture\n");
    }

    const selected = await hashSourcePackage({
      root,
      include: ["**/*", ".github/**"],
    });

    assertEquals(selected.files.map((file) => file.path), [
      ".github/workflows/ci.yml",
      "src/module.ts",
      "workflow.ts",
    ]);
  } finally {
    await Deno.remove(root, { recursive: true });
  }
});
