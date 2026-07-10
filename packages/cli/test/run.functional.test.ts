import { assert, assertEquals, assertStringIncludes } from "jsr:@std/assert";
import {
  copyFixture,
  exists,
  findRunArtifact,
  fixtureEntry,
  join,
  makeStore,
  repoRoot,
  runCli,
} from "./harness.ts";

// WS-6.1 — `massive run` drives the full compiled-artifact path end to end
// (SDK emit -> persist spec -> Go orchestrator -> Deno step runner -> read
// artifacts) and produces REAL datastore outputs at the frozen keys. There is
// no in-memory execution path.

Deno.test("massive run linear-chain: exit 0, per-step output, real frozen artifacts", async () => {
  const fixture = await copyFixture("linear-chain");
  const store = await makeStore();
  const runId = "run-e2e";

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
    runId,
  ]);

  assertEquals(result.code, 0, result.stderr);

  // Author-facing per-step status and the final result value.
  assertStringIncludes(result.stdout, "double");
  assertStringIncludes(result.stdout, "increment");
  assertStringIncludes(result.stdout, "label");
  assertStringIncludes(result.stdout, "succeeded");
  assertStringIncludes(result.stdout, "value:41");

  // Real result artifact at projects/<project-key>/runs/run-e2e/result.json.
  const resultPath = await findRunArtifact(store, runId, "result.json");
  assert(resultPath !== undefined, "result.json should exist under the run");
  assertEquals(await Deno.readTextFile(resultPath), `"value:41"`);

  // Per-step output artifact at steps/<id>/1/output.json.
  const doubleOutput = await findRunArtifact(
    store,
    runId,
    join("steps", "double", "1", "output.json"),
  );
  assert(doubleOutput !== undefined, "steps/double/1/output.json should exist");

  // Run manifest records a succeeded run.
  const manifestPath = await findRunArtifact(store, runId, "run-manifest.json");
  assert(manifestPath !== undefined, "run-manifest.json should exist");
  const manifest = JSON.parse(await Deno.readTextFile(manifestPath)) as {
    readonly status: string;
    readonly steps: readonly {
      readonly nodeId: string;
      readonly status: string;
    }[];
  };
  assertEquals(manifest.status, "succeeded");
  assertEquals(manifest.steps.map((step) => step.nodeId), [
    "double",
    "increment",
    "label",
  ]);
});

Deno.test("massive run diamond: fan-in result 81 at the frozen result key", async () => {
  const fixture = await copyFixture("diamond");
  const store = await makeStore();
  const runId = "run-diamond-e2e";

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
    runId,
  ]);

  assertEquals(result.code, 0, result.stderr);

  const resultPath = await findRunArtifact(store, runId, "result.json");
  assert(resultPath !== undefined, "diamond result.json should exist");
  assertEquals(await Deno.readTextFile(resultPath), `81`);

  const mergeInput = await findRunArtifact(
    store,
    runId,
    join("inputs", "merge.json"),
  );
  assert(mergeInput !== undefined, "merge fan-in input should exist");
  assertEquals(await Deno.readTextFile(mergeInput), `[21,60]`);
});

Deno.test("the in-memory SDK run path is gone (packages/sdk/src/run.ts absent)", async () => {
  assertEquals(
    await exists(join(repoRoot(), "packages", "sdk", "src", "run.ts")),
    false,
  );
});
