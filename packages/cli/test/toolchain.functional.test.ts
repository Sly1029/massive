import { assert, assertStringIncludes } from "jsr:@std/assert";
import { selectOrchestratorBinary } from "../src/toolchain.ts";
import { repoRoot } from "./harness.ts";

// The orchestrator binary cache must not serve a binary built from a different
// Massive version. The cache key embeds the repo's HEAD revision, so a new
// commit keys a fresh build; a dirty Go tree bypasses the cache entirely.

const decoder = new TextDecoder();

async function gitHead(root: string): Promise<string> {
  const { code, stdout } = await new Deno.Command("git", {
    args: ["-C", root, "rev-parse", "HEAD"],
    stdout: "piped",
    stderr: "null",
  }).output();
  assert(code === 0, "git rev-parse HEAD should succeed in the repo");
  return decoder.decode(stdout).trim();
}

Deno.test("orchestrator binary cache key embeds the repo git revision", async () => {
  const root = repoRoot();
  const head = await gitHead(root);

  const selection = await selectOrchestratorBinary(root);

  // The 12-char revision prefix must appear in the cached binary name so a
  // different commit resolves to a different path (no stale reuse).
  assertStringIncludes(selection.path, head.slice(0, 12));
  assertStringIncludes(selection.path, "massive-orchestrator@");
});
