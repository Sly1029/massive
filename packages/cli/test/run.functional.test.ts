import { assert, assertEquals, assertStringIncludes } from "jsr:@std/assert";
import { basename, dirname } from "node:path";
import { parseWorkflowSpecText } from "@massive/sdk";
import {
  copyFixture,
  exists,
  findRunArtifact,
  fixtureEntry,
  join,
  listStoreKeys,
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

  // A step exposes its output only through an immutable manifest. The body is
  // content-addressed outside the mutable run prefix and both refs are kept
  // in the journal for inspection/recovery.
  const doubleManifest = await findRunArtifact(
    store,
    runId,
    join("steps", "double", "1", "output-manifest.json"),
  );
  assert(
    doubleManifest !== undefined,
    "steps/double/1/output-manifest.json should exist",
  );

  // Run manifest records a succeeded run.
  const manifestPath = await findRunArtifact(store, runId, "run-manifest.json");
  assert(manifestPath !== undefined, "run-manifest.json should exist");
  const manifest = JSON.parse(await Deno.readTextFile(manifestPath)) as {
    readonly schemaVersion: number;
    readonly encoding: string;
    readonly status: string;
    readonly steps: readonly {
      readonly nodeId: string;
      readonly status: string;
      readonly attempts: readonly {
        readonly output?: {
          readonly manifest: { readonly key: string };
          readonly body: { readonly key: string };
        };
      }[];
    }[];
  };
  assertEquals(manifest.schemaVersion, 1);
  assertEquals(manifest.encoding, "json-v1");
  assertEquals(manifest.status, "succeeded");
  assertEquals(manifest.steps.map((step) => step.nodeId), [
    "double",
    "increment",
    "label",
  ]);
  const doubleOutput = manifest.steps[0].attempts[0].output;
  assert(
    doubleOutput !== undefined,
    "double attempt should journal its output",
  );
  assertEquals(
    await Deno.readTextFile(join(store, doubleOutput.body.key)),
    "40",
  );
});

Deno.test("massive run Python graph: same compiler, runner, and frozen artifact path", async () => {
  const fixture = await copyFixture("python-linear");
  const store = await makeStore();

  const result = await runCli([
    "run",
    `${basename(fixture)}/workflow.py#graph`,
    "--input",
    '{"value":20}',
    "--store",
    store,
    "--project",
    "acme/python-workflow",
    "--run-id",
    "python-linear-run",
    "--verbose",
  ], { cwd: dirname(fixture) });

  assertEquals(result.code, 0, result.stderr);
  assertStringIncludes(result.stdout, "add_one");
  assertStringIncludes(result.stdout, '{"value":21}');

  const keys = await listStoreKeys(store);
  const specKey = keys.find((key) => key.endsWith("/workflow-spec.json"));
  const planKey = keys.find((key) => key.endsWith("/workflow.json"));
  const outputManifestKey = keys.find((key) =>
    key.endsWith("output-manifest.json")
  );
  assert(specKey !== undefined, "canonical WorkflowSpec should be persisted");
  assert(planKey !== undefined, "compiled WorkflowPlan should be persisted");
  assert(
    outputManifestKey !== undefined,
    "Python output should be visible through a committed manifest",
  );
  assertEquals(keys.some((key) => key.includes("/source.tar")), true);

  const spec = await parseWorkflowSpecText(
    await Deno.readTextFile(join(store, specKey)),
  );
  assertEquals(spec.graph.irVersion, "0.1");
  assertEquals(spec.sourcePackages["python-main"]?.language, "python");

  const plan = JSON.parse(await Deno.readTextFile(join(store, planKey))) as {
    graph: { nodes: { kind: string; symbolRef?: string }[] };
    symbols: {
      language: string;
      module: string;
      export: string;
      packageId: string;
      symbolRef: string;
    }[];
    environments: { kind: string; container?: { image: string } }[];
  };
  assertEquals(plan.symbols, [{
    export: "add_one",
    language: "python",
    module: "workflow",
    packageId: "python-main",
    symbolRef: "python-main:workflow#add_one",
  }]);
  assertEquals(
    plan.graph.nodes.find((node) => node.kind === "step")?.symbolRef,
    "python-main:workflow#add_one",
  );
  assertEquals(plan.environments[0]?.kind, "container-plan");
  assertStringIncludes(
    plan.environments[0]?.container?.image ?? "",
    "python-runner@sha256:",
  );

  const outputManifest = JSON.parse(
    await Deno.readTextFile(join(store, outputManifestKey)),
  ) as { kind: string; producer: { nodeId: string; attempt: number } };
  assertEquals(outputManifest.kind, "DataArtifactManifest");
  assertEquals(outputManifest.producer.nodeId, "add_one");
  assertEquals(outputManifest.producer.attempt, 1);
});

Deno.test("massive run Python decision: selects one typed branch and journals the route", async () => {
  const fixture = await copyFixture("python-decision");
  const store = await makeStore();

  const result = await runCli([
    "run",
    join(fixture, "workflow.py"),
    "--input",
    '{"score":91}',
    "--store",
    store,
    "--project",
    "acme/python-decision",
    "--run-id",
    "python-decision-run",
    "--json",
  ]);

  assertEquals(result.code, 0, result.stderr);
  const outcome = JSON.parse(result.stdout) as {
    result: { message: string };
    steps: { nodeId: string; status: string }[];
  };
  assertEquals(outcome.result, { message: "approved:91" });
  assertEquals(outcome.steps, [
    { nodeId: "classify", status: "succeeded" },
    { nodeId: "approve", status: "succeeded" },
    { nodeId: "reject", status: "skipped" },
  ]);

  const manifestPath = await findRunArtifact(
    store,
    "python-decision-run",
    "run-manifest.json",
  );
  assert(manifestPath !== undefined, "decision run manifest should exist");
  const manifest = JSON.parse(await Deno.readTextFile(manifestPath)) as {
    schemaVersion: number;
    encoding: string;
    decisions: { nodeId: string; selectedCase: string }[];
  };
  assertEquals(manifest.decisions, [{
    nodeId: "route",
    selectedCase: "approved",
  }]);
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
