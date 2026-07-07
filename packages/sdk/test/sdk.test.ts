import {
  assert,
  assertEquals,
  assertNotEquals,
  assertRejects,
} from "jsr:@std/assert";
import { rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { z } from "zod";
import {
  datastore,
  DatastoreKeyError,
  emitWorkflowSpec,
  GraphValidationError,
  SchemaPortabilityError,
  workflow,
  type WorkflowSpec,
} from "../src/index.ts";
import { batchMergeGraphCase, graphCases } from "./graph-fixtures.ts";

Deno.test("linear workflow builds runtime registry and emits graph spec", async () => {
  const g = workflow({
    name: "math",
    input: z.number(),
    output: z.string(),
  });

  const double = g.step("double", {
    input: z.number(),
    output: z.number(),
    run: async ({ input }) => input * 2,
  });

  const stringify = g.step("stringify", {
    input: z.number(),
    output: z.string(),
    run: async ({ input }) => `Result: ${input}`,
  });

  g.start().to(double).to(stringify).to(g.end());

  const spec = await emitWorkflowSpec(g, { source: sourceSpec() });

  assertEquals(g.runtimeRegistry.size, 2);
  assertEquals(spec.kind, "WorkflowSpec");
  assertEquals(spec.encoding, "json-v0");
  assertEquals(spec.schemaVersion, 0);
  assertEquals(spec.workflow.name, "math");
  assertEquals(stepNodes(spec).map((node) => node.id), [
    "double",
    "stringify",
  ]);
  assertEquals(
    spec.graph.edges,
    [
      { from: "__start", to: "double" },
      { from: "double", to: "stringify" },
      { from: "stringify", to: "__end" },
    ],
  );
  assertEquals(spec.symbols["ts-main:./workflow.ts#double"], {
    packageId: "ts-main",
    language: "typescript",
    module: "./workflow.ts",
    export: "double",
  });
});

Deno.test("WorkflowSpec emission is deterministic for identical source", async () => {
  const first = await emitWorkflowSpec(makeMathWorkflow(), {
    source: sourceSpec(),
  });
  const second = await emitWorkflowSpec(makeMathWorkflow(), {
    source: sourceSpec(),
  });

  assertEquals(second.specHash, first.specHash);
  assertEquals(second, first);
});

Deno.test("included source changes update source package and spec hashes", async () => {
  await withTempDir(async (root) => {
    const sourceRoot = join(root, "source");
    await Deno.mkdir(sourceRoot, { recursive: true });
    await writeFile(
      join(sourceRoot, "workflow.ts"),
      "export const version = 1;\n",
    );
    await writeFile(
      join(sourceRoot, "untracked.ts"),
      "export const version = 1;\n",
    );

    const source = { root: sourceRoot, include: ["workflow.ts"] };
    const first = await emitWorkflowSpec(makeMathWorkflow(), { source });

    await writeFile(
      join(sourceRoot, "workflow.ts"),
      "export const version = 2;\n",
    );
    const second = await emitWorkflowSpec(makeMathWorkflow(), { source });

    assertNotEquals(
      sourcePackage(first).packageHash,
      sourcePackage(second).packageHash,
    );
    assertNotEquals(first.specHash, second.specHash);

    await writeFile(
      join(sourceRoot, "untracked.ts"),
      "export const version = 2;\n",
    );
    const third = await emitWorkflowSpec(makeMathWorkflow(), { source });
    assertEquals(third.specHash, second.specHash);
  });
});

Deno.test("non-portable Zod schemas fail emission with schema diagnostics", async () => {
  const g = workflow({
    name: "bad-schema",
    input: z.string().transform((value) => value.length),
    output: z.number(),
  });
  g.start()
    .to(g.step("noop", {
      input: z.number(),
      output: z.number(),
      run: ({ input }) => input,
    }))
    .to(g.end());

  await assertRejects(
    () => emitWorkflowSpec(g, { source: sourceSpec() }),
    SchemaPortabilityError,
    "bad-schema.input",
  );
});

Deno.test("graph validation rejects cycles and missing paths to end", async () => {
  const cyclic = workflow({
    name: "cyclic",
    input: z.number(),
    output: z.number(),
  });
  const one = cyclic.step("one", {
    input: z.number(),
    output: z.number(),
    run: ({ input }) => input,
  });
  const two = cyclic.step("two", {
    input: z.number(),
    output: z.number(),
    run: ({ input }) => input,
  });
  cyclic.start().to(one).to(two).to(one);

  await assertRejects(
    () => emitWorkflowSpec(cyclic, { source: sourceSpec() }),
    GraphValidationError,
    "cycle",
  );

  const missingEnd = workflow({
    name: "missing-end",
    input: z.number(),
    output: z.number(),
  });
  missingEnd.start().to(
    missingEnd.step("one", {
      input: z.number(),
      output: z.number(),
      run: ({ input }) => input,
    }),
  );

  await assertRejects(
    () => emitWorkflowSpec(missingEnd, { source: sourceSpec() }),
    GraphValidationError,
    "cannot reach end",
  );

  const noEdges = workflow({
    name: "no-edges",
    input: z.number(),
    output: z.number(),
  });
  await assertRejects(
    () => emitWorkflowSpec(noEdges, { source: sourceSpec() }),
    GraphValidationError,
    "End is not reachable from start",
  );
});

Deno.test("local datastore rejects traversal keys and writes exact bytes", async () => {
  await withTempDir(async (root) => {
    const store = datastore.local({ path: join(root, "store") });
    await store.put("objects/abc", "hello");

    assertEquals(
      new TextDecoder().decode(await store.get("objects/abc")),
      "hello",
    );
    await assertRejects(() => store.put("../escape", "bad"), DatastoreKeyError);
    await assertRejects(
      () => store.put("objects/../escape", "bad"),
      DatastoreKeyError,
    );
    await assertRejects(() => store.put("/absolute", "bad"), DatastoreKeyError);
  });
});

Deno.test("graph catalog emits topology and merge metadata consistently", async () => {
  for (const graphCase of graphCases) {
    const spec = await emitWorkflowSpec(graphCase.build(), {
      source: sourceSpec(),
    });

    assertEquals(
      stepNodes(spec).length,
      graphCase.expectedTasks,
      graphCase.name,
    );
    assertEquals(
      spec.graph.edges.length,
      graphCase.expectedEdges,
      graphCase.name,
    );

    for (
      const [stepId, mergeInputs] of Object.entries(
        graphCase.mergeExpectations ?? {},
      )
    ) {
      assertEquals(
        stepNode(spec, stepId)?.mergeInputs,
        mergeInputs,
        graphCase.name,
      );
    }
  }
});

Deno.test("fan-out fan-in diamond graph emits merge metadata", async () => {
  const spec = await emitWorkflowSpec(graphCases[3]!.build(), {
    source: sourceSpec(),
  });

  assertEquals(stepNode(spec, "merge")?.mergeInputs, ["left", "right"]);
  assert(
    spec.graph.edges.some((edge) =>
      edge.from === "left" && edge.to === "merge"
    ),
  );
  assert(
    spec.graph.edges.some((edge) =>
      edge.from === "right" && edge.to === "merge"
    ),
  );
});

Deno.test("100-way batch split and merge emits stable topology", async () => {
  const graphCase = batchMergeGraphCase(100);
  const spec = await emitWorkflowSpec(graphCase.build(), {
    source: sourceSpec(),
  });
  const mergeInputs = stepNode(spec, "merge")?.mergeInputs;

  assertEquals(spec.graph.nodes.length, 104);
  assertEquals(stepNodes(spec).length, 102);
  assertEquals(spec.graph.edges.length, 202);
  assertEquals(mergeInputs?.length, 100);
  assertEquals(mergeInputs?.at(0), "worker-000");
  assertEquals(mergeInputs?.at(99), "worker-099");
  assertEquals(
    spec.graph.edges.filter((edge) => edge.from === "split").length,
    100,
  );
  assertEquals(
    spec.graph.edges.filter((edge) => edge.to === "merge").length,
    100,
  );
});

function makeMathWorkflow() {
  const g = workflow({ name: "math", input: z.number(), output: z.string() });
  const double = g.step("double", {
    input: z.number(),
    output: z.number(),
    run: ({ input }) => input * 2,
  });
  const stringify = g.step("stringify", {
    input: z.number(),
    output: z.string(),
    run: ({ input }) => `Result: ${input}`,
  });
  g.start().to(double).to(stringify).to(g.end());
  return g;
}

function sourceSpec() {
  return {
    root: new URL(".", import.meta.url).pathname,
    include: ["fixtures/**/*.ts"],
  };
}

function sourcePackage(
  spec: WorkflowSpec,
): WorkflowSpec["sourcePackages"][string] {
  return spec.sourcePackages["ts-main"]!;
}

function stepNodes(
  spec: WorkflowSpec,
): Extract<
  WorkflowSpec["graph"]["nodes"][number],
  { readonly kind: "step" }
>[] {
  return spec.graph.nodes.filter((node) => node.kind === "step");
}

function stepNode(
  spec: WorkflowSpec,
  id: string,
):
  | Extract<WorkflowSpec["graph"]["nodes"][number], { readonly kind: "step" }>
  | undefined {
  return stepNodes(spec).find((node) => node.id === id);
}

async function withTempDir(
  callback: (path: string) => Promise<void>,
): Promise<void> {
  const root = await Deno.makeTempDir({ prefix: "massive-sdk-" });
  try {
    await callback(root);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
}
