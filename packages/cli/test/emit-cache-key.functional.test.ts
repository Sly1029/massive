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

// The emit cache key must capture the RESOLVED entrypoint identity and the
// config-file bytes, not just the source-package hash — otherwise two workflows
// in one package collide, and a config edit reuses a stale spec.

Deno.test("emit cache keys on entrypoint identity: a second workflow in the same package is a miss", async () => {
  const fixture = await copyFixture("two-workflows");
  const store = await makeStore();
  const workflowPath = fixtureEntry(fixture);

  const sentinelDir = await Deno.makeTempDir({ prefix: "massive-sentinel-" });
  const sentinel = join(sentinelDir, "import-sentinel.log");
  const env = { MASSIVE_IMPORT_SENTINEL: sentinel };

  const args = (entry: string, runId: string): string[] => [
    "run",
    entry,
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

  // Run A (alpha): cache miss — imports the module and emits alpha's spec.
  const first = await runCli(args(`${workflowPath}#alpha`, "run-alpha-1"), {
    env,
  });
  assertEquals(first.code, 0, first.stderr);
  assertMatch(first.stdout, /spec\s+emitted/);
  assert(await exists(sentinel), "run A should import the workflow module");

  const alphaResult = await findRunArtifact(
    store,
    "run-alpha-1",
    "result.json",
  );
  assert(alphaResult !== undefined);
  assertEquals(
    await Deno.readTextFile(alphaResult),
    "40",
    "alpha doubles 20 -> 40",
  );

  // Run B (beta): same source hash + config + targets, different export. It must
  // still MISS and execute beta's distinct graph (triple 20 -> 60), not reuse
  // alpha's cached spec.
  await Deno.remove(sentinel);
  const second = await runCli(args(`${workflowPath}#beta`, "run-beta"), {
    env,
  });
  assertEquals(second.code, 0, second.stderr);
  assertMatch(second.stdout, /spec\s+emitted/);
  assert(
    await exists(sentinel),
    "run B must be a cache miss (module re-imported) despite the shared package",
  );

  const betaResult = await findRunArtifact(store, "run-beta", "result.json");
  assert(betaResult !== undefined);
  assertEquals(
    await Deno.readTextFile(betaResult),
    "60",
    "beta triples 20 -> 60 — proves B ran its own graph, not alpha's",
  );
  assertEquals(
    (await listDirEntries(join(store, "specs"))).length,
    2,
    "two distinct workflows must persist two specs",
  );

  // Run A again (alpha): now a genuine HIT — the module import is skipped.
  await Deno.remove(sentinel);
  const third = await runCli(args(`${workflowPath}#alpha`, "run-alpha-2"), {
    env,
  });
  assertEquals(third.code, 0, third.stderr);
  assertMatch(third.stdout, /spec\s+reused/);
  assertEquals(
    await exists(sentinel),
    false,
    "the repeated alpha run must be a cache hit (no module import)",
  );
});

Deno.test("a massive.config.ts edit invalidates the cached spec", async () => {
  const fixture = await copyFixture("linear-chain");
  const store = await makeStore();
  const workflowPath = fixtureEntry(fixture);
  const configPath = join(fixture, "massive.config.ts");

  const args = (runId: string): string[] => [
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

  const first = await runCli(args("run-cfg-1"));
  assertEquals(first.code, 0, first.stderr);
  assertMatch(first.stdout, /spec\s+emitted/);
  assertEquals((await listDirEntries(join(store, "specs"))).length, 1);

  // Edit the config's environment. The config file is not covered by `include`,
  // so the source-package hash is unchanged; only configHash differs. Without it
  // in the key this would be a false hit reusing the stale spec.
  const edited = (await Deno.readTextFile(configPath)).replace(
    "targets: [target.local()],",
    'environment: { kind: "container", image: "docker.io/library/node:20" },\n  targets: [target.local()],',
  );
  await Deno.writeTextFile(configPath, edited);

  const second = await runCli(args("run-cfg-2"));
  assertEquals(second.code, 0, second.stderr);
  assertMatch(
    second.stdout,
    /spec\s+emitted/,
    "a config edit must be a cache miss",
  );
  assertEquals(
    (await listDirEntries(join(store, "specs"))).length,
    2,
    "the config edit must persist a new spec",
  );
});

Deno.test("a corrupt emit-cache pointer self-heals: the run re-emits instead of crashing", async () => {
  const fixture = await copyFixture("linear-chain");
  const store = await makeStore();
  const workflowPath = fixtureEntry(fixture);

  const args = (runId: string): string[] => [
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

  const first = await runCli(args("run-heal-1"));
  assertEquals(first.code, 0, first.stderr);

  // Point the emit-cache entry at a spec key that does not exist.
  const cacheDir = join(store, "cache", "emit");
  const pointers = (await listDirEntries(cacheDir)).filter((name) =>
    name.endsWith(".json")
  );
  assertEquals(
    pointers.length,
    1,
    "run 1 should have written one cache pointer",
  );
  await Deno.writeTextFile(
    join(cacheDir, pointers[0]!),
    `specs/sha256-${"0".repeat(64)}/workflow-spec.json`,
  );

  // The stale pointer must not crash the run: it is treated as a miss and the
  // spec is re-emitted.
  const second = await runCli(args("run-heal-2"));
  assertEquals(second.code, 0, second.stderr);
  assertMatch(second.stdout, /spec\s+emitted/);
  assertMatch(second.stdout, /emit cache invalid/);
  // Self-healing must be graceful: no uncaught stack trace leaks to stderr.
  assertEquals(second.stderr.includes("    at "), false, second.stderr);
  assertEquals(second.stderr.includes("Uncaught"), false, second.stderr);

  const result = await findRunArtifact(store, "run-heal-2", "result.json");
  assert(
    result !== undefined,
    "the re-emitted run should still produce a result",
  );
});

Deno.test("editing an imported settings module invalidates the cached spec", async () => {
  const fixture = await copyFixture("settings-import");
  const store = await makeStore();
  const workflowPath = fixtureEntry(fixture);
  const settingsPath = join(fixture, "settings.ts");

  const args = (runId: string): string[] => [
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

  const first = await runCli(args("run-set-1"));
  assertEquals(first.code, 0, first.stderr);
  assertMatch(first.stdout, /spec\s+emitted/);
  assertEquals((await listDirEntries(join(store, "specs"))).length, 1);

  // Edit settings.ts only. massive.config.ts's bytes are unchanged, but the
  // evaluated config (environment.image) differs, so it must be a cache miss.
  const edited = (await Deno.readTextFile(settingsPath)).replace(
    "node:20",
    "node:22",
  );
  await Deno.writeTextFile(settingsPath, edited);

  const second = await runCli(args("run-set-2"));
  assertEquals(second.code, 0, second.stderr);
  assertMatch(
    second.stdout,
    /spec\s+emitted/,
    "an imported-settings edit must be a cache miss",
  );
  assertEquals(
    (await listDirEntries(join(store, "specs"))).length,
    2,
    "the settings edit must persist a new spec",
  );
});

Deno.test("a cache pointer aimed at a different valid spec is rejected; the correct workflow runs", async () => {
  const fixture = await copyFixture("two-workflows");
  const store = await makeStore();
  const workflowPath = fixtureEntry(fixture);
  const cacheDir = join(store, "cache", "emit");

  const args = (entry: string, runId: string): string[] => [
    "run",
    entry,
    "--input",
    "20",
    "--store",
    store,
    "--project",
    "acme/wf",
    "--run-id",
    runId,
  ];

  // Run alpha (result 40): one valid spec + one pointer.
  assertEquals(
    (await runCli(args(`${workflowPath}#alpha`, "diff-alpha"))).code,
    0,
  );
  const afterAlpha = new Set(
    (await listDirEntries(cacheDir)).filter((n) => n.endsWith(".json")),
  );

  // Run beta (result 60): a second valid spec + a second pointer.
  assertEquals(
    (await runCli(args(`${workflowPath}#beta`, "diff-beta"))).code,
    0,
  );
  const allPointers = (await listDirEntries(cacheDir)).filter((n) =>
    n.endsWith(".json")
  );
  const alphaPointer = [...afterAlpha][0]!;
  const betaPointer = allPointers.find((n) => !afterAlpha.has(n))!;

  // Repoint alpha's cache entry at beta's (a different, internally-valid spec).
  await Deno.writeTextFile(
    join(cacheDir, alphaPointer),
    await Deno.readTextFile(join(cacheDir, betaPointer)),
  );

  // Running alpha again must NOT execute beta: the integrity check rejects the
  // mis-pointed pointer and re-emits alpha's spec.
  const rerun = await runCli([
    ...args(`${workflowPath}#alpha`, "diff-alpha-2"),
    "--verbose",
  ]);
  assertEquals(rerun.code, 0, rerun.stderr);
  assertMatch(rerun.stdout, /spec\s+emitted/);
  const result = await findRunArtifact(store, "diff-alpha-2", "result.json");
  assert(result !== undefined);
  assertEquals(
    await Deno.readTextFile(result),
    "40",
    "must run alpha (40), not the mis-pointed beta (60)",
  );
});
