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
  assertEquals(manifest.schemaVersion, 3);
  assertEquals(manifest.encoding, "json-v3");
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

Deno.test("massive run Python map: persists scoped items and collects in source order", async (t) => {
  for (
    const testCase of [
      {
        name: "duplicates and out-of-order completion",
        runId: "python-map-ordered",
        input: '{"values":[3,1,3,2]}',
        expected: [
          { source: 3, doubled: 6 },
          { source: 1, doubled: 2 },
          { source: 3, doubled: 6 },
          { source: 2, doubled: 4 },
        ],
        itemCount: 4,
      },
      {
        name: "empty source",
        runId: "python-map-empty",
        input: '{"values":[]}',
        expected: [],
        itemCount: 0,
      },
    ] as const
  ) {
    await t.step(testCase.name, async () => {
      const fixture = await copyFixture("python-map");
      const store = await makeStore();
      const result = await runCli([
        "run",
        join(fixture, "workflow.py"),
        "--input",
        testCase.input,
        "--store",
        store,
        "--project",
        "acme/python-map",
        "--run-id",
        testCase.runId,
        "--json",
      ]);

      assertEquals(result.code, 0, result.stderr);
      const outcome = JSON.parse(result.stdout) as {
        result: readonly { source: number; doubled: number }[];
      };
      assertEquals(outcome.result, testCase.expected);

      const manifestPath = await findRunArtifact(
        store,
        testCase.runId,
        "run-manifest.json",
      );
      assert(manifestPath !== undefined, "map run manifest should exist");
      const manifest = JSON.parse(await Deno.readTextFile(manifestPath)) as {
        schemaVersion: number;
        encoding: string;
        steps: {
          nodeId: string;
          status: string;
          items?: {
            index: number;
            status: string;
            attempts: { output?: { manifest: { key: string } } }[];
          }[];
        }[];
      };
      assertEquals(manifest.schemaVersion, 3);
      assertEquals(manifest.encoding, "json-v3");

      const mapStep = manifest.steps.find((step) =>
        step.nodeId === "inspect-items"
      );
      assert(mapStep !== undefined, "map step should be journaled");
      assertEquals(mapStep.status, "succeeded");
      assertEquals(
        mapStep.items?.map((item) => item.index),
        Array.from({ length: testCase.itemCount }, (_, index) => index),
      );
      assertEquals(
        mapStep.items?.every((item) => item.status === "succeeded"),
        true,
      );

      const keys = await listStoreKeys(store);
      const itemManifests = keys.filter((key) =>
        key.includes("/steps/inspect-items/scopes/maps/inspect-items/items/") &&
        key.endsWith("/1/output-manifest.json")
      );
      assertEquals(itemManifests.length, testCase.itemCount);
      for (let index = 0; index < testCase.itemCount; index += 1) {
        assertEquals(
          itemManifests.some((key) => key.includes(`/items/${index}/1/`)),
          true,
        );
      }

      const collectedManifest = await findRunArtifact(
        store,
        testCase.runId,
        join("steps", "inspect-items", "1", "output-manifest.json"),
      );
      assert(
        collectedManifest !== undefined,
        "map collection should publish through the map node output slot",
      );
    });
  }
});

Deno.test("massive run Python map: preserves item failures without publishing a collection", async () => {
  const fixture = await copyFixture("python-map");
  const store = await makeStore();
  const runId = "python-map-partial-failure";
  const result = await runCli([
    "run",
    join(fixture, "workflow.py"),
    "--input",
    '{"values":[1,-1,3]}',
    "--store",
    store,
    "--project",
    "acme/python-map",
    "--run-id",
    runId,
    "--json",
  ]);

  assertEquals(result.code, 66, result.stderr);
  const manifestPath = await findRunArtifact(store, runId, "run-manifest.json");
  assert(manifestPath !== undefined, "failed map run should have a manifest");
  const manifestText = await Deno.readTextFile(manifestPath);
  assert(
    !manifestText.includes("private-fixture-payload"),
    "durable map diagnostics must not contain user exception text",
  );
  const manifest = JSON.parse(manifestText) as {
    status: string;
    steps: {
      nodeId: string;
      status: string;
      attempts: { status: string; output?: unknown }[];
      items?: {
        index: number;
        status: string;
        attempts: { status: string }[];
      }[];
    }[];
  };
  const map = manifest.steps.find((step) => step.nodeId === "inspect-items");
  assert(map !== undefined, "map node should be journaled");
  assertEquals(manifest.status, "failed");
  assertEquals(map.status, "failed");
  assertEquals(map.attempts[0]?.status, "failed");
  assertEquals(map.attempts[0]?.output, undefined);
  assertEquals(map.items?.map((item) => [item.index, item.status]), [
    [0, "succeeded"],
    [1, "failed"],
    [2, "succeeded"],
  ]);
  assertEquals(
    await findRunArtifact(
      store,
      runId,
      join("steps", "inspect-items", "1", "output-manifest.json"),
    ),
    undefined,
    "a partial map must not publish its collected output",
  );
});

