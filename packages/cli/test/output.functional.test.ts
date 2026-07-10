import { assertEquals, assertStringIncludes } from "jsr:@std/assert";
import {
  copyFixture,
  fixtureEntry,
  listStoreKeys,
  makeStore,
  runCli,
} from "./harness.ts";

// WS-6.3 — default output is author-facing (no hashes, no store paths); verbose
// reveals artifact keys + hashes; `inspect` reports a past run without
// re-executing anything.

Deno.test("default run output hides hashes and store paths", async () => {
  const fixture = await copyFixture("linear-chain");
  const store = await makeStore();

  const result = await runCli([
    "run",
    fixtureEntry(fixture),
    "--input",
    "20",
    "--store",
    store,
    "--project",
    "acme/wf",
    "--run-id",
    "run-quiet",
  ]);

  assertEquals(result.code, 0, result.stderr);
  // The result value is surfaced...
  assertStringIncludes(result.stdout, "value:41");
  // ...but not digests or absolute datastore paths.
  assertEquals(
    result.stdout.includes("sha256"),
    false,
    "quiet output must not print digests",
  );
  assertEquals(
    result.stdout.includes(store),
    false,
    "quiet output must not print the absolute store path",
  );
});

Deno.test("verbose run output reveals specHash, planHash, and artifact keys", async () => {
  const fixture = await copyFixture("linear-chain");
  const store = await makeStore();

  const result = await runCli([
    "run",
    fixtureEntry(fixture),
    "--input",
    "20",
    "--store",
    store,
    "--project",
    "acme/wf",
    "--run-id",
    "run-verbose",
    "--verbose",
  ]);

  assertEquals(result.code, 0, result.stderr);
  assertStringIncludes(result.stdout, "specHash");
  assertStringIncludes(result.stdout, "planHash");
  assertStringIncludes(result.stdout, "sha256");
  // A project-scoped result key is disclosed under verbose.
  assertStringIncludes(result.stdout, "result.json");
});

Deno.test("massive inspect reports a past run without re-executing", async () => {
  const fixture = await copyFixture("linear-chain");
  const store = await makeStore();
  const runId = "run-inspect";

  const run = await runCli([
    "run",
    fixtureEntry(fixture),
    "--input",
    "20",
    "--store",
    store,
    "--project",
    "acme/wf",
    "--run-id",
    runId,
  ]);
  assertEquals(run.code, 0, run.stderr);

  const before = await listStoreKeys(store);

  const inspect = await runCli([
    "inspect",
    runId,
    "--store",
    store,
    "--project",
    "acme/wf",
  ]);
  assertEquals(inspect.code, 0, inspect.stderr);

  // inspect surfaces keys/hashes for the past run...
  assertStringIncludes(inspect.stdout, "result.json");
  assertStringIncludes(inspect.stdout, "sha256");

  // ...and writes no new run artifacts (no new run dir, no step spawned).
  const after = await listStoreKeys(store);
  assertEquals(after, before, "inspect must not create datastore artifacts");
});

Deno.test("massive inspect rejects an unsafe run id before touching the filesystem", async () => {
  const store = await makeStore();

  const result = await runCli([
    "inspect",
    "../../../../etc",
    "--store",
    store,
    "--project",
    "acme/wf",
  ]);

  // A run id that is not a single safe path segment is a usage error (exit 2),
  // caught at the entry — not interpolated into a stat path.
  assertEquals(result.code, 2, result.stderr);
  assertStringIncludes(result.stderr, "invalid run id");
  assertStringIncludes(result.stderr, "next");
});

Deno.test("massive inspect errors when a run id exists under multiple projects", async () => {
  const fixture = await copyFixture("linear-chain");
  const store = await makeStore();
  const runId = "dup-run";

  // Same run id under two different projects -> two run dirs in the store.
  for (const project of ["acme/one", "acme/two"]) {
    const run = await runCli([
      "run",
      fixtureEntry(fixture),
      "--input",
      "20",
      "--store",
      store,
      "--project",
      project,
      "--run-id",
      runId,
    ]);
    assertEquals(run.code, 0, run.stderr);
  }

  // The manifest records only the normalized project key, so --project cannot be
  // matched without reimplementing that normalization: inspect must refuse and
  // list the candidates rather than silently pick the first.
  const inspect = await runCli([
    "inspect",
    runId,
    "--store",
    store,
    "--project",
    "acme/one",
  ]);
  assertEquals(inspect.code, 4, inspect.stderr);
  assertStringIncludes(inspect.stderr, "multiple projects");
  assertStringIncludes(inspect.stderr, "next");
});
