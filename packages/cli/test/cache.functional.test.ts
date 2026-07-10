import { assert, assertEquals, assertMatch } from "jsr:@std/assert";
import {
  copyFixture,
  exists,
  findRunArtifact,
  fixtureEntry,
  join,
  listDirEntries,
  makeStore,
  runCli,
} from "./harness.ts";

// WS-6.2 — a second identical run reuses the cached spec + plan (observable in
// --verbose) and is materially faster. "Materially faster" is asserted through
// a deterministic proxy: the workflow module import is skipped on a cache hit
// (import sentinel absent) and the reused markers appear in verbose output —
// not a flaky wall-clock threshold.

Deno.test("massive run twice: cache hit reuses spec + plan and skips the module import; a source edit is a cache miss", async () => {
  const fixture = await copyFixture("linear-chain");
  const store = await makeStore();
  const workflowPath = fixtureEntry(fixture);

  const sentinelDir = await Deno.makeTempDir({ prefix: "massive-sentinel-" });
  const sentinel = join(sentinelDir, "import-sentinel.log");
  const env = { MASSIVE_IMPORT_SENTINEL: sentinel };

  const baseArgs = (runId: string): string[] => [
    "run",
    workflowPath,
    "--input",
    "20",
    "--store",
    store,
    "--project",
    "acme/wf",
    "--run-id",
    runId,
    "--verbose",
  ];

  // Run 1: full path. The emit import records the sentinel; spec + plan land.
  const first = await runCli(baseArgs("run-a"), { env });
  assertEquals(first.code, 0, first.stderr);
  assert(
    await exists(sentinel),
    "run 1 emit should import the workflow module (sentinel written)",
  );
  assertEquals(
    (await listDirEntries(join(store, "specs"))).length,
    1,
    "run 1 should persist exactly one spec",
  );
  assert(
    (await listDirEntries(join(store, "plans"))).length >= 1,
    "run 1 should persist a compiled plan",
  );
  const firstResultPath = await findRunArtifact(store, "run-a", "result.json");
  assert(firstResultPath !== undefined);
  const firstResult = await Deno.readTextFile(firstResultPath);

  // Run 2: source unchanged -> cache hit. Prove the module import was skipped.
  await Deno.remove(sentinel);
  const second = await runCli(baseArgs("run-b"), { env });
  assertEquals(second.code, 0, second.stderr);
  assertMatch(second.stdout, /spec\s+reused/);
  assertMatch(second.stdout, /plan\s+reused/);
  assertEquals(
    await exists(sentinel),
    false,
    "run 2 cache hit must not re-import the workflow module (no sentinel)",
  );
  assertEquals(
    (await listDirEntries(join(store, "specs"))).length,
    1,
    "a cache hit must not create a new spec",
  );

  const secondResultPath = await findRunArtifact(store, "run-b", "result.json");
  assert(secondResultPath !== undefined);
  assertEquals(
    await Deno.readTextFile(secondResultPath),
    firstResult,
    "reused run must produce a byte-identical result",
  );

  // Edit a source file -> new sourcePackageHash -> new specHash -> cache miss.
  const edited = (await Deno.readTextFile(workflowPath)).replace(
    "return args.input * 2;",
    "return args.input * 3;",
  );
  await Deno.writeTextFile(workflowPath, edited);

  const third = await runCli(baseArgs("run-c"), { env });
  assertEquals(third.code, 0, third.stderr);
  assertMatch(third.stdout, /spec\s+emitted/);
  assert(
    await exists(sentinel),
    "a cache miss must re-import the workflow module (sentinel written)",
  );
  assertEquals(
    (await listDirEntries(join(store, "specs"))).length,
    2,
    "an edited source must emit a new spec (new specHash)",
  );
});
