import { assert, assertEquals, assertRejects } from "jsr:@std/assert";
import { rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { z } from "zod";
import {
  channel,
  compile,
  compileArgoWorkflow,
  datastore,
  DatastoreKeyError,
  GraphValidationError,
  SchemaPortabilityError,
  stateSchema,
  workflow,
} from "../src/index.ts";
import { graphCases } from "./graph-fixtures.ts";

Deno.test("linear workflow compiles and writes plan artifacts", async () => {
  await withTempDir(async (root) => {
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

    const store = datastore.local({ path: join(root, "store") });
    const compiled = await compile(g, {
      target: "local",
      datastore: store,
      source: sourceSpec(),
    });

    assertEquals(g.runtimeRegistry.size, 2);
    assertEquals(await store.exists(`plans/${compiled.planHash}/workflow.json`), true);

    const planBytes = await store.get(`plans/${compiled.planHash}/workflow.json`);
    const plan = JSON.parse(new TextDecoder().decode(planBytes));
    assertEquals(plan.encoding, "json-v0");
    assertEquals(plan.schemaVersion, 0);
    assertEquals(plan.planHash, compiled.planHash);
    assertEquals(plan.graph.nodes.filter((node: { kind: string }) => node.kind === "step").length, 2);
  });
});

Deno.test("compilation is deterministic for identical source", async () => {
  await withTempDir(async (root) => {
    const store = datastore.local({ path: join(root, "store") });
    const first = await compile(makeMathWorkflow(), { target: "local", datastore: store, source: sourceSpec() });
    const second = await compile(makeMathWorkflow(), { target: "local", datastore: store, source: sourceSpec() });

    assertEquals(second.planHash, first.planHash);
    assertEquals(second.plan, first.plan);
  });
});

Deno.test("included source changes update source, symbol, and plan hashes", async () => {
  await withTempDir(async (root) => {
    const sourceRoot = join(root, "source");
    await Deno.mkdir(sourceRoot, { recursive: true });
    await writeFile(join(sourceRoot, "workflow.ts"), "export const version = 1;\n");
    await writeFile(join(sourceRoot, "untracked.ts"), "export const version = 1;\n");

    const store = datastore.local({ path: join(root, "store") });
    const source = { root: sourceRoot, include: ["workflow.ts"] };
    const first = await compile(makeMathWorkflow(), { target: "local", datastore: store, source });

    await writeFile(join(sourceRoot, "workflow.ts"), "export const version = 2;\n");
    const second = await compile(makeMathWorkflow(), { target: "local", datastore: store, source });

    assert(first.plan.source.sourcePackageHash !== second.plan.source.sourcePackageHash);
    assert(first.plan.symbols.symbolManifestHash !== second.plan.symbols.symbolManifestHash);
    assert(first.planHash !== second.planHash);

    await writeFile(join(sourceRoot, "untracked.ts"), "export const version = 2;\n");
    const third = await compile(makeMathWorkflow(), { target: "local", datastore: store, source });
    assertEquals(third.planHash, second.planHash);
  });
});

Deno.test("non-portable Zod schemas fail compilation with schema diagnostics", async () => {
  await withTempDir(async (root) => {
    const g = workflow({
      name: "bad-schema",
      input: z.string().transform((value) => value.length),
      output: z.number(),
    });
    g.start().to(g.step("noop", { input: z.number(), output: z.number(), run: ({ input }) => input })).to(g.end());

    await assertRejects(
      () =>
        compile(g, {
          target: "local",
          datastore: datastore.local({ path: join(root, "store") }),
          source: sourceSpec(),
        }),
      SchemaPortabilityError,
      "bad-schema.input"
    );
  });
});

Deno.test("graph validation rejects cycles and missing paths to end", async () => {
  await withTempDir(async (root) => {
    const compileOptions = {
      target: "local" as const,
      datastore: datastore.local({ path: join(root, "store") }),
      source: sourceSpec(),
    };

    const cyclic = workflow({ name: "cyclic", input: z.number(), output: z.number() });
    const one = cyclic.step("one", { input: z.number(), output: z.number(), run: ({ input }) => input });
    const two = cyclic.step("two", { input: z.number(), output: z.number(), run: ({ input }) => input });
    cyclic.start().to(one).to(two).to(one);

    await assertRejects(() => compile(cyclic, compileOptions), GraphValidationError, "cycle");

    const missingEnd = workflow({ name: "missing-end", input: z.number(), output: z.number() });
    missingEnd.start().to(
      missingEnd.step("one", { input: z.number(), output: z.number(), run: ({ input }) => input })
    );

    await assertRejects(() => compile(missingEnd, compileOptions), GraphValidationError, "cannot reach end");

    const noEdges = workflow({ name: "no-edges", input: z.number(), output: z.number() });
    await assertRejects(() => compile(noEdges, compileOptions), GraphValidationError, "End is not reachable from start");
  });
});

Deno.test("local datastore rejects traversal keys and writes exact bytes", async () => {
  await withTempDir(async (root) => {
    const store = datastore.local({ path: join(root, "store") });
    await store.put("objects/abc", "hello");

    assertEquals(new TextDecoder().decode(await store.get("objects/abc")), "hello");
    await assertRejects(() => store.put("../escape", "bad"), DatastoreKeyError);
    await assertRejects(() => store.put("objects/../escape", "bad"), DatastoreKeyError);
    await assertRejects(() => store.put("/absolute", "bad"), DatastoreKeyError);
  });
});

Deno.test("minimal channel publish compiles named channel plan", async () => {
  await withTempDir(async (root) => {
    const State = stateSchema({
      answer: channel(z.number()),
    });
    const g = workflow({ name: "channels", input: z.number(), output: z.number(), state: State });
    const publish = g.step("publish", {
      input: z.number(),
      output: z.number(),
      channel: "answer",
      run: ({ input }) => input + 1,
    });
    g.start().to(publish).to(g.end());

    const store = datastore.local({ path: join(root, "store") });
    const compiled = await compile(g, { target: "local", datastore: store, source: sourceSpec() });
    const publishNode = compiled.plan.graph.nodes.find((node) => node.id === "publish");

    assertEquals(compiled.plan.channels.answer?.reducer, "last");
    assertEquals(publishNode?.kind === "step" ? publishNode.channel : undefined, "answer");
    assertEquals(await store.exists(`plans/${compiled.planHash}/workflow.json`), true);
  });
});

Deno.test("graph catalog compiles consistently", async () => {
  await withTempDir(async (root) => {
    for (const graphCase of graphCases) {
      const store = datastore.local({ path: join(root, "async", graphCase.name) });
      const compiled = await compile(graphCase.build(), { target: "local", datastore: store, source: sourceSpec() });

      assertEquals(compiled.plan.graph.nodes.filter((node) => node.kind === "step").length, graphCase.expectedTasks);
      assertEquals(compiled.plan.graph.edges.length, graphCase.expectedEdges);
      assertEquals(await store.exists(`plans/${compiled.planHash}/workflow.json`), true);

      for (const [stepId, mergeInputs] of Object.entries(graphCase.mergeExpectations ?? {})) {
        const node = compiled.plan.graph.nodes.find((candidate) => candidate.id === stepId);
        assertEquals(node?.kind === "step" ? node.mergeInputs : undefined, mergeInputs, graphCase.name);
      }
    }
  });
});

Deno.test("graph catalog emits Argo DAG manifests consistently", async () => {
  await withTempDir(async (root) => {
    for (const graphCase of graphCases) {
      const store = datastore.local({ path: join(root, "argo-local", graphCase.name) });
      const compiled = await compile(graphCase.build(), { target: "local", datastore: store, source: sourceSpec() });
      const manifest = compileArgoWorkflow(compiled);
      const main = mainTemplate(manifest);

      assertEquals(main.dag.tasks.length, graphCase.expectedTasks, graphCase.name);

      for (const [stepId, mergeInputs] of Object.entries(graphCase.mergeExpectations ?? {})) {
        const task = main.dag.tasks.find((candidate) => candidate.name === stepId);
        assertEquals(task?.dependencies, [...mergeInputs].sort(), graphCase.name);
      }
    }
  });
});

Deno.test("fan-out fan-in diamond graph compiles merge metadata", async () => {
  await withTempDir(async (root) => {
    const g = workflow({ name: "diamond", input: z.number(), output: z.number() });
    const split = g.step("split", {
      input: z.number(),
      output: z.number(),
      run: ({ input }) => input,
    });
    const left = g.step("left", {
      input: z.number(),
      output: z.number(),
      run: async ({ input }) => input + 1,
    });
    const right = g.step("right", {
      input: z.number(),
      output: z.number(),
      run: async ({ input }) => input * 10,
    });
    const merge = g.step("merge", {
      input: z.array(z.number()),
      output: z.number(),
      run: ({ input }) => input.reduce((sum, value) => sum + value, 0),
    });

    g.start().to(split);
    g.from(split).to(left);
    g.from(split).to(right);
    g.merge([left, right]).to(merge).to(g.end());

    const compiled = await compile(g, {
      target: "local",
      datastore: datastore.local({ path: join(root, "store") }),
      source: sourceSpec(),
    });

    assertEquals(compiled.plan.graph.nodes.find((node) => node.id === "merge")?.kind, "step");
    const mergeNode = compiled.plan.graph.nodes.find((node) => node.id === "merge");
    assertEquals(mergeNode?.kind === "step" ? mergeNode.mergeInputs : undefined, ["left", "right"]);
  });
});

Deno.test("100-way batch split and merge compiles stable graph shape", async () => {
  await withTempDir(async (root) => {
    const g = workflow({ name: "batch-merge", input: z.number(), output: z.number() });
    const split = g.step("split", {
      input: z.number(),
      output: z.number(),
      run: ({ input }) => input,
    });
    const workers = Array.from({ length: 100 }, (_, index) =>
      g.step(`worker-${String(index).padStart(3, "0")}`, {
        input: z.number(),
        output: z.number(),
        run: async ({ input }) => input + index,
      })
    );
    const merge = g.step("merge", {
      input: z.array(z.number()),
      output: z.number(),
      run: ({ input }) => input.reduce((sum, value) => sum + value, 0),
    });

    g.start().to(split);
    for (const worker of workers) {
      g.from(split).to(worker);
    }
    g.merge(workers).to(merge).to(g.end());

    const compiled = await compile(g, {
      target: "local",
      datastore: datastore.local({ path: join(root, "store") }),
      source: sourceSpec(),
    });

    assertEquals(compiled.plan.graph.nodes.length, 104);
    assertEquals(compiled.plan.graph.edges.length, 202);
  });
});

Deno.test("Argo DAG manifest includes diamond dependencies", async () => {
  await withTempDir(async (root) => {
    const compiled = await compile(makeDiamondWorkflow(), {
      target: "local",
      datastore: datastore.local({ path: join(root, "store") }),
      source: sourceSpec(),
    });

    const manifest = compileArgoWorkflow(compiled);
    const main = mainTemplate(manifest);
    const mergeTask = main.dag.tasks.find((task) => task.name === "merge");

    assertEquals(manifest.metadata.annotations["massive.dev/plan-hash"], compiled.planHash);
    assertEquals(mergeTask?.dependencies, ["left", "right"]);
  });
});

Deno.test("Argo DAG manifest includes 100-way batch dependencies", async () => {
  await withTempDir(async (root) => {
    const compiled = await compile(makeBatchMergeWorkflow(100), {
      target: "local",
      datastore: datastore.local({ path: join(root, "store") }),
      source: sourceSpec(),
    });

    const manifest = compileArgoWorkflow(compiled);
    const main = mainTemplate(manifest);
    const mergeTask = main.dag.tasks.find((task) => task.name === "merge");

    assertEquals(main.dag.tasks.length, 102);
    assertEquals(mergeTask?.dependencies?.length, 100);
    assertEquals(mergeTask?.dependencies?.at(0), "worker-000");
    assertEquals(mergeTask?.dependencies?.at(99), "worker-099");
  });
});

function makeMathWorkflow() {
  const g = workflow({ name: "math", input: z.number(), output: z.string() });
  const double = g.step("double", { input: z.number(), output: z.number(), run: ({ input }) => input * 2 });
  const stringify = g.step("stringify", {
    input: z.number(),
    output: z.string(),
    run: ({ input }) => `Result: ${input}`,
  });
  g.start().to(double).to(stringify).to(g.end());
  return g;
}

function mainTemplate(manifest: ReturnType<typeof compileArgoWorkflow>) {
  const template = manifest.spec.templates.find((candidate) => "dag" in candidate);
  if (template === undefined || !("dag" in template)) {
    throw new Error("Argo manifest does not contain a DAG template");
  }
  return template;
}

function makeDiamondWorkflow() {
  const g = workflow({ name: "diamond", input: z.number(), output: z.number() });
  const split = g.step("split", { input: z.number(), output: z.number(), run: ({ input }) => input });
  const left = g.step("left", { input: z.number(), output: z.number(), run: ({ input }) => input + 1 });
  const right = g.step("right", { input: z.number(), output: z.number(), run: ({ input }) => input * 10 });
  const merge = g.step("merge", {
    input: z.array(z.number()),
    output: z.number(),
    run: ({ input }) => input.reduce((sum, value) => sum + value, 0),
  });

  g.start().to(split);
  g.from(split).to(left);
  g.from(split).to(right);
  g.merge([left, right]).to(merge).to(g.end());
  return g;
}

function makeBatchMergeWorkflow(size: number) {
  const g = workflow({ name: "batch-merge", input: z.number(), output: z.number() });
  const split = g.step("split", { input: z.number(), output: z.number(), run: ({ input }) => input });
  const workers = Array.from({ length: size }, (_, index) =>
    g.step(`worker-${String(index).padStart(3, "0")}`, {
      input: z.number(),
      output: z.number(),
      run: async ({ input }) => input + index,
    })
  );
  const merge = g.step("merge", {
    input: z.array(z.number()),
    output: z.number(),
    run: ({ input }) => input.reduce((sum, value) => sum + value, 0),
  });

  g.start().to(split);
  for (const worker of workers) {
    g.from(split).to(worker);
  }
  g.merge(workers).to(merge).to(g.end());
  return g;
}

function sourceSpec() {
  return {
    root: new URL(".", import.meta.url).pathname,
    include: ["fixtures/**/*.ts"],
  };
}

async function withTempDir(callback: (path: string) => Promise<void>): Promise<void> {
  const root = await Deno.makeTempDir({ prefix: "massive-sdk-" });
  try {
    await callback(root);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
}
