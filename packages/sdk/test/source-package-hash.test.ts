import { assertEquals } from "jsr:@std/assert";
import { sha256RefText, stableStringify } from "../src/stable.ts";

// Non-circular golden vector for the source-package hash construction used by
// hashSourcePackage in ../src/compile.ts: sha256 of the canonical JSON of the
// {path, hash} entries array, prefixed with "sha256:". The expected value is
// computed once and mirrored in the Go test
// (internal/orchestrator TestSourcePackageHashGoldenVector) so the Go and TS
// implementations stay pinned to the same constant rather than to each other.
// TODO: promote this vector into conformance/fixtures/hashing once the frozen
// contract fixtures are opened for additions.
Deno.test("source package hash matches the fixed golden vector", () => {
  const entries = [
    {
      path: "src/a.ts",
      hash:
        "sha256:1111111111111111111111111111111111111111111111111111111111111111",
    },
    {
      path: "src/b.ts",
      hash:
        "sha256:2222222222222222222222222222222222222222222222222222222222222222",
    },
    {
      path: "src/nested/c.ts",
      hash:
        "sha256:3333333333333333333333333333333333333333333333333333333333333333",
    },
  ];

  assertEquals(
    sha256RefText(stableStringify(entries)),
    "sha256:88780f05b7195a396acac9aa6ddbea16445f275dfc10f32c94972beb59a711cb",
  );
});