for (
  const testCase of [
    {
      name: "selected branch composes two maps",
      mode: "mapped",
      runId: "python-map-branch-selected",
      expected: [{ value: 5 }, { value: 3 }],
      mapStatuses: ["succeeded", "succeeded"],
    },
    {
      name: "unselected branch skips both maps",
      mode: "bypass",
      runId: "python-map-branch-skipped",
      expected: [{ value: 2 }, { value: 1 }],
      mapStatuses: ["skipped", "skipped"],
    },
  ] as const
) {
  Deno.test(`massive run Python map: ${testCase.name}`, async () => {
    const fixture = await copyFixture("python-map-branches");
    const store = await makeStore();
    const result = await runCli([
      "run",
      join(fixture, "workflow.py"),
      "--input",
      JSON.stringify({ mode: testCase.mode, values: [2, 1] }),
      "--store",
      store,
      "--project",
      "acme/python-map-branches",
      "--run-id",
      testCase.runId,
      "--json",
    ]);

    assertEquals(result.code, 0, result.stderr);
    const outcome = JSON.parse(result.stdout) as {
      result: { value: number }[];
    };
    assertEquals(outcome.result, [...testCase.expected]);
    const manifestPath = await findRunArtifact(
      store,
      testCase.runId,
      "run-manifest.json",
    );
    assert(manifestPath !== undefined);
    const manifest = JSON.parse(await Deno.readTextFile(manifestPath)) as {
      steps: { nodeId: string; status: string; items?: unknown[] }[];
    };
    const maps = ["double-items", "increment-items"].map((nodeId) =>
      manifest.steps.find((step) => step.nodeId === nodeId)
    );
    assertEquals(maps.map((step) => step?.status), [...testCase.mapStatuses]);
    if (testCase.mode === "bypass") {
      assertEquals(maps.map((step) => step?.items), [[], []]);
    } else {
      assertEquals(maps.map((step) => step?.items?.length), [2, 2]);
    }
  });
}

Deno.test("massive run Python decision: selects the approved branch and journals the route", async () => {
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
    decisions: { nodeId: string; status: string; selectedCase?: string }[];
    steps: {
      nodeId: string;
      status: string;
      skipReason?: { kind: string; decisionId: string; case: string };
      attempts: { output?: { body: { key: string; hash: string } } }[];
    }[];
  };
  assertEquals(manifest.schemaVersion, 3);
  assertEquals(manifest.encoding, "json-v3");
  assertEquals(manifest.decisions, [{
    nodeId: "route",
    status: "selected",
    selectedCase: "approved",
  }]);
  assertEquals(
    manifest.steps.find((step) => step.nodeId === "reject")?.skipReason,
    {
      kind: "decision-not-selected",
      decisionId: "route",
      case: "rejected",
    },
  );
  const approvedBody = manifest.steps.find((step) => step.nodeId === "approve")
    ?.attempts[0]?.output?.body;
  assert(
    approvedBody !== undefined,
    "selected branch should publish one output body",
  );
  assertEquals(
    await Deno.readTextFile(join(store, approvedBody.key)),
    '{"message":"approved:91"}',
  );
  const keys = await listStoreKeys(store);
  assertEquals(
    keys.some((key) => key.includes("/steps/route-select/")),
    false,
    "select must alias the selected body rather than publish a synthetic step output",
  );
});

Deno.test("massive run Python decision: selects the rejected branch and skips approval", async () => {
  const fixture = await copyFixture("python-decision");
  const store = await makeStore();

  const result = await runCli([
    "run",
    join(fixture, "workflow.py"),
    "--input",
    '{"score":10}',
    "--store",
    store,
    "--project",
    "acme/python-decision",
    "--run-id",
    "python-decision-rejected",
    "--json",
  ]);

  assertEquals(result.code, 0, result.stderr);
  const outcome = JSON.parse(result.stdout) as {
    result: { message: string };
    steps: { nodeId: string; status: string }[];
  };
  assertEquals(outcome.result, { message: "rejected:score below threshold" });
  assertEquals(outcome.steps, [
    { nodeId: "classify", status: "succeeded" },
    { nodeId: "approve", status: "skipped" },
    { nodeId: "reject", status: "succeeded" },
  ]);

  const manifestPath = await findRunArtifact(
    store,
    "python-decision-rejected",
    "run-manifest.json",
  );
  assert(manifestPath !== undefined, "decision run manifest should exist");
  const manifest = JSON.parse(await Deno.readTextFile(manifestPath)) as {
    decisions: { nodeId: string; status: string; selectedCase?: string }[];
    steps: {
      nodeId: string;
      skipReason?: { kind: string; decisionId: string; case: string };
      attempts: { output?: { body: { key: string } } }[];
    }[];
  };
  assertEquals(manifest.decisions, [{
    nodeId: "route",
    status: "selected",
    selectedCase: "rejected",
  }]);
  assertEquals(
    manifest.steps.find((step) => step.nodeId === "approve")?.skipReason,
    {
      kind: "decision-not-selected",
      decisionId: "route",
      case: "approved",
    },
  );
  const rejectedBody = manifest.steps.find((step) => step.nodeId === "reject")
    ?.attempts[0]?.output?.body;
  assert(
    rejectedBody !== undefined,
    "selected branch should publish one output body",
  );
  assertEquals(
    await Deno.readTextFile(join(store, rejectedBody.key)),
    '{"message":"rejected:score below threshold"}',
  );
  const keys = await listStoreKeys(store);
  assertEquals(
    keys.some((key) => key.includes("/steps/route-select/")),
    false,
    "select must alias the selected body rather than publish a synthetic step output",
  );
});

Deno.test("massive run Python nested decisions activate only the selected control region", async () => {
  const cases = [
    {
      input: '{"score":95}',
      runId: "python-nested-fast",
      message: "fast:95",
      selectedNode: "fast",
      selected: [
        { nodeId: "outer-route", status: "selected", selectedCase: "approved" },
        { nodeId: "inner-route", status: "selected", selectedCase: "fast" },
      ],
      statuses: {
        classify_outer: "succeeded",
        review: "succeeded",
        fast: "succeeded",
        manual: "skipped",
        reject: "skipped",
      },
    },
    {
      input: '{"score":75}',
      runId: "python-nested-manual",
      message: "manual:75",
      selectedNode: "manual",
      selected: [
        { nodeId: "outer-route", status: "selected", selectedCase: "approved" },
        { nodeId: "inner-route", status: "selected", selectedCase: "manual" },
      ],
      statuses: {
        classify_outer: "succeeded",
        review: "succeeded",
        fast: "skipped",
        manual: "succeeded",
        reject: "skipped",
      },
    },
    {
      input: '{"score":10}',
      runId: "python-nested-rejected",
      message: "rejected:score below threshold",
      selectedNode: "reject",
      selected: [
        { nodeId: "outer-route", status: "selected", selectedCase: "rejected" },
        {
          nodeId: "inner-route",
          status: "skipped",
          skipReason: {
            kind: "decision-not-selected",
            decisionId: "outer-route",
            case: "approved",
          },
        },
      ],
      statuses: {
        classify_outer: "succeeded",
        review: "skipped",
        fast: "skipped",
        manual: "skipped",
        reject: "succeeded",
      },
    },
  ] as const;

  for (const testCase of cases) {
    const fixture = await copyFixture("python-nested-decision");
    const store = await makeStore();
    const result = await runCli([
      "run",
      join(fixture, "workflow.py"),
      "--input",
      testCase.input,
      "--store",
      store,
      "--project",
      "acme/python-nested-decision",
      "--run-id",
      testCase.runId,
      "--json",
    ]);

    assertEquals(result.code, 0, result.stderr);
    const outcome = JSON.parse(result.stdout) as {
      result: { message: string };
      steps: { nodeId: string; status: string }[];
    };
    assertEquals(outcome.result, { message: testCase.message });
    assertEquals(
      Object.fromEntries(
        outcome.steps.map((step) => [step.nodeId, step.status]),
      ),
      testCase.statuses,
    );

    const manifestPath = await findRunArtifact(
      store,
      testCase.runId,
      "run-manifest.json",
    );
    assert(
      manifestPath !== undefined,
      "nested decision run manifest should exist",
    );
    const manifest = JSON.parse(await Deno.readTextFile(manifestPath)) as {
      decisions: readonly unknown[];
      result?: { key: string; hash: string };
      steps: {
        nodeId: string;
        attempts: { output?: { body: { key: string; hash: string } } }[];
      }[];
    };
    assertEquals(manifest.decisions, testCase.selected);

    const selectedBody = manifest.steps.find((step) =>
      step.nodeId === testCase.selectedNode
    )?.attempts[0]?.output?.body;
    assert(
      selectedBody !== undefined,
      "selected branch should publish its body",
    );
    assert(manifest.result !== undefined, "run should journal a final result");
    assertEquals(
      manifest.result.hash,
      selectedBody.hash,
      "the final result must be the selected branch value",
    );
    assertEquals(
      await Deno.readTextFile(join(store, manifest.result.key)),
      await Deno.readTextFile(join(store, selectedBody.key)),
      "the final result body must equal the selected branch body",
    );

    const keys = await listStoreKeys(store);
    assertEquals(
      keys.some((key) => key.includes("/steps/inner-route-select/")),
      false,
      "nested select must alias its chosen branch body",
    );
    assertEquals(
      keys.some((key) => key.includes("/steps/outer-route-select/")),
      false,
      "outer select must alias its chosen branch body",
    );
  }
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
